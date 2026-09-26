package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/redis/go-redis/v9"
	"time"
)

func uploadKey(id string) string                 { return "upload:session:" + id }
func uploadHashKey(uid uint, hash string) string { return fmt.Sprintf("upload:hash:%d:%s", uid, hash) }
func (a *App) cacheUpload(u Upload) {
	raw, _ := json.Marshal(u)
	ttl := time.Until(u.CreatedAt.Add(24 * time.Hour))
	if ttl <= 0 {
		return
	}
	_ = a.withRedis(context.Background(), func(ctx context.Context) error {
		_, err := a.redis.TxPipelined(ctx, func(p redis.Pipeliner) error {
			p.Set(ctx, uploadKey(u.ID), raw, min(ttl, time.Minute))
			if !u.Published {
				p.Set(ctx, uploadHashKey(u.UserID, u.FileMD5), u.ID, ttl)
			} else {
				p.Del(ctx, uploadHashKey(u.UserID, u.FileMD5))
			}
			return nil
		})
		return err
	})
}
