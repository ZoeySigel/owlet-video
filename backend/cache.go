package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

var releaseVideoLock = redis.NewScript(`if redis.call('GET',KEYS[1]) == ARGV[1] then return redis.call('DEL',KEYS[1]) end; return 0`)
var publishVideoCache = redis.NewScript(`if redis.call('GET',KEYS[2]) ~= ARGV[1] then return 0 end; redis.call('SET',KEYS[1],ARGV[2],'PX',ARGV[3]); redis.call('DEL',KEYS[2]); return 1`)
var evictVideoCache = redis.NewScript(`return redis.call('DEL',KEYS[1],KEYS[2])`)

type videoResult struct {
	value videoEnvelope
	stale bool
}

func videoCacheKeys(id uint) (string, string) {
	return fmt.Sprintf("video:v2:{%d}", id), fmt.Sprintf("video:v2:{%d}:lock", id)
}
func (a *App) readVideoCache(ctx context.Context, id uint) (videoEnvelope, bool, error) {
	key, _ := videoCacheKeys(id)
	var raw []byte
	err := a.withRedis(ctx, func(ctx context.Context) error { var e error; raw, e = a.redis.Get(ctx, key).Bytes(); return e })
	if errors.Is(err, redis.Nil) {
		return videoEnvelope{}, false, nil
	}
	if err != nil {
		return videoEnvelope{}, false, err
	}
	var v videoEnvelope
	if json.Unmarshal(raw, &v) != nil || (!v.Missing && v.Video.ID != id) {
		return videoEnvelope{}, false, nil
	}
	return v, true, nil
}
func (a *App) detail(ctx context.Context, id uint) (videoResult, error) {
	s := a.state()
	if v, ok := s.videos.get(id, false); ok {
		return videoResult{value: v}, nil
	}
	generation := s.videos.generation()
	value, err, shared := s.flights.do(ctx, fmt.Sprintf("video:%d:%d", id, generation), func() (any, error) {
		loadCtx, cancel := context.WithTimeout(context.Background(), s.cfg.DBTimeout+3*s.cfg.RedisTimeout+time.Second)
		defer cancel()
		stale := func() (videoResult, bool) {
			v, ok := s.videos.get(id, true)
			return videoResult{value: v, stale: true}, ok && !v.Missing
		}
		cached, hit, redisErr := a.readVideoCache(loadCtx, id)
		if hit {
			s.videos.put(id, cached, generation)
			return videoResult{value: cached}, nil
		}
		key, lock := videoCacheKeys(id)
		token := uuid.NewString()
		locked := false
		if redisErr == nil {
			redisErr = a.withRedis(loadCtx, func(ctx context.Context) error {
				var e error
				locked, e = a.redis.SetNX(ctx, lock, token, s.cfg.DBTimeout+time.Second).Result()
				return e
			})
			if redisErr == nil && !locked {
				if v, ok := stale(); ok {
					return v, nil
				}
				// Do not turn lock contention into a stampede against the database.
				deadline := time.NewTimer(600 * time.Millisecond)
				defer deadline.Stop()
				tick := time.NewTicker(25 * time.Millisecond)
				defer tick.Stop()
			waiting:
				for {
					select {
					case <-loadCtx.Done():
						return nil, loadCtx.Err()
					case <-deadline.C:
						return nil, errBusy
					case <-tick.C:
						cached, hit, redisErr = a.readVideoCache(loadCtx, id)
						if hit {
							s.videos.put(id, cached, generation)
							return videoResult{value: cached}, nil
						}
						if redisErr != nil {
							break waiting
						}
					}
				}
			}
		}
		if locked {
			defer a.withRedis(context.Background(), func(ctx context.Context) error {
				return releaseVideoLock.Run(ctx, a.redis, []string{lock}, token).Err()
			})
			// A preceding owner may have filled the cache between GET and SET NX.
			if cached, hit, _ := a.readVideoCache(loadCtx, id); hit {
				s.videos.put(id, cached, generation)
				return videoResult{value: cached}, nil
			}
		}
		release, err := a.dbSlot(loadCtx)
		if err != nil {
			if v, ok := stale(); ok {
				return v, nil
			}
			return nil, err
		}
		defer release()
		dbCtx, dbCancel := context.WithTimeout(loadCtx, s.cfg.DBTimeout)
		defer dbCancel()
		s.detailLoads.Add(1)
		var v Video
		err = a.db.WithContext(dbCtx).Preload("User").First(&v, id).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			if old, ok := stale(); ok {
				return old, nil
			}
			return nil, err
		}
		result := videoEnvelope{Video: v, Missing: errors.Is(err, gorm.ErrRecordNotFound)}
		if !result.Missing {
			a.hydrateVideo(&result.Video)
		}
		canStoreLocal := true
		if locked {
			payload, _ := json.Marshal(result)
			ttl := 4*time.Minute + time.Duration(rand.IntN(120))*time.Second
			if result.Missing {
				ttl = 20 * time.Second
			}
			var stored int64
			e := a.withRedis(loadCtx, func(ctx context.Context) error {
				var err error
				stored, err = publishVideoCache.Run(ctx, a.redis, []string{key, lock}, token, payload, ttl.Milliseconds()).Int64()
				return err
			})
			// An invalidation revoked our lease: never publish this old read into L1.
			canStoreLocal = e != nil || stored == 1
		}
		if canStoreLocal {
			s.videos.put(id, result, generation)
		}
		return videoResult{value: result}, nil
	})
	if shared {
		s.sharedLoads.Add(1)
	}
	if err != nil {
		return videoResult{}, err
	}
	result := value.(videoResult)
	if result.stale {
		s.staleReads.Add(1)
	}
	return result, nil
}
func (a *App) invalidateVideo(id uint) {
	a.state().videos.invalidate(id)
	key, lock := videoCacheKeys(id)
	_ = a.withRedis(context.Background(), func(ctx context.Context) error { return evictVideoCache.Run(ctx, a.redis, []string{key, lock}).Err() })
}
