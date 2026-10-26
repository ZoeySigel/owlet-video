package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	applicationfeed "github.com/ZoeySigel/owlet-video/backend/internal/application/feed"
	domainfeed "github.com/ZoeySigel/owlet-video/backend/internal/domain/feed"
	"github.com/redis/go-redis/v9"
)

func TestWholeTTLMatchesJitteredContractAndShortestFreshness(t *testing.T) {
	req := applicationfeed.FeedRequest{Scene: domainfeed.SceneTimeline, Limit: 10}
	if got := wholePageTTL(req); got < 5500*time.Millisecond || got > 6*time.Second {
		t.Fatalf("first-page TTL=%s", got)
	}
	defaultReq := applicationfeed.FeedRequest{}
	if wholePageKey(defaultReq) != wholePageKey(req) {
		t.Fatal("normalised requests must share a page key")
	}
	req.Cursor = "next"
	if got := wholePageTTL(req); got != 15*time.Second {
		t.Fatalf("later page must be capped to counter freshness: %s", got)
	}
	if _, err := splitInvalidationKey(3, "other"); err == nil {
		t.Fatal("invalid kind accepted")
	}
}

func TestWholeInvalidationRemovesOnlyDependentPagesAndAllTheirIndexMembers(t *testing.T) {
	rdb, fake := newFakeRedis(t)
	reader := &wholeReader{rdb: rdb}
	ctx := context.Background()
	if err := reader.store(ctx, "page:a", testFeed(1, 2), time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := reader.store(ctx, "page:b", testFeed(2, 3), time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := reader.store(ctx, "page:c", testFeed(3, 4), time.Minute); err != nil {
		t.Fatal(err)
	}
	ctx, operation := withOperation(ctx)
	keys, err := reader.invalidate(ctx, 2, "count")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 5 {
		t.Fatalf("expected 2 pages and 3 unique indexes, got %v", keys)
	}
	if rdb.Exists(ctx, "page:c").Val() != 1 || rdb.Exists(ctx, "page:a", "page:b").Val() != 0 {
		t.Fatal("invalidation removed unrelated page or retained dependent page")
	}
	if got := rdb.ZRange(ctx, wholeIndexKey(3), 0, -1).Val(); len(got) != 1 || got[0] != "page:c" {
		t.Fatalf("cross-index cleanup failed: %v", got)
	}
	if got := operation.Snapshot(); got.KeysDeleted != 2 || got.IndexWrites != 3 {
		t.Fatalf("unexpected invalidation accounting: %+v", got)
	}
	for _, command := range fake.commands {
		if command == "scan" || command == "keys" {
			t.Fatal("invalidation must not scan keyspace")
		}
	}
}

func TestWholeIndexLazilyPrunesExpiredPages(t *testing.T) {
	rdb, fake := newFakeRedis(t)
	reader := &wholeReader{rdb: rdb}
	ctx := context.Background()
	if err := reader.store(ctx, "expired", testFeed(1), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.expiry["expired"] = time.Now().Add(-time.Second)
	fake.indexes[wholeIndexKey(1)]["expired"] = float64(time.Now().Add(-time.Second).UnixMilli())
	fake.mu.Unlock()
	if err := reader.store(ctx, "live", testFeed(1), time.Minute); err != nil {
		t.Fatal(err)
	}
	got := rdb.ZRange(ctx, wholeIndexKey(1), 0, -1).Val()
	if len(got) != 1 || got[0] != "live" {
		t.Fatalf("expired dependency retained: %v", got)
	}
}

func TestWholeIndexCleanupDiscoveryIsBatchedAcrossVideos(t *testing.T) {
	rdb, fake := newFakeRedis(t)
	reader := &wholeReader{rdb: rdb}
	ids := make([]int64, 20)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	if err := reader.store(context.Background(), "many-videos", testFeed(ids...), time.Second); err != nil {
		t.Fatal(err)
	}
	if len(fake.pipelineBatches) < 2 {
		t.Fatalf("expected batched cleanup and transactional store: %v", fake.pipelineBatches)
	}
	first := fake.pipelineBatches[0]
	if len(first) != 20 {
		t.Fatalf("cleanup discovery made %d commands in first pipeline, want 20", len(first))
	}
	for _, command := range first {
		if command != "zrangebyscore" {
			t.Fatalf("unexpected discovery command %q", command)
		}
	}
}

func TestWholeFailedIndexWriteDoesNotLeaveReadableOrphanPage(t *testing.T) {
	rdb, fake := newFakeRedis(t)
	reader := &wholeReader{rdb: rdb}
	fake.failZAdd = true
	ctx, op := withOperation(context.Background())
	if err := reader.store(ctx, "orphan", testFeed(1), time.Minute); err == nil {
		t.Fatal("injected reverse-index failure was ignored")
	}
	if rdb.Exists(ctx, "orphan").Val() != 0 {
		t.Fatal("partially committed page was not removed")
	}
	if _, hit := reader.cached(ctx, "orphan"); hit {
		t.Fatal("failed fill was trusted")
	}
	if got := op.Snapshot(); got.KeysDeleted != 1 || got.RedisErrors != 1 {
		t.Fatalf("cleanup accounting lost successful SET/DEL or index error: %+v", got)
	}
	if err := reader.store(ctx, "orphan", testFeed(1), time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, hit := reader.cached(ctx, "orphan"); !hit {
		t.Fatal("repaired page remained untrusted")
	}
}

func TestWholeUnrelatedPagesDoNotSerializeBehindSlowFill(t *testing.T) {
	rdb, fake := newFakeRedis(t)
	fake.blockSetKey = "page:slow"
	fake.fillEntered = make(chan struct{})
	fake.fillRelease = make(chan struct{})
	reader := &wholeReader{rdb: rdb}
	slowDone := make(chan error, 1)
	go func() { slowDone <- reader.store(context.Background(), "page:slow", testFeed(1), time.Second) }()
	<-fake.fillEntered
	// Always unblock the fake, including on an assertion failure.
	defer func() {
		close(fake.fillRelease)
		if err := <-slowDone; err != nil {
			t.Errorf("slow fill failed: %v", err)
		}
	}()
	independentDone := make(chan error, 1)
	go func() {
		if err := reader.store(context.Background(), "page:independent", testFeed(2), time.Second); err != nil {
			independentDone <- err
			return
		}
		_, err := reader.invalidate(context.Background(), 2, "card")
		independentDone <- err
	}()
	select {
	case err := <-independentDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("unrelated page fill/invalidation was serialized behind slow fill")
	}
}

func TestWholeRedisFailureFallsBackAndReturnedResultsAreIsolated(t *testing.T) {
	rdb, fake := newFakeRedis(t)
	fake.offline = true
	var loads atomic.Int64
	source := testFeed(9)
	reader := &wholeReader{rdb: rdb, load: func(context.Context, applicationfeed.FeedRequest) (*applicationfeed.FeedResult, error) {
		loads.Add(1)
		return source, nil
	}}
	got, err := reader.read(context.Background(), applicationfeed.FeedRequest{})
	if err != nil || len(got.Items) != 1 || loads.Load() != 1 {
		t.Fatalf("cache outage failed database read: result=%v, error=%v", got, err)
	}
	got.Items[0].Liked = true
	if source.Items[0].Liked {
		t.Fatal("caller modified shared loader result")
	}
}

func TestWholeReadSingleflightAndUserIndependentCache(t *testing.T) {
	rdb, _ := newFakeRedis(t)
	var loads atomic.Int64
	reader := &wholeReader{rdb: rdb, load: func(context.Context, applicationfeed.FeedRequest) (*applicationfeed.FeedResult, error) {
		loads.Add(1)
		time.Sleep(5 * time.Millisecond)
		return testFeed(4), nil
	}}
	var wait sync.WaitGroup
	for i := 0; i < 8; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := reader.read(context.Background(), applicationfeed.FeedRequest{})
			if err != nil || len(result.Items) != 1 {
				t.Errorf("read failed: %v", err)
			}
		}()
	}
	wait.Wait()
	if loads.Load() != 1 {
		t.Fatalf("singleflight loaded %d times", loads.Load())
	}
	first, _ := reader.read(context.Background(), applicationfeed.FeedRequest{})
	first.Items[0].Liked = true
	second, _ := reader.read(context.Background(), applicationfeed.FeedRequest{})
	if second.Items[0].Liked {
		t.Fatal("viewer state leaked into shared page")
	}
}

func testFeed(ids ...int64) *applicationfeed.FeedResult {
	result := &applicationfeed.FeedResult{Scene: domainfeed.SceneTimeline, NextCursor: "next", HasMore: true}
	for _, id := range ids {
		result.Items = append(result.Items, &domainfeed.FeedItem{VideoID: id, Title: fmt.Sprintf("video %d", id)})
	}
	return result
}

// This hook is a deterministic Redis command fake, not a performance model. It
// permits cache/index lifecycle tests without an external Redis dependency.
type fakeRedis struct {
	mu              sync.Mutex
	values          map[string]string
	expiry          map[string]time.Time
	indexes         map[string]map[string]float64
	commands        []string
	offline         bool
	failZAdd        bool
	blockSetKey     string
	fillEntered     chan struct{}
	fillRelease     chan struct{}
	pipelineBatches [][]string
}

func newFakeRedis(t *testing.T) (*redis.Client, *fakeRedis) {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: "unused.invalid:1", MaxRetries: -1})
	fake := &fakeRedis{values: map[string]string{}, expiry: map[string]time.Time{}, indexes: map[string]map[string]float64{}}
	installRedisMeter(client)
	client.AddHook(fake)
	t.Cleanup(func() { _ = client.Close() })
	return client, fake
}
func (fake *fakeRedis) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		return next(ctx, network, address)
	}
}
func (fake *fakeRedis) ProcessHook(redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error { fake.execute(cmd); return cmd.Err() }
}
func (fake *fakeRedis) ProcessPipelineHook(redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, commands []redis.Cmder) error {
		batch := make([]string, len(commands))
		for i, cmd := range commands {
			batch[i] = cmd.Name()
		}
		fake.mu.Lock()
		fake.pipelineBatches = append(fake.pipelineBatches, batch)
		fake.mu.Unlock()
		for _, cmd := range commands {
			if fake.blockSetKey != "" && cmd.Name() == "set" && fmt.Sprint(cmd.Args()[1]) == fake.blockSetKey {
				close(fake.fillEntered)
				<-fake.fillRelease
				break
			}
		}
		var first error
		for _, cmd := range commands {
			fake.execute(cmd)
			if first == nil {
				first = cmd.Err()
			}
		}
		return first
	}
}
func (fake *fakeRedis) execute(cmd redis.Cmder) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.commands = append(fake.commands, cmd.Name())
	if fake.offline {
		cmd.SetErr(errors.New("Redis offline"))
		return
	}
	if fake.failZAdd && cmd.Name() == "zadd" {
		fake.failZAdd = false
		cmd.SetErr(errors.New("injected index write failure"))
		return
	}
	args := cmd.Args()
	text := func(i int) string {
		if bytes, ok := args[i].([]byte); ok {
			return string(bytes)
		}
		return fmt.Sprint(args[i])
	}
	get := func(key string) (string, bool) {
		if expiry, ok := fake.expiry[key]; ok && !time.Now().Before(expiry) {
			delete(fake.values, key)
			delete(fake.expiry, key)
		}
		value, ok := fake.values[key]
		return value, ok
	}
	switch cmd.Name() {
	case "multi", "exec":
	case "get":
		value, ok := get(text(1))
		if !ok {
			cmd.SetErr(redis.Nil)
		} else {
			cmd.(*redis.StringCmd).SetVal(value)
		}
	case "mget":
		values := make([]interface{}, len(args)-1)
		for i := 1; i < len(args); i++ {
			if value, ok := get(text(i)); ok {
				values[i-1] = value
			}
		}
		cmd.(*redis.SliceCmd).SetVal(values)
	case "set":
		fake.values[text(1)] = text(2)
		if len(args) >= 5 {
			n, _ := strconv.ParseInt(text(4), 10, 64)
			unit := time.Millisecond
			if strings.ToLower(text(3)) == "ex" {
				unit = time.Second
			}
			fake.expiry[text(1)] = time.Now().Add(time.Duration(n) * unit)
		}
		cmd.(*redis.StatusCmd).SetVal("OK")
	case "zadd":
		key := text(1)
		if fake.indexes[key] == nil {
			fake.indexes[key] = map[string]float64{}
		}
		score, _ := strconv.ParseFloat(text(2), 64)
		var added int64
		if _, ok := fake.indexes[key][text(3)]; !ok {
			added = 1
		}
		fake.indexes[key][text(3)] = score
		cmd.(*redis.IntCmd).SetVal(added)
	case "zrange", "zrangebyscore":
		members := make([]string, 0)
		min, max := math.Inf(-1), math.Inf(1)
		if cmd.Name() == "zrangebyscore" {
			min, _ = strconv.ParseFloat(text(2), 64)
			max, _ = strconv.ParseFloat(text(3), 64)
		}
		for member, score := range fake.indexes[text(1)] {
			if score >= min && score <= max {
				members = append(members, member)
			}
		}
		sort.Slice(members, func(i, j int) bool {
			left, right := fake.indexes[text(1)][members[i]], fake.indexes[text(1)][members[j]]
			if left != right {
				return left < right
			}
			return members[i] < members[j]
		})
		cmd.(*redis.StringSliceCmd).SetVal(members)
	case "zrem":
		var deleted int64
		for i := 2; i < len(args); i++ {
			if _, ok := fake.indexes[text(1)][text(i)]; ok {
				deleted++
				delete(fake.indexes[text(1)], text(i))
			}
		}
		if len(fake.indexes[text(1)]) == 0 {
			delete(fake.indexes, text(1))
		}
		cmd.(*redis.IntCmd).SetVal(deleted)
	case "pexpire":
		milliseconds, _ := strconv.ParseInt(text(2), 10, 64)
		fake.expiry[text(1)] = time.Now().Add(time.Duration(milliseconds) * time.Millisecond)
		cmd.(*redis.BoolCmd).SetVal(true)
	case "del", "exists":
		var found int64
		for i := 1; i < len(args); i++ {
			_, valueExists := get(text(i))
			_, indexExists := fake.indexes[text(i)]
			if valueExists || indexExists {
				found++
			}
			if cmd.Name() == "del" {
				delete(fake.values, text(i))
				delete(fake.expiry, text(i))
				delete(fake.indexes, text(i))
			}
		}
		cmd.(*redis.IntCmd).SetVal(found)
	default:
		cmd.SetErr(fmt.Errorf("unsupported fake command %s", cmd.Name()))
	}
}
