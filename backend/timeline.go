package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

type timelineMeta struct {
	Floor uint
	Count int
}

var publishTimeline = redis.NewScript(`
 if redis.call('GET',KEYS[3])~=ARGV[1] then return 0 end
 redis.call('DEL',KEYS[1]);
 for i=4,#ARGV do redis.call('ZADD',KEYS[1],0,ARGV[i]) end
 redis.call('PEXPIRE',KEYS[1],ARGV[3]); redis.call('SET',KEYS[2],ARGV[2],'PX',ARGV[3]);
 redis.call('DEL',KEYS[3]);return 1`)

func timelineKeys(uid uint, epoch string) (string, string, string) {
	key := fmt.Sprintf("feed:timeline:{%d:%s}", uid, epoch)
	return key, key + ":meta", key + ":lock"
}
func (a *App) timelineQuery(ctx context.Context, uid uint) *gorm.DB {
	q := a.db.WithContext(ctx).Model(&Video{})
	if uid > 0 {
		q = q.Where("user_id IN (?)", a.db.Model(&Follow{}).Select("following_id").Where("follower_id = ?", uid))
	}
	return q
}
func (a *App) invalidateTimeline(uid uint) {
	_ = a.withRedis(context.Background(), func(ctx context.Context) error {
		// All following feeds inherit the publication epoch; a follow change only
		// advances that user's epoch. Old namespaces expire without scanning Redis.
		return a.redis.Set(ctx, fmt.Sprintf("feed:epoch:%d", uid), uuid.NewString(), 0).Err()
	})
}
func (a *App) timelineEpoch(ctx context.Context, uid uint) (string, error) {
	var vals []interface{}
	err := a.withRedis(ctx, func(ctx context.Context) error {
		var e error
		vals, e = a.redis.MGet(ctx, "feed:epoch:0", fmt.Sprintf("feed:epoch:%d", uid)).Result()
		return e
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%v:%v", vals[0], vals[1]), nil
}
func (a *App) timeline(ctx context.Context, uid uint, before uint) ([]Video, error) {
	epoch, err := a.timelineEpoch(ctx, uid)
	if err != nil {
		return a.coldTimeline(ctx, uid, before, 21)
	}
	key, metaKey, lock := timelineKeys(uid, epoch)
	var raw string
	read := func() error {
		return a.withRedis(ctx, func(ctx context.Context) error { var e error; raw, e = a.redis.Get(ctx, metaKey).Result(); return e })
	}
	if read() != nil {
		_, err, _ = a.state().flights.do(ctx, "timeline:"+key, func() (any, error) {
			work, cancel := context.WithTimeout(context.Background(), a.state().cfg.DBTimeout+time.Second)
			defer cancel()
			token := uuid.NewString()
			var acquired bool
			e := a.withRedis(work, func(ctx context.Context) error {
				var e error
				acquired, e = a.redis.SetNX(ctx, lock, token, a.state().cfg.DBTimeout+time.Second).Result()
				return e
			})
			if e != nil {
				return nil, e
			}
			if !acquired {
				return nil, errBusy
			}
			defer a.withRedis(context.Background(), func(ctx context.Context) error {
				return releaseVideoLock.Run(ctx, a.redis, []string{lock}, token).Err()
			})
			var ids []uint
			if e = a.timelineQuery(work, uid).Order("id DESC").Limit(1000).Pluck("id", &ids).Error; e != nil {
				return nil, e
			}
			meta := timelineMeta{Count: len(ids)}
			if len(ids) > 0 {
				meta.Floor = ids[len(ids)-1]
			}
			b, _ := json.Marshal(meta)
			args := []interface{}{token, string(b), 15000}
			for _, id := range ids {
				args = append(args, fmt.Sprintf("%020d", id))
			}
			e = a.withRedis(work, func(ctx context.Context) error {
				return publishTimeline.Run(ctx, a.redis, []string{key, metaKey, lock}, args...).Err()
			})
			return nil, e
		})
		if errors.Is(err, errBusy) {
			deadline := time.NewTimer(600 * time.Millisecond)
			defer deadline.Stop()
			tick := time.NewTicker(25 * time.Millisecond)
			defer tick.Stop()
		wait:
			for {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-deadline.C:
					return nil, errBusy
				case <-tick.C:
					if read() == nil {
						break wait
					}
				}
			}
		} else if err != nil {
			return a.coldTimeline(ctx, uid, before, 21)
		}
		if read() != nil {
			return a.coldTimeline(ctx, uid, before, 21)
		}
	}
	var meta timelineMeta
	if json.Unmarshal([]byte(raw), &meta) != nil || meta.Count < 0 || meta.Count > 1000 {
		return a.coldTimeline(ctx, uid, before, 21)
	}
	max := "+"
	if before > 0 {
		max = "(" + fmt.Sprintf("%020d", before)
	}
	var ids []string
	err = a.withRedis(ctx, func(ctx context.Context) error {
		// Detect independently evicted ZSETs instead of silently skipping hot rows.
		n, e := a.redis.ZCard(ctx, key).Result()
		if e != nil {
			return e
		}
		if int(n) != meta.Count {
			return errors.New("incomplete timeline")
		}
		ids, e = a.redis.ZRevRangeByLex(ctx, key, &redis.ZRangeBy{Max: max, Min: "-", Offset: 0, Count: 21}).Result()
		return e
	})
	if err != nil {
		return a.coldTimeline(ctx, uid, before, 21)
	}
	if len(ids) == 0 && meta.Count > 0 && (before == 0 || before > meta.Floor) {
		return a.coldTimeline(ctx, uid, before, 21)
	}
	rows := make([]Video, 0, 21)
	for _, rawID := range ids {
		n, e := strconv.ParseUint(rawID, 10, 64)
		if e != nil {
			return a.coldTimeline(ctx, uid, before, 21)
		}
		result, e := a.detail(ctx, uint(n))
		if e != nil {
			if errors.Is(e, errBusy) {
				return nil, e
			}
			return a.coldTimeline(ctx, uid, before, 21)
		}
		if result.value.Missing {
			return a.coldTimeline(ctx, uid, before, 21)
		}
		rows = append(rows, result.value.Video)
	}
	if len(rows) < 21 && meta.Count == 1000 {
		boundary := meta.Floor
		if before > 0 && before < boundary {
			boundary = before
		}
		cold, e := a.coldTimeline(ctx, uid, boundary, 21-len(rows))
		if e != nil {
			return nil, e
		}
		rows = append(rows, cold...)
	}
	return rows, nil
}
func (a *App) coldTimeline(ctx context.Context, uid, before uint, limit int) ([]Video, error) {
	rows := []Video{}
	q := a.timelineQuery(ctx, uid).Preload("User")
	if before > 0 {
		q = q.Where("id < ?", before)
	}
	err := q.Order("id DESC").Limit(limit).Find(&rows).Error
	return rows, err
}
func (a *App) invalidateAuthor(uid uint) {
	var ids []uint
	if a.db.Model(&Video{}).Where("user_id = ?", uid).Pluck("id", &ids).Error == nil {
		for _, id := range ids {
			a.invalidateVideo(id)
		}
	}
}
