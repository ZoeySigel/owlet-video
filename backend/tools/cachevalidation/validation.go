package main

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	feed "github.com/ZoeySigel/owlet-video/backend/internal/application/feed"
	feedrepo "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/feed"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

type gateResult struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}
type memorySnapshot struct {
	UsedMemory   int64 `json:"used_memory"`
	DatasetBytes int64 `json:"dataset_bytes"`
	EvictedKeys  int64 `json:"evicted_keys"`
	Keys         int64 `json:"keys"`
}
type mechanismResult struct {
	Mode            string         `json:"mode"`
	Kind            string         `json:"kind"`
	Overlap         int            `json:"overlap"`
	Round           int            `json:"round"`
	ActualAffected  int            `json:"actual_affected_pages"`
	MaintenanceKeys []string       `json:"maintenance_keys"`
	Cold            Counters       `json:"cold"`
	Prewarm         Counters       `json:"prewarm"`
	Maintenance     Counters       `json:"maintenance"`
	Rebuild         Counters       `json:"rebuild"`
	MaintenanceNS   int64          `json:"maintenance_ns"`
	RebuildNS       int64          `json:"rebuild_ns"`
	Memory          memorySnapshot `json:"memory_after_warmup"`
	Correct         bool           `json:"correct"`
}

func requestFor(p pageSpec, size int, viewer int64) feed.FeedRequest {
	return feed.FeedRequest{Scene: "timeline", Cursor: p.Cursor, Limit: size, ViewerID: viewer}
}
func readMeasured(ctx context.Context, r Reader, req feed.FeedRequest) (*feed.FeedResult, Counters, error) {
	ctx, op := withOperation(ctx)
	result, err := r.Read(ctx, req)
	return result, op.Snapshot(), err
}
func containsID(r *feed.FeedResult, id int64) bool {
	if r == nil {
		return false
	}
	for _, item := range r.Items {
		if item.VideoID == id {
			return true
		}
	}
	return false
}

// Expectations are derived from the immutable fixture, independently of both
// cache implementations. The mutable target is checked against explicit values
// in qualification and version histories during concurrent load.
func verifyResult(result *feed.FeedResult, p pageSpec, viewer int64, users int, target int64, targetCount, targetTitleVersion *int64) error {
	if result == nil || result.Scene != "timeline" || len(result.Items) != len(p.IDs) {
		return fmt.Errorf("fixture oracle: wrong scene or item count (expected %d)", len(p.IDs))
	}
	for i, id := range p.IDs {
		item := result.Items[i]
		if item == nil || item.VideoID != id {
			return fmt.Errorf("fixture oracle: wrong video/order at %d, expected %d", i, id)
		}
		author := 1 + id%int64(users)
		if item.AuthorID != author || item.AuthorNickname != fmt.Sprintf("User %d", author) || item.AuthorAvatarURL != "/validation/avatar.png" || item.Description != strings.TrimSpace(strings.Repeat("Cache validation metadata. ", 8)) || item.MediaURL != fmt.Sprintf("/validation/%d.mp4", id) || item.CoverURL != fmt.Sprintf("/validation/%d.jpg", id) || !item.PublishedAt.Equal(publishedAt(id)) {
			return fmt.Errorf("fixture oracle: card mismatch for %d", id)
		}
		liked := false
		for j := int64(0); j < 3; j++ {
			liked = liked || viewer == 1+(id+j)%int64(users)
		}
		if item.Liked != liked || item.Favorited != (viewer == author) {
			return fmt.Errorf("fixture oracle: viewer state mismatch for %d", id)
		}
		if item.CommentCount != 0 || item.FavoriteCount != 1 {
			return fmt.Errorf("fixture oracle: static counts mismatch for %d", id)
		}
		if id != target {
			if item.Title != videoTitle(id, 0) || item.LikeCount != 3 {
				return fmt.Errorf("fixture oracle: unexpected mutation of %d", id)
			}
		} else {
			var parsedID, version int64
			if _, err := fmt.Sscanf(item.Title, "video-%d-v%d", &parsedID, &version); err != nil || parsedID != id || version < 0 || item.LikeCount < 3 {
				return fmt.Errorf("fixture oracle: malformed target data")
			}
			if targetCount != nil && int64(item.LikeCount) != *targetCount {
				return fmt.Errorf("fixture oracle: target count %d != %d", item.LikeCount, *targetCount)
			}
			if targetTitleVersion != nil && item.Title != videoTitle(id, *targetTitleVersion) {
				return fmt.Errorf("fixture oracle: stale target title")
			}
		}
	}
	wantCursor := ""
	wantMore := false
	if len(p.IDs) > 0 {
		last := p.IDs[len(p.IDs)-1]
		wantMore = last > 1
		wantCursor = cursorFor(last)
	}
	if result.NextCursor != wantCursor || result.HasMore != wantMore {
		return fmt.Errorf("fixture oracle: pagination mismatch")
	}
	return nil
}

func runCorrectness(ctx context.Context, f *fixture) ([]gateResult, error) {
	var gates []gateResult
	check := func(name string, err error) error {
		g := gateResult{Name: name, Passed: err == nil}
		if err != nil {
			g.Detail = err.Error()
		}
		gates = append(gates, g)
		return err
	}
	if err := f.reset(ctx); err != nil {
		return gates, err
	}
	whole, err := newReader("whole", feedrepo.New(f.db), f.rdb)
	if err != nil {
		return gates, err
	}
	split, err := newReader("split", feedrepo.New(f.db), f.rdb)
	if err != nil {
		return gates, err
	}
	c := makeCatalog(f.cfg.Videos, f.cfg.PageSize, 5)
	wantCount, wantTitle := int64(3), int64(0)
	compare := func(p pageSpec, viewer int64) error {
		a, _, e := readMeasured(ctx, whole, requestFor(p, f.cfg.PageSize, viewer))
		if e != nil {
			return e
		}
		if e = verifyResult(a, p, viewer, f.cfg.Users, c.Target, &wantCount, &wantTitle); e != nil {
			return e
		}
		b, _, e := readMeasured(ctx, split, requestFor(p, f.cfg.PageSize, viewer))
		if e != nil {
			return e
		}
		if e = verifyResult(b, p, viewer, f.cfg.Users, c.Target, &wantCount, &wantTitle); e != nil {
			return e
		}
		if !reflect.DeepEqual(a, b) {
			return fmt.Errorf("whole/split responses differ for viewer=%d cursor=%q", viewer, p.Cursor)
		}
		return nil
	}
	for _, name := range []string{"cold_response_equivalence", "warm_response_equivalence"} {
		if err = check(name, compare(c.Pages[0], 1)); err != nil {
			return gates, err
		}
	}
	if err = check("empty_page", compare(pageSpec{Cursor: cursorFor(1)}, 1)); err != nil {
		return gates, err
	}
	_, aStats, err := readMeasured(ctx, whole, requestFor(c.Pages[0], f.cfg.PageSize, 1))
	if err != nil {
		return gates, err
	}
	_, bStats, err := readMeasured(ctx, split, requestFor(c.Pages[0], f.cfg.PageSize, 1))
	if err != nil {
		return gates, err
	}
	if aStats.SQLQueries != 2 || bStats.SQLQueries != 2 {
		err = fmt.Errorf("warm reads must retain equal viewer+visibility SQL: whole=%d split=%d", aStats.SQLQueries, bStats.SQLQueries)
	}
	if err = check("equal_mandatory_database_reads", err); err != nil {
		return gates, err
	}
	owner := 1 + c.Target%int64(f.cfg.Users)
	if err = check("viewer_state_isolation", func() error {
		for _, r := range []Reader{whole, split} {
			liked, _, e := readMeasured(ctx, r, requestFor(c.Pages[0], 20, owner))
			if e != nil {
				return e
			}
			other := 1 + (owner+3)%int64(f.cfg.Users)
			unliked, _, e := readMeasured(ctx, r, requestFor(c.Pages[0], 20, other))
			if e != nil {
				return e
			}
			found := false
			for _, v := range liked.Items {
				if v.VideoID == c.Target {
					found = v.Liked
				}
			}
			otherLiked := false
			for _, v := range unliked.Items {
				if v.VideoID == c.Target {
					otherLiked = v.Liked
				}
			}
			if !found || otherLiked {
				return fmt.Errorf("viewer state leaked or was not loaded")
			}
		}
		return nil
	}()); err != nil {
		return gates, err
	}
	for _, kind := range []string{"count", "card"} {
		if kind == "count" {
			err = f.db.WithContext(ctx).Exec("UPDATE video_stat SET like_count=like_count+1 WHERE video_id=?", c.Target).Error
		} else {
			err = f.db.WithContext(ctx).Exec("UPDATE video SET title=? WHERE id=?", videoTitle(c.Target, 1), c.Target).Error
		}
		if err != nil {
			return gates, err
		}
		if kind == "count" {
			wantCount = 4
		} else {
			wantTitle = 1
		}
		for _, r := range []Reader{whole, split} {
			if _, err = r.Invalidate(ctx, c.Target, kind); err != nil {
				return gates, err
			}
		}
		err = func() error {
			for _, pi := range c.Affected {
				if e := compare(c.Pages[pi], owner); e != nil {
					return e
				}
				for _, r := range []Reader{whole, split} {
					result, _, e := readMeasured(ctx, r, requestFor(c.Pages[pi], 20, owner))
					if e != nil {
						return e
					}
					for _, v := range result.Items {
						if v.VideoID == c.Target {
							if kind == "count" && v.LikeCount != 4 {
								return fmt.Errorf("stale count %d", v.LikeCount)
							}
							if kind == "card" && v.Title != videoTitle(c.Target, 1) {
								return fmt.Errorf("stale title %q", v.Title)
							}
						}
					}
				}
			}
			return nil
		}()
		if err = check(kind+"_update_visible_on_next_read", err); err != nil {
			return gates, err
		}
	}
	if err = f.rdb.Del(ctx, fmt.Sprintf("video:card:v1:%d", c.Target), fmt.Sprintf("video:stat:v1:%d", c.Target)).Err(); err != nil {
		return gates, err
	}
	if err = check("partial_cache_miss", compare(c.Pages[0], owner)); err != nil {
		return gates, err
	}
	// Force actual entries to expire without making correctness qualification wait 15 minutes.
	keys := []string{wholePageKey(requestFor(c.Pages[0], 20, 1)), fmt.Sprintf("video:card:v1:%d", c.Target), fmt.Sprintf("video:stat:v1:%d", c.Target)}
	for _, key := range keys {
		if err = f.rdb.PExpire(ctx, key, time.Millisecond).Err(); err != nil {
			return gates, err
		}
	}
	time.Sleep(5 * time.Millisecond)
	if err = check("expired_entries_reload", compare(c.Pages[0], owner)); err != nil {
		return gates, err
	}
	if err = f.db.WithContext(ctx).Exec("UPDATE video SET status=3 WHERE id=?", c.Target).Error; err != nil {
		return gates, err
	}
	err = func() error {
		for _, r := range []Reader{whole, split} {
			res, _, e := readMeasured(ctx, r, requestFor(c.Pages[0], 20, owner))
			if e != nil {
				return e
			}
			if containsID(res, c.Target) {
				return fmt.Errorf("offline video returned from warm cache")
			}
		}
		return nil
	}()
	if err = check("warm_cache_publication_visibility", err); err != nil {
		return gates, err
	}
	if err = f.db.WithContext(ctx).Exec("UPDATE video SET status=2 WHERE id=?", c.Target).Error; err != nil {
		return gates, err
	}
	bad := requestFor(c.Pages[0], 20, 1)
	bad.Cursor = "malformed"
	err = func() error {
		for _, r := range []Reader{whole, split} {
			if _, e := r.Read(ctx, bad); e == nil {
				return fmt.Errorf("malformed cursor accepted")
			}
		}
		return nil
	}()
	if err = check("invalid_cursor_rejected", err); err != nil {
		return gates, err
	}
	// Commands fail only in this context; the server is not stopped and unrelated clients are unaffected.
	f.rdb.AddHook(redisFaultHook{})
	faultCtx := context.WithValue(ctx, redisFaultKey{}, true)
	err = func() error {
		for _, r := range []Reader{whole, split} {
			res, stats, e := readMeasured(faultCtx, r, requestFor(c.Pages[0], 20, owner))
			if e != nil {
				return e
			}
			if len(res.Items) != 20 || stats.RedisErrors == 0 {
				return fmt.Errorf("cache failure did not exercise SQL fallback")
			}
		}
		return nil
	}()
	if err = check("cache_read_failure_falls_back", err); err != nil {
		return gates, err
	}
	return gates, nil
}

func runMechanisms(ctx context.Context, f *fixture) ([]mechanismResult, error) {
	var out []mechanismResult
	for round := 1; round <= f.cfg.Rounds; round++ {
		for _, k := range []int{1, 5, 20} {
			for _, kind := range []string{"count", "card"} {
				modes := []string{"whole", "split"}
				if round%2 == 0 {
					modes = []string{"split", "whole"}
				}
				for _, mode := range modes {
					if err := f.reset(ctx); err != nil {
						return out, err
					}
					r, err := newReader(mode, feedrepo.New(f.db), f.rdb)
					if err != nil {
						return out, err
					}
					c := makeCatalog(f.cfg.Videos, 20, k)
					m := mechanismResult{Mode: mode, Kind: kind, Overlap: k, Round: round}
					_, m.Cold, err = readMeasured(ctx, r, requestFor(c.Pages[0], 20, 1))
					if err != nil {
						return out, err
					}
					for _, pi := range c.Affected {
						result, stats, e := readMeasured(ctx, r, requestFor(c.Pages[pi], 20, 1))
						if e != nil {
							return out, e
						}
						m.Prewarm = m.Prewarm.Add(stats)
						if containsID(result, c.Target) {
							m.ActualAffected++
						}
					}
					if m.ActualAffected != k {
						return out, fmt.Errorf("expected overlap %d, got %d", k, m.ActualAffected)
					}
					m.Memory = readMemory(ctx, f.rdb)
					if m.Memory.EvictedKeys != f.startEvicted {
						return out, fmt.Errorf("eviction invalidates reverse-index comparison")
					}
					mc, op := withOperation(ctx)
					start := time.Now()
					if kind == "count" {
						err = f.db.WithContext(mc).Transaction(func(tx *gorm.DB) error {
							return tx.Exec("UPDATE video_stat SET like_count=like_count+1 WHERE video_id=?", c.Target).Error
						})
					} else {
						err = f.db.WithContext(mc).Transaction(func(tx *gorm.DB) error {
							return tx.Exec("UPDATE video SET title=? WHERE id=?", videoTitle(c.Target, 1), c.Target).Error
						})
					}
					if err == nil {
						m.MaintenanceKeys, err = r.Invalidate(mc, c.Target, kind)
					}
					m.MaintenanceNS = time.Since(start).Nanoseconds()
					m.Maintenance = op.Snapshot()
					if err != nil {
						return out, err
					}
					expectedDeletes := int64(1)
					if mode == "whole" {
						expectedDeletes = int64(k)
					}
					if m.Maintenance.KeysDeleted != expectedDeletes {
						return out, fmt.Errorf("live fanout invalid for %s/%s/K%d: deleted %d, expected %d; expiration or failed cache fill interfered", mode, kind, k, m.Maintenance.KeysDeleted, expectedDeletes)
					}
					start = time.Now()
					m.Correct = true
					for _, pi := range c.Affected {
						result, stats, e := readMeasured(ctx, r, requestFor(c.Pages[pi], 20, 1))
						m.Rebuild = m.Rebuild.Add(stats)
						if e != nil {
							return out, e
						}
						seen := false
						for _, v := range result.Items {
							if v.VideoID == c.Target {
								seen = true
								if kind == "count" && v.LikeCount != 4 {
									m.Correct = false
								}
								if kind == "card" && v.Title != videoTitle(c.Target, 1) {
									m.Correct = false
								}
							}
						}
						m.Correct = m.Correct && seen
					}
					m.RebuildNS = time.Since(start).Nanoseconds()
					out = append(out, m)
					if !m.Correct {
						return out, fmt.Errorf("mechanism correctness failed: %s/%s/K%d", mode, kind, k)
					}
				}
			}
		}
	}
	return out, nil
}

type redisFaultKey struct{}
type redisFaultHook struct{}

func (redisFaultHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (redisFaultHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if ctx.Value(redisFaultKey{}) == true {
			e := fmt.Errorf("validation injected Redis read failure")
			cmd.SetErr(e)
			return e
		}
		return next(ctx, cmd)
	}
}
func (redisFaultHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if ctx.Value(redisFaultKey{}) == true {
			e := fmt.Errorf("validation injected Redis pipeline failure")
			for _, cmd := range cmds {
				cmd.SetErr(e)
			}
			return e
		}
		return next(ctx, cmds)
	}
}

type changeRecord struct {
	Version int64
	At      time.Time
}
type oracle struct {
	mu          sync.RWMutex
	count, card []changeRecord
	target      int64
}

func (o *oracle) commit(kind string, v int64, at time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r := changeRecord{v, at}
	if kind == "count" {
		o.count = append(o.count, r)
	} else {
		o.card = append(o.card, r)
	}
}
func (o *oracle) inspect(result *feed.FeedResult, start, end time.Time) (bool, time.Duration, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	stale := false
	var maxAge time.Duration
	violation := false
	for _, item := range result.Items {
		if item.VideoID != o.target {
			continue
		}
		var id, titleVersion int64
		if _, err := fmt.Sscanf(item.Title, "video-%d-v%d", &id, &titleVersion); err != nil {
			violation = true
		}
		countMax, cardMax := int64(0), int64(0)
		if len(o.count) > 0 {
			countMax = o.count[len(o.count)-1].Version
		}
		if len(o.card) > 0 {
			cardMax = o.card[len(o.card)-1].Version
		}
		if id != o.target || int64(item.LikeCount-3) > countMax || titleVersion > cardMax {
			violation = true
		}
		for _, entry := range []struct {
			history []changeRecord
			actual  int64
			bound   time.Duration
		}{{o.count, int64(item.LikeCount - 3), 15 * time.Second}, {o.card, titleVersion, 15 * time.Minute}} {
			for _, change := range entry.history {
				if change.At.After(start) {
					break
				}
				if change.Version > entry.actual {
					age := end.Sub(change.At)
					if age > maxAge {
						maxAge = age
					}
					stale = true
					if age > entry.bound {
						violation = true
					}
					break
				}
			}
		}
	}
	return stale, maxAge, violation
}
