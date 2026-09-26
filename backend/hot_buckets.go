package main

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Live event buckets distribute high-frequency writes. Each event carries the
// ORIGINAL interaction time, including removals; retries must not add twice.
var applyHotDelta = redis.NewScript(`
 if redis.call('EXISTS',KEYS[2])==1 then return 0 end
 redis.call('ZINCRBY',KEYS[1],ARGV[1],ARGV[2]);redis.call('PEXPIRE',KEYS[1],ARGV[3]);
 redis.call('SET',KEYS[2],'1','PX',ARGV[3]);return 1`)

func (a *App) updateHotBucket(ctx context.Context, e event) error {
	delta, ok := e.Data["delta"].(float64)
	if !ok {
		return nil
	}
	at, ok := e.Data["occurredAt"].(float64)
	if !ok {
		return nil
	}
	occurred := time.UnixMilli(int64(at))
	ttl := time.Until(occurred.Add(a.state().cfg.HotWindow + 2*time.Minute))
	if ttl <= 0 {
		return nil
	}
	key := fmt.Sprintf("hot:live:{events}:%d", occurred.Unix()/60)
	return a.withRedis(ctx, func(ctx context.Context) error {
		return applyHotDelta.Run(ctx, a.redis, []string{key, "hot:live:{events}:seen:" + e.ID}, delta, fmt.Sprintf("%020d", eventUint(e, "videoId")), ttl.Milliseconds()).Err()
	})
}

// Rebuild the authoritative rolling-window snapshot in SQL. Returning only the
// bounded top K avoids materializing every minute/video pair in Go and Redis.
// Live event buckets remain idempotent hints, not the authority for a snapshot.
func (a *App) bucketRanking(ctx context.Context, asOf time.Time) ([]rankEntry, error) {
	return a.computeRanking(ctx, "hot", asOf)
}
