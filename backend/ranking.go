package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var errCursorExpired = errors.New("feed_cursor_expired")

type rankEntry struct {
	ID    uint  `json:"id"`
	Score int64 `json:"score"`
}
type rankCursor struct {
	Version  int    `json:"v"`
	Sort     string `json:"sort"`
	Snapshot string `json:"snapshot"`
	Offset   int    `json:"offset"`
	Expires  int64  `json:"expires"`
}

func (a *App) signRankCursor(c rankCursor) string {
	raw, _ := json.Marshal(c)
	body := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, []byte(a.cfg.JWTSecret))
	mac.Write([]byte("rank-cursor:" + body))
	return "r1." + body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (a *App) parseRankCursor(raw, sort string) (rankCursor, error) {
	var c rankCursor
	if len(raw) > 1024 {
		return c, errors.New("invalid_cursor")
	}
	p := strings.Split(raw, ".")
	if len(p) != 3 || p[0] != "r1" {
		return c, errors.New("invalid_cursor")
	}
	sig, err := base64.RawURLEncoding.DecodeString(p[2])
	if err != nil {
		return c, errors.New("invalid_cursor")
	}
	mac := hmac.New(sha256.New, []byte(a.cfg.JWTSecret))
	mac.Write([]byte("rank-cursor:" + p[1]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return c, errors.New("invalid_cursor")
	}
	b, err := base64.RawURLEncoding.DecodeString(p[1])
	if err != nil || json.Unmarshal(b, &c) != nil {
		return c, errors.New("invalid_cursor")
	}
	if c.Version != 1 || c.Sort != sort || c.Offset < 0 || c.Offset%20 != 0 || c.Offset > 10000 {
		return c, errors.New("invalid_cursor")
	}
	if _, err := uuid.Parse(c.Snapshot); err != nil {
		return c, errors.New("invalid_cursor")
	}
	if time.Now().Unix() >= c.Expires {
		return c, errCursorExpired
	}
	return c, nil
}
func snapshotKeys(id string) (string, string) {
	return "feed:snapshot:{" + id + "}", "feed:rank:{" + id + "}"
}
func decodeRank(s FeedSnapshot) ([]rankEntry, error) {
	var entries []rankEntry
	if len(s.Entries) > 2*1024*1024 || json.Unmarshal(s.Entries, &entries) != nil || len(entries) > 10000 {
		return nil, errors.New("invalid_snapshot")
	}
	return entries, nil
}
func (a *App) cacheSnapshot(ctx context.Context, s FeedSnapshot) error {
	entries, err := decodeRank(s)
	if err != nil {
		return err
	}
	ttl := time.Until(s.ExpiresAt)
	if ttl <= 0 {
		return errCursorExpired
	}
	metadata, key := snapshotKeys(s.ID)
	raw, _ := json.Marshal(s)
	return a.withRedis(ctx, func(ctx context.Context) error {
		_, err := a.redis.TxPipelined(ctx, func(p redis.Pipeliner) error {
			p.Del(ctx, key)
			if len(entries) > 0 {
				members := make([]redis.Z, 0, len(entries))
				for _, e := range entries {
					members = append(members, redis.Z{Score: float64(e.Score), Member: fmt.Sprintf("%020d", e.ID)})
				}
				p.ZAdd(ctx, key, members...)
				p.PExpire(ctx, key, ttl)
			}
			p.Set(ctx, metadata, raw, ttl)
			return nil
		})
		return err
	})
}
func (a *App) snapshot(ctx context.Context, id, sort string) (FeedSnapshot, error) {
	var s FeedSnapshot
	metadata, _ := snapshotKeys(id)
	var raw []byte
	err := a.withRedis(ctx, func(ctx context.Context) error { var e error; raw, e = a.redis.Get(ctx, metadata).Bytes(); return e })
	if err == nil && json.Unmarshal(raw, &s) == nil && s.ID == id && s.Sort == sort {
		if _, e := decodeRank(s); e == nil && time.Now().Before(s.ExpiresAt) {
			return s, nil
		}
	}
	// Do not let partially decoded cache metadata add an unintended primary-key
	// predicate to GORM's fallback query.
	s = FeedSnapshot{}
	err = a.db.WithContext(ctx).Where("id = ? AND sort = ?", id, sort).First(&s).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return s, errCursorExpired
	}
	if err != nil {
		return s, err
	}
	if !time.Now().Before(s.ExpiresAt) {
		return s, errCursorExpired
	}
	return s, nil
}
func (a *App) computeRanking(ctx context.Context, sort string, asOf time.Time) ([]rankEntry, error) {
	s := a.state()
	entries := []rankEntry{}
	if sort == "likes" {
		err := a.db.WithContext(ctx).Model(&Video{}).Select("id, likes_count AS score").Order("likes_count DESC, id DESC").Limit(s.cfg.RankLimit).Scan(&entries).Error
		return entries, err
	}
	if sort != "hot" {
		return nil, errors.New("invalid_sort")
	}
	// A true rolling window: expired and removed interactions no longer count.
	// One SQL statement observes one consistent view; no timestamp buckets to round.
	cutoff := asOf.Add(-s.cfg.HotWindow)
	query := `SELECT v.id, COALESCE(a.score,0) AS score FROM videos v
 LEFT JOIN (SELECT video_id, SUM(weight) AS score FROM (
   SELECT video_id, 3 AS weight FROM likes WHERE created_at > ? AND created_at <= ?
   UNION ALL
   SELECT video_id, 5 AS weight FROM comments WHERE created_at > ? AND created_at <= ?
 ) events GROUP BY video_id) a ON a.video_id=v.id
 WHERE v.published_at <= ? ORDER BY score DESC, v.id DESC LIMIT ?`
	err := a.db.WithContext(ctx).Raw(query, cutoff, asOf, cutoff, asOf, asOf, s.cfg.RankLimit).Scan(&entries).Error
	return entries, err
}
func (a *App) latestRank(ctx context.Context, sort string) (FeedSnapshot, error) {
	s := a.state()
	now := time.Now().UTC()
	s.rankMu.Lock()
	cached, ok := s.ranks[sort]
	// Keep serving an immutable, valid snapshot during the next refresh.
	// Bound freshness independently of the longer cursor retention period.
	usable := ok && now.Before(cached.ExpiresAt) && now.Before(cached.CreatedAt.Add(2*s.cfg.RankRefresh))
	if usable && cached.ID != a.rankID(sort, now) && !s.rankRefreshing[sort] {
		if s.rankRefreshing == nil {
			s.rankRefreshing = make(map[string]bool)
		}
		s.rankRefreshing[sort] = true
		go func() {
			defer func() { s.rankMu.Lock(); delete(s.rankRefreshing, sort); s.rankMu.Unlock() }()
			work, cancel := context.WithTimeout(context.Background(), s.cfg.DBTimeout+4*s.cfg.RedisTimeout)
			defer cancel()
			if _, err := a.rebuildRank(work, sort); err != nil {
				log.Printf("background ranking refresh failed (%s): %v", sort, err)
			}
		}()
	}
	s.rankMu.Unlock()
	if usable {
		return cached, nil
	}
	return a.rebuildRank(ctx, sort)
}

func (a *App) rankID(sort string, now time.Time) string {
	cfg := a.state().cfg
	bucket := now.Truncate(cfg.RankRefresh)
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("owlet:rank:v1:%s:%s:%d:%d:%d", sort, cfg.HotWindow, cfg.RankLimit, cfg.RankRefresh, bucket.UnixNano()))).String()
}

func (a *App) rebuildRank(ctx context.Context, sort string) (FeedSnapshot, error) {
	s := a.state()
	now := time.Now().UTC()
	bucket := now.Truncate(s.cfg.RankRefresh)
	id := a.rankID(sort, now)
	s.rankMu.Lock()
	cached, ok := s.ranks[sort]
	s.rankMu.Unlock()
	if ok && cached.ID == id && time.Now().Before(cached.ExpiresAt) {
		return cached, nil
	}
	value, err, _ := s.flights.do(ctx, "rank:"+id, func() (any, error) {
		work, cancel := context.WithTimeout(context.Background(), s.cfg.DBTimeout+3*s.cfg.RedisTimeout)
		defer cancel()
		snapshot, err := a.snapshot(work, id, sort)
		if errors.Is(err, errCursorExpired) {
			release, e := a.dbSlot(work)
			if e != nil {
				return nil, e
			}
			defer release()
			var entries []rankEntry
			if sort == "hot" {
				entries, e = a.bucketRanking(work, now)
			} else {
				entries, e = a.computeRanking(work, sort, now)
			}
			if e != nil {
				return nil, e
			}
			raw, _ := json.Marshal(entries)
			snapshot = FeedSnapshot{ID: id, Sort: sort, Entries: raw, CreatedAt: now, ExpiresAt: bucket.Add(s.cfg.SnapshotTTL)}
			// Concurrent API/Worker builders converge on the immutable winning row.
			if e = a.db.WithContext(work).Clauses(clause.OnConflict{DoNothing: true}).Create(&snapshot).Error; e != nil {
				return nil, e
			}
			if e = a.db.WithContext(work).Where("id = ?", id).First(&snapshot).Error; e != nil {
				return nil, e
			}
			s.rankBuilds.Add(1)
		} else if err != nil {
			return nil, err
		}
		_ = a.cacheSnapshot(work, snapshot)
		s.rankMu.Lock()
		s.ranks[sort] = snapshot
		s.rankMu.Unlock()
		return snapshot, nil
	})
	if err != nil {
		return FeedSnapshot{}, err
	}
	return value.(FeedSnapshot), nil
}
func (a *App) rankPage(ctx context.Context, s FeedSnapshot, offset int) ([]rankEntry, int, error) {
	entries, err := decodeRank(s)
	if err != nil {
		return nil, 0, err
	}
	if offset > len(entries) {
		return nil, 0, errors.New("invalid_cursor")
	}
	end := min(offset+20, len(entries))
	page := entries[offset:end]
	_, key := snapshotKeys(s.ID)
	var members []redis.Z
	err = a.withRedis(ctx, func(ctx context.Context) error {
		var e error
		members, e = a.redis.ZRevRangeWithScores(ctx, key, int64(offset), int64(end-1)).Result()
		return e
	})
	if end == offset {
		return []rankEntry{}, end, nil
	}
	valid := err == nil && len(members) == len(page)
	ranked := make([]rankEntry, 0, len(page))
	for i, m := range members {
		member, ok := m.Member.(string)
		if !ok || i >= len(page) {
			valid = false
			break
		}
		id, e := strconv.ParseUint(member, 10, 64)
		if e != nil || uint(id) != page[i].ID || int64(m.Score) != page[i].Score {
			valid = false
			break
		}
		ranked = append(ranked, rankEntry{uint(id), int64(m.Score)})
	}
	if valid {
		return ranked, end, nil
	}
	// Durable fallback and lazy reconstruction after eviction or Redis restart.
	if err == nil {
		_ = a.cacheSnapshot(ctx, s)
	}
	return page, end, nil
}
func (a *App) rankedFeed(c *gin.Context, sort string) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), a.state().cfg.DBTimeout+4*a.state().cfg.RedisTimeout)
	defer cancel()
	var s FeedSnapshot
	var err error
	offset := 0
	raw := c.Query("cursor")
	if raw == "" {
		s, err = a.latestRank(ctx, sort)
	} else {
		var cursor rankCursor
		cursor, err = a.parseRankCursor(raw, sort)
		if err == nil {
			s, err = a.snapshot(ctx, cursor.Snapshot, sort)
			offset = cursor.Offset
			if err == nil && s.ExpiresAt.Unix() != cursor.Expires {
				err = errors.New("invalid_cursor")
			}
		} else if !strings.HasPrefix(raw, "r1.") {
			err = errCursorExpired
		}
	}
	if errors.Is(err, errCursorExpired) {
		errorJSON(c, 410, "feed_cursor_expired")
		return
	}
	if err != nil {
		if err.Error() == "invalid_cursor" {
			errorJSON(c, 400, "invalid_cursor")
		} else {
			c.Header("Retry-After", "1")
			errorJSON(c, 503, "feed_temporarily_unavailable")
		}
		return
	}
	entries, end, err := a.rankPage(ctx, s, offset)
	if err != nil {
		errorJSON(c, 400, "invalid_cursor")
		return
	}
	ids := make([]uint, 0, len(entries))
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	rows := []Video{}
	if len(ids) > 0 {
		if a.db.WithContext(ctx).Preload("User").Where("id IN ?", ids).Find(&rows).Error != nil {
			errorJSON(c, 503, "feed_temporarily_unavailable")
			return
		}
	}
	byID := make(map[uint]Video, len(rows))
	for _, v := range rows {
		byID[v.ID] = v
	}
	videos := make([]Video, 0, len(rows))
	for _, e := range entries {
		if v, ok := byID[e.ID]; ok {
			a.hydrateVideo(&v)
			videos = append(videos, v)
		}
	}
	all, _ := decodeRank(s)
	next := ""
	if end < len(all) {
		next = a.signRankCursor(rankCursor{1, sort, s.ID, end, s.ExpiresAt.Unix()})
	}
	c.Header("X-Feed-Snapshot", s.ID)
	c.JSON(200, gin.H{"items": videos, "nextCursor": next, "snapshotExpiresAt": s.ExpiresAt, "rankedAt": s.CreatedAt})
}
func (a *App) rankMaintenance(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			for _, sort := range []string{"hot", "likes"} {
				if s, err := a.rebuildRank(ctx, sort); err == nil {
					_ = a.cacheSnapshot(ctx, s)
				} else if ctx.Err() == nil {
					log.Printf("ranking refresh failed (%s): %v", sort, err)
				}
			}
			work, cancel := context.WithTimeout(ctx, a.state().cfg.DBTimeout)
			_ = a.db.WithContext(work).Where("expires_at < ?", time.Now()).Limit(100).Delete(&FeedSnapshot{}).Error
			cancel()
			timer.Reset(a.state().cfg.RankRefresh)
		}
	}
}
