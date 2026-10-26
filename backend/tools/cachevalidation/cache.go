package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"sync"
	"time"

	applicationfeed "github.com/ZoeySigel/owlet-video/backend/internal/application/feed"
	domainfeed "github.com/ZoeySigel/owlet-video/backend/internal/domain/feed"
	infracache "github.com/ZoeySigel/owlet-video/backend/internal/infra/cache"
	infrafeed "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/feed"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

const wholePagePrefix = "cachevalidation:whole:page:"
const wholeIndexPrefix = "cachevalidation:whole:index:"

type Reader interface {
	Read(context.Context, applicationfeed.FeedRequest) (*applicationfeed.FeedResult, error)
	Invalidate(context.Context, int64, string) ([]string, error)
}

// The embedded interface intentionally hides the optional PublishedVideoIDs
// method. Both variants run exactly the same visibility and viewer-state reads
// in commonReader, once per request, outside their shared caches.
type sharedFeedRepository struct{ domainfeed.Repository }

type commonReader struct {
	repo       *infrafeed.Repository
	read       func(context.Context, applicationfeed.FeedRequest) (*applicationfeed.FeedResult, error)
	invalidate func(context.Context, int64, string) ([]string, error)
}

func newReader(mode string, repo *infrafeed.Repository, rdb *redis.Client) (Reader, error) {
	if repo == nil || rdb == nil {
		return nil, fmt.Errorf("reader requires repository and Redis")
	}
	hidden := sharedFeedRepository{Repository: repo}
	var read func(context.Context, applicationfeed.FeedRequest) (*applicationfeed.FeedResult, error)
	var invalidate func(context.Context, int64, string) ([]string, error)
	switch mode {
	case "split":
		cache := &observedFeedCache{FeedCache: infracache.NewFeedCache(rdb)}
		service := applicationfeed.New(hidden, applicationfeed.WithFeedCache(cache))
		read = service.GetFeed
		invalidate = func(ctx context.Context, videoID int64, kind string) ([]string, error) {
			key, err := splitInvalidationKey(videoID, kind)
			if err != nil {
				return nil, err
			}
			n, err := rdb.Del(ctx, key).Result()
			if n > 0 {
				return []string{key}, err
			}
			return nil, err
		}
	case "whole":
		service := applicationfeed.New(hidden)
		whole := &wholeReader{rdb: rdb, load: service.GetFeed}
		read, invalidate = whole.read, whole.invalidate
	default:
		return nil, fmt.Errorf("unknown reader mode %q", mode)
	}
	return &commonReader{repo: repo, read: read, invalidate: invalidate}, nil
}

func (reader *commonReader) Read(ctx context.Context, req applicationfeed.FeedRequest) (*applicationfeed.FeedResult, error) {
	if domainfeed.NormalizeScene(req.Scene) != domainfeed.SceneTimeline {
		return nil, fmt.Errorf("cache validation supports timeline only")
	}
	viewerID := req.ViewerID
	req.ViewerID = 0
	result, err := reader.read(ctx, req)
	if err != nil || result == nil {
		return result, err
	}
	result = cloneFeedResult(result)
	ids := resultVideoIDs(result)
	if len(ids) == 0 {
		return result, nil
	}
	actions, err := reader.repo.BatchGetViewerActionStates(ctx, viewerID, ids)
	if err != nil {
		return nil, err
	}
	visible, err := reader.repo.PublishedVideoIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	items := result.Items[:0]
	for _, item := range result.Items {
		if !visible[item.VideoID] {
			continue
		}
		if state := actions[item.VideoID]; state != nil {
			item.Liked, item.Favorited = state.Liked, state.Favorited
		}
		items = append(items, item)
	}
	result.Items = items
	return result, nil
}

func (reader *commonReader) Invalidate(ctx context.Context, videoID int64, kind string) ([]string, error) {
	if _, err := splitInvalidationKey(videoID, kind); err != nil {
		return nil, err
	}
	return reader.invalidate(ctx, videoID, kind)
}

func splitInvalidationKey(videoID int64, kind string) (string, error) {
	if videoID <= 0 {
		return "", fmt.Errorf("invalid video ID")
	}
	switch kind {
	case "count":
		return fmt.Sprintf("video:stat:v1:%d", videoID), nil
	case "card":
		return fmt.Sprintf("video:card:v1:%d", videoID), nil
	default:
		return "", fmt.Errorf("unknown invalidation kind %q", kind)
	}
}

type observedFeedCache struct{ *infracache.FeedCache }

func (cache *observedFeedCache) GetPage(ctx context.Context, key string) (*applicationfeed.FeedPage, bool, error) {
	page, hit, err := cache.FeedCache.GetPage(ctx, key)
	if hit && err == nil {
		observeCache(ctx, "page", 1, 0)
	} else {
		observeCache(ctx, "page", 0, 1)
	}
	return page, hit, err
}
func (cache *observedFeedCache) GetCards(ctx context.Context, ids []int64) (map[int64]*domainfeed.FeedCard, error) {
	values, err := cache.FeedCache.GetCards(ctx, ids)
	hits := 0
	if err == nil {
		hits = len(values)
	}
	observeCache(ctx, "card", hits, len(ids)-hits)
	return values, err
}
func (cache *observedFeedCache) GetStats(ctx context.Context, ids []int64) (map[int64]*domainfeed.FeedStat, error) {
	values, err := cache.FeedCache.GetStats(ctx, ids)
	hits := 0
	if err == nil {
		hits = len(values)
	}
	observeCache(ctx, "stat", hits, len(ids)-hits)
	return values, err
}

type wholeReader struct {
	rdb         *redis.Client
	load        func(context.Context, applicationfeed.FeedRequest) (*applicationfeed.FeedResult, error)
	group       singleflight.Group
	unsafePages sync.Map // In-flight/failed fills are never trusted without all reverse dependencies.
}

func (reader *wholeReader) read(ctx context.Context, req applicationfeed.FeedRequest) (*applicationfeed.FeedResult, error) {
	key := wholePageKey(req)
	if result, hit := reader.cached(ctx, key); hit {
		return result, nil
	}
	value, err, _ := reader.group.Do(key, func() (interface{}, error) {
		if result, hit := reader.cached(ctx, key); hit {
			return result, nil
		}
		result, err := reader.load(ctx, req)
		if err != nil {
			return nil, err
		}
		// Like production's split caches, failed cache writes are best effort and
		// do not turn a successful database read into a failed request.
		_ = reader.store(ctx, key, result, wholePageTTL(req))
		return result, nil
	})
	if err != nil {
		return nil, err
	}
	return cloneFeedResult(value.(*applicationfeed.FeedResult)), nil
}

func (reader *wholeReader) cached(ctx context.Context, key string) (*applicationfeed.FeedResult, bool) {
	if _, unsafe := reader.unsafePages.Load(key); unsafe {
		observeCache(ctx, "whole", 0, 1)
		return nil, false
	}
	content, err := reader.rdb.Get(ctx, key).Bytes()
	var result applicationfeed.FeedResult
	if err != nil || json.Unmarshal(content, &result) != nil {
		observeCache(ctx, "whole", 0, 1)
		return nil, false
	}
	observeCache(ctx, "whole", 1, 0)
	return &result, true
}

func (reader *wholeReader) store(ctx context.Context, key string, result *applicationfeed.FeedResult, ttl time.Duration) error {
	content, err := json.Marshal(result)
	if err != nil {
		return err
	}
	ids := resultVideoIDs(result)
	// Index timestamps guide lazy cleanup only. Cache key existence is checked
	// before dropping members, avoiding client/server clock skew races.
	if err := reader.pruneIndexes(ctx, ids); err != nil {
		return err
	}
	// Validation uses one Redis instance: these transactions span keys and are
	// not Cluster-safe. Different pages may fill/invalidate concurrently, like
	// the production split caches. Cache-aside refill races are evaluated under
	// the shared freshness contract rather than hidden by a global Go lock.
	// Redis transactions do not roll back individual command failures. Refuse
	// concurrent hits until all page and reverse-index writes are acknowledged.
	reader.unsafePages.Store(key, true)
	_, err = reader.rdb.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.Set(ctx, key, content, ttl)
		indexTTL := 15 * time.Second
		if ttl > indexTTL {
			indexTTL = ttl
		}
		for _, id := range ids {
			pipe.ZAdd(ctx, wholeIndexKey(id), redis.Z{Score: float64(time.Now().Add(ttl).UnixMilli()), Member: key})
			// Every supported page lives at most 15 seconds. Extending its index
			// by this bound preserves other live members and also expires indexes
			// for videos that are never accessed again.
			pipe.PExpire(ctx, wholeIndexKey(id), indexTTL)
		}
		return nil
	})
	if err != nil {
		// A failed ZADD/PEXPIRE must not leave a readable but unindexed page.
		// Keep the local refusal marker even if Redis is unreachable during
		// cleanup. The fixture is reset before a new reader/round is created.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		_ = reader.rdb.Del(cleanupCtx, key).Err()
		return err
	}
	reader.unsafePages.Delete(key)
	return err
}

func (reader *wholeReader) pruneIndexes(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	// Cleanup is three batched phases, not one network round-trip per video:
	// discover possibly expired members, test unique page keys with one MGET,
	// then pipeline removals. Hooks still account for every actual command.
	queries := make([]*redis.StringSliceCmd, len(ids))
	cutoff := fmt.Sprint(time.Now().UnixMilli())
	_, err := reader.rdb.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for i, id := range ids {
			queries[i] = pipe.ZRangeByScore(ctx, wholeIndexKey(id), &redis.ZRangeBy{Min: "-inf", Max: cutoff})
		}
		return nil
	})
	if err != nil {
		return err
	}
	pages := make([]string, 0)
	seen := map[string]bool{}
	for _, query := range queries {
		for _, page := range query.Val() {
			if !seen[page] {
				pages = append(pages, page)
				seen[page] = true
			}
		}
	}
	if len(pages) == 0 {
		return nil
	}
	values, err := reader.rdb.MGet(ctx, pages...).Result()
	if err != nil {
		return err
	}
	absent := map[string]bool{}
	for i, value := range values {
		if value == nil {
			absent[pages[i]] = true
		}
	}
	if len(absent) == 0 {
		return nil
	}
	_, err = reader.rdb.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for i, query := range queries {
			stale := make([]interface{}, 0)
			for _, page := range query.Val() {
				if absent[page] {
					stale = append(stale, page)
				}
			}
			if len(stale) > 0 {
				pipe.ZRem(ctx, wholeIndexKey(ids[i]), stale...)
			}
		}
		return nil
	})
	return err
}

func (reader *wholeReader) invalidate(ctx context.Context, videoID int64, kind string) ([]string, error) {
	index := wholeIndexKey(videoID)
	pages, err := reader.rdb.ZRange(ctx, index, 0, -1).Result()
	if err != nil || len(pages) == 0 {
		return nil, err
	}
	values, err := reader.rdb.MGet(ctx, pages...).Result()
	if err != nil {
		return nil, err
	}
	remove := map[string][]interface{}{}
	deletions := make([]string, 0)
	for i, value := range values {
		remove[index] = append(remove[index], pages[i])
		if value == nil {
			continue
		}
		var result applicationfeed.FeedResult
		if json.Unmarshal(redisValueBytes(value), &result) != nil {
			deletions = append(deletions, pages[i])
			continue
		}
		ids := resultVideoIDs(&result)
		contains := false
		for _, id := range ids {
			if id == videoID {
				contains = true
				break
			}
		}
		if !contains {
			continue
		} // A re-used page key no longer depends on this video.
		deletions = append(deletions, pages[i])
		for _, id := range ids {
			if id != videoID {
				key := wholeIndexKey(id)
				remove[key] = append(remove[key], pages[i])
			}
		}
	}
	keys := make([]string, 0, len(remove)+len(deletions))
	deleted := map[string]*redis.IntCmd{}
	_, err = reader.rdb.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		for _, page := range deletions {
			deleted[page] = pipe.Del(ctx, page)
		}
		for key, members := range remove {
			pipe.ZRem(ctx, key, members...)
			keys = append(keys, key)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for page, command := range deleted {
		if command.Val() > 0 {
			keys = append(keys, page)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

func resultVideoIDs(result *applicationfeed.FeedResult) []int64 {
	ids := make([]int64, 0, len(result.Items))
	seen := map[int64]bool{}
	for _, item := range result.Items {
		if item != nil && !seen[item.VideoID] {
			ids = append(ids, item.VideoID)
			seen[item.VideoID] = true
		}
	}
	return ids
}

func cloneFeedResult(result *applicationfeed.FeedResult) *applicationfeed.FeedResult {
	copy := *result
	copy.Items = make([]*domainfeed.FeedItem, 0, len(result.Items))
	for _, item := range result.Items {
		if item != nil {
			value := *item
			copy.Items = append(copy.Items, &value)
		}
	}
	return &copy
}

func redisValueBytes(value interface{}) []byte {
	if raw, ok := value.([]byte); ok {
		return raw
	}
	return []byte(fmt.Sprint(value))
}
func wholeIndexKey(videoID int64) string { return fmt.Sprintf("%s%d", wholeIndexPrefix, videoID) }
func pageContractKey(req applicationfeed.FeedRequest) string {
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > domainfeed.MaxLimit {
		limit = domainfeed.MaxLimit
	}
	scene := domainfeed.NormalizeScene(req.Scene)
	cursor := strings.TrimSpace(req.Cursor)
	if cursor == "" {
		return fmt.Sprintf("feed:page:v1:%s:limit:%d:first", scene, limit)
	}
	sum := sha1.Sum([]byte(cursor))
	return fmt.Sprintf("feed:page:v1:%s:limit:%d:cursor:%s", scene, limit, hex.EncodeToString(sum[:]))
}
func wholePageKey(req applicationfeed.FeedRequest) string {
	return wholePagePrefix + pageContractKey(req)
}
func wholePageTTL(req applicationfeed.FeedRequest) time.Duration {
	base := 45 * time.Second
	if strings.TrimSpace(req.Cursor) == "" {
		base = 5 * time.Second
	}
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(pageContractKey(req)))
	ttl := base + time.Duration(10+hasher.Sum32()%11)*base/100
	if ttl > 15*time.Second {
		return 15 * time.Second
	}
	return ttl
}
