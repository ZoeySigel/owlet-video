package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestCountersSnapshotsDoNotAliasAndConcurrentAdds(t *testing.T) {
	ctx, op := withOperation(context.Background())
	var wait sync.WaitGroup
	for i := 0; i < 12; i++ {
		wait.Add(1)
		go func() { defer wait.Done(); observeCache(ctx, "card", 1, 2); op.add(Counters{SQLQueries: 1}) }()
	}
	wait.Wait()
	first := op.Snapshot()
	if first.SQLQueries != 12 || first.Hits["card"] != 12 || first.Misses["card"] != 24 {
		t.Fatalf("unexpected snapshot: %+v", first)
	}
	first.Hits["card"] = 999
	second := op.Snapshot()
	if second.Hits["card"] != 12 {
		t.Fatal("snapshot mutated live operation")
	}
	sum := first.Add(second)
	sum.Misses["card"] = 0
	if first.Misses["card"] != 24 || second.Misses["card"] != 24 {
		t.Fatal("Add aliased its inputs")
	}
}

func TestSQLMeterCountsRowsErrorsAndContext(t *testing.T) {
	ctx, op := withOperation(context.Background())
	meter := newSQLMeter()
	meter.Trace(ctx, time.Now().Add(-time.Millisecond), func() (string, int64) { return "SELECT", 3 }, nil)
	meter.Trace(ctx, time.Now(), func() (string, int64) { return "failed", -1 }, errors.New("unavailable"))
	meter.Trace(context.Background(), time.Now(), func() (string, int64) { t.Fatal("unobserved SQL evaluated"); return "", 0 }, nil)
	got := op.Snapshot()
	if got.SQLQueries != 2 || got.SQLRows != 3 || got.SQLErrors != 1 || got.SQLDurationNS <= 0 {
		t.Fatalf("unexpected SQL accounting: %+v", got)
	}
}

func TestRedisMeterPipelineCountsActualDeletesAndPayload(t *testing.T) {
	ctx, op := withOperation(context.Background())
	set := redis.NewStatusCmd(ctx, "set", "key", "value", "px", 1000)
	set.SetVal("OK")
	get := redis.NewStringCmd(ctx, "get", "key")
	get.SetVal("value")
	missing := redis.NewStringCmd(ctx, "get", "absent")
	missing.SetErr(redis.Nil)
	remove := redis.NewIntCmd(ctx, "del", "key", "absent")
	remove.SetVal(1)
	index := redis.NewIntCmd(ctx, "zadd", wholeIndexKey(7), 100, "page")
	index.SetVal(0) // An existing member's score is still a successful write.
	failed := redis.NewStatusCmd(ctx, "set", "bad", "value")
	failed.SetErr(errors.New("offline"))
	commands := []redis.Cmder{set, get, missing, remove, index, failed}
	hook := redisMeter{}.ProcessPipelineHook(func(context.Context, []redis.Cmder) error { return failed.Err() })
	_ = hook(ctx, commands)
	got := op.Snapshot()
	if got.RedisCommands != 6 || got.RedisErrors != 1 || got.KeysDeleted != 1 || got.KeysWritten != 2 || got.IndexWrites != 1 {
		t.Fatalf("unexpected Redis accounting: %+v", got)
	}
	wantWrite := int64(len("keyvalue") + len("keyabsent") + len(wholeIndexKey(7)) + len("100page"))
	if got.RedisWriteBytes != wantWrite {
		t.Fatalf("write bytes=%d, want %d", got.RedisWriteBytes, wantWrite)
	}
	// OK + value + integer DEL result + integer ZADD result; nil/error have no payload.
	if got.RedisReadBytes != int64(len("OKvalue10")) {
		t.Fatalf("response bytes=%d", got.RedisReadBytes)
	}
}
