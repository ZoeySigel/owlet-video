package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	accountmodel "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/account"
	feedrepo "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/feed"
	interactionmodel "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/interaction"
	videomodel "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/video"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

type fixture struct {
	cfg          config
	db           *gorm.DB
	rdb          *redis.Client
	startEvicted int64
}

func isLoopbackAddress(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func authorizeAndPrepare(ctx context.Context, db *gorm.DB, rdb *redis.Client) error {
	var count int64
	if err := db.Raw("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE()").Scan(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		if err := db.Exec("CREATE TABLE cache_validation_marker (id INT PRIMARY KEY, purpose VARCHAR(128) NOT NULL)").Error; err != nil {
			return err
		}
		if err := db.Exec("INSERT INTO cache_validation_marker (id,purpose) VALUES (1,?)", purpose).Error; err != nil {
			return err
		}
	}
	var marker string
	if err := db.Raw("SELECT purpose FROM cache_validation_marker WHERE id=1").Scan(&marker).Error; err != nil {
		return fmt.Errorf("database is not an authorized fixture: %w", err)
	}
	if marker != purpose {
		return fmt.Errorf("unexpected database marker %q", marker)
	}
	v, err := rdb.Get(ctx, markerKey).Result()
	if err == redis.Nil {
		n, e := rdb.DBSize(ctx).Result()
		if e != nil {
			return e
		}
		if n != 0 {
			return fmt.Errorf("refusing Redis with %d existing keys and no validation marker", n)
		}
		if e = rdb.Set(ctx, markerKey, purpose, 0).Err(); e != nil {
			return e
		}
	} else if err != nil {
		return err
	} else if v != purpose {
		return fmt.Errorf("unexpected Redis marker %q", v)
	}
	maxmemory, err := rdb.ConfigGet(ctx, "maxmemory").Result()
	if err != nil {
		return err
	}
	policy, err := rdb.ConfigGet(ctx, "maxmemory-policy").Result()
	if err != nil {
		return err
	}
	if maxmemory["maxmemory"] != "67108864" || policy["maxmemory-policy"] != "allkeys-lru" {
		return fmt.Errorf("require dedicated Redis maxmemory=67108864 and allkeys-lru; got %v %v", maxmemory, policy)
	}
	if err = db.AutoMigrate(&accountmodel.UserModel{}, &videomodel.VideoModel{}, &videomodel.VideoStatModel{}, &interactionmodel.ActionModel{}); err != nil {
		return err
	}
	return feedrepo.EnsureTimelineIndex(db)
}

func (f *fixture) reset(ctx context.Context) error {
	var marker string
	if err := f.db.Raw("SELECT purpose FROM cache_validation_marker WHERE id=1").Scan(&marker).Error; err != nil || marker != purpose {
		return fmt.Errorf("database fixture authorization missing: %v", err)
	}
	if v, e := f.rdb.Get(ctx, markerKey).Result(); e != nil || v != purpose {
		return fmt.Errorf("Redis fixture authorization missing: %v", e)
	}
	// Only the four tables owned by this fixture are reset. No database is dropped.
	for _, table := range []string{"interaction_action", "video_stat", "video", "account"} {
		if err := f.db.Exec("TRUNCATE TABLE " + table).Error; err != nil {
			return err
		}
	}
	if err := f.rdb.FlushDB(ctx).Err(); err != nil {
		return err
	}
	if err := f.rdb.Set(ctx, markerKey, purpose, 0).Err(); err != nil {
		return err
	}
	f.startEvicted = readMemory(ctx, f.rdb).EvictedKeys
	users := make([]accountmodel.UserModel, 0, f.cfg.Users)
	for i := 1; i <= f.cfg.Users; i++ {
		users = append(users, accountmodel.UserModel{ID: int64(i), Account: fmt.Sprintf("validation-%06d", i), Password: "not-used-by-validation", Nickname: fmt.Sprintf("User %d", i), AvatarURL: "/validation/avatar.png", Status: 1, Role: "user"})
	}
	if err := f.db.CreateInBatches(users, 500).Error; err != nil {
		return err
	}
	videos := make([]videomodel.VideoModel, 0, f.cfg.Videos)
	stats := make([]videomodel.VideoStatModel, 0, f.cfg.Videos)
	actions := make([]interactionmodel.ActionModel, 0, f.cfg.Videos*4)
	for i := 1; i <= f.cfg.Videos; i++ {
		id := int64(i)
		published := publishedAt(id)
		videos = append(videos, videomodel.VideoModel{ID: id, AuthorID: 1 + id%int64(f.cfg.Users), Title: videoTitle(id, 0), Description: strings.Repeat("Cache validation metadata. ", 8), MediaURL: fmt.Sprintf("/validation/%d.mp4", i), CoverURL: fmt.Sprintf("/validation/%d.jpg", i), Status: 2, PublishedAt: &published})
		stats = append(stats, videomodel.VideoStatModel{VideoID: id, LikeCount: 3, FavoriteCount: 1})
		for j := 0; j < 3; j++ {
			actions = append(actions, interactionmodel.ActionModel{UserID: 1 + (id+int64(j))%int64(f.cfg.Users), VideoID: id, ActionType: "LIKE", Status: 1})
		}
		actions = append(actions, interactionmodel.ActionModel{UserID: 1 + id%int64(f.cfg.Users), VideoID: id, ActionType: "FAVORITE", Status: 1})
	}
	if err := f.db.CreateInBatches(videos, 500).Error; err != nil {
		return err
	}
	if err := f.db.CreateInBatches(stats, 500).Error; err != nil {
		return err
	}
	return f.db.CreateInBatches(actions, 500).Error
}

func publishedAt(id int64) time.Time {
	return time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(id) * time.Second)
}
func videoTitle(id, version int64) string { return fmt.Sprintf("video-%06d-v%06d", id, version) }

func cursorFor(id int64) string {
	b, _ := json.Marshal(struct {
		PublishedAt string `json:"published_at"`
		VideoID     int64  `json:"video_id"`
	}{publishedAt(id).Format(time.RFC3339Nano), id})
	return base64.RawURLEncoding.EncodeToString(b)
}

type pageSpec struct {
	Cursor string  `json:"cursor"`
	IDs    []int64 `json:"expected_ids"`
}
type catalog struct {
	Target   int64      `json:"target_video"`
	Overlap  int        `json:"overlap"`
	Pages    []pageSpec `json:"pages"`
	Affected []int      `json:"affected_pages"`
}

func makeCatalog(videos, size, overlap int) catalog {
	c := catalog{Target: int64(videos / 2), Overlap: overlap}
	seen := map[string]bool{}
	add := func(before int64) {
		cursor := ""
		top := int64(videos)
		if before <= int64(videos) {
			b, _ := json.Marshal(struct {
				PublishedAt string `json:"published_at"`
				VideoID     int64  `json:"video_id"`
			}{publishedAt(before).Format(time.RFC3339Nano), before})
			cursor = base64.RawURLEncoding.EncodeToString(b)
			top = before - 1
		}
		if seen[cursor] {
			return
		}
		seen[cursor] = true
		p := pageSpec{Cursor: cursor}
		for id := top; id > 0 && len(p.IDs) < size; id-- {
			p.IDs = append(p.IDs, id)
		}
		index := len(c.Pages)
		for _, id := range p.IDs {
			if id == c.Target {
				c.Affected = append(c.Affected, index)
				break
			}
		}
		c.Pages = append(c.Pages, p)
	}
	for offset := 0; offset < overlap; offset++ {
		add(c.Target + 1 + int64(offset))
	}
	for before := int64(videos + 1); before > int64(size) && len(c.Pages) < 200; before -= int64(size) {
		if before > c.Target && before-int64(size) <= c.Target {
			continue
		}
		add(before)
	}
	return c
}
