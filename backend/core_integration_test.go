package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

func TestMigrationDSNIsolation(t *testing.T) {
	for _, pair := range [][2]string{{"u:p@tcp(localhost:3306)/same", "u:p@tcp(127.0.0.1:3306)/same"}, {"u:p@tcp(localhost:3306)/", "u:p@tcp(localhost:3306)/next"}} {
		if distinctMigrationDSNs(pair[0], pair[1]) == nil {
			t.Fatal("unsafe migration accepted")
		}
	}
	if err := distinctMigrationDSNs("u:p@tcp(localhost:3306)/old", "u:p@tcp(localhost:3306)/next"); err != nil {
		t.Fatal(err)
	}
}

// Owns two uniquely named databases on an explicitly configured test server.
// It never reads application MYSQL_DSN or a production database.
func TestCoreMigrationAndCompatibility(t *testing.T) {
	dsn := os.Getenv("CORE_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("CORE_TEST_MYSQL_DSN not configured")
	}
	root, err := openDatabase(dsn, &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	rootSQL, _ := root.DB()
	t.Cleanup(func() { rootSQL.Close() })
	prefix := "owlet_core_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	sourceName, targetName := prefix+"_old", prefix+"_new"
	for _, name := range []string{sourceName, targetName} {
		if err := root.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci").Error; err != nil {
			t.Fatal(err)
		}
		n := name
		t.Cleanup(func() { root.Exec("DROP DATABASE `" + n + "`") })
	}
	cfg, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ParseTime = true
	cfg.DBName = sourceName
	sourceDSN := cfg.FormatDSN()
	cfg.DBName = targetName
	targetDSN := cfg.FormatDSN()
	source, err := openDatabase(sourceDSN, &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	target, err := openDatabase(targetDSN, &gorm.Config{TranslateError: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, db := range []*gorm.DB{source, target} {
		sqlDB, _ := db.DB()
		t.Cleanup(func() { sqlDB.Close() })
	}
	if err := source.AutoMigrate(&InteractionCommand{}, &User{}, &Invite{}, &Session{}, &Video{}, &Upload{}, &Like{}, &Comment{}, &Follow{}, &VideoTag{}, &Message{}, &Notification{}, &Outbox{}, &ProcessedEvent{}, &FeedSnapshot{}); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("CORE_TEST_LEGACY_SCHEMA") == "1" {
		if err := source.Migrator().DropTable(&InteractionCommand{}, &FeedSnapshot{}); err != nil {
			t.Fatal(err)
		}
	}
	dataDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dataDir, "media", "videos"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "media", "videos", "sample.mp4"), []byte("fixture-media"), 0600); err != nil {
		t.Fatal(err)
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte("migration-test-password"), bcrypt.MinCost)
	users := []User{{ID: 51, Username: "migration_author", PasswordHash: string(hash), Bio: strings.Repeat("b", 400)}, {ID: 92, Username: "migration_viewer", PasswordHash: string(hash)}}
	if err := source.Create(&users).Error; err != nil {
		t.Fatal(err)
	}
	v := Video{ID: 73, UserID: 51, Title: strings.Repeat("t", 150), Description: strings.Repeat("d", 1600), FilePath: "videos/sample.mp4", Size: 13, LikesCount: 1, CommentsCount: 1, Popularity: 8, PublishedAt: time.Now()}
	for _, row := range []any{&v, &Like{ID: 18, UserID: 92, VideoID: 73}, &Comment{ID: 27, UserID: 92, VideoID: 73, Body: "keep this comment"}, &Follow{ID: 39, FollowerID: 92, FollowingID: 51}, &Message{ID: 44, SenderID: 92, RecipientID: 51, Body: "private message survives"}, &Notification{ID: 88, UserID: 51, ActorID: 92, VideoID: &v.ID, Kind: "like", Body: "liked your video"}, &VideoTag{ID: 33, VideoID: 73, Tag: "migration"}} {
		if err := source.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("SOURCE_MYSQL_DSN", sourceDSN)
	oldApp := &App{cfg: Config{JWTSecret: strings.Repeat("c", 32)}}
	oldSessionID := uuid.NewString()
	oldRefresh, err := oldApp.signToken(92, oldSessionID, "refresh", refreshTTL)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Create(&Session{ID: oldSessionID, UserID: 92, RefreshHash: shaHex(oldRefresh), ExpiresAt: time.Now().Add(refreshTTL)}).Error; err != nil {
		t.Fatal(err)
	}
	// A completed but unpublished upload must survive migration and remain publishable.
	uploadID := uuid.NewString()
	uploadPath := filepath.Join(dataDir, "pending.mp4")
	if err := os.WriteFile(uploadPath, []byte("pending-media"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := source.Create(&Upload{ID: uploadID, UserID: 92, FileMD5: strings.Repeat("a", 32), Size: 13, Chunks: 1, Completed: true, StoredPath: uploadPath}).Error; err != nil {
		t.Fatal(err)
	}
	t.Setenv("TARGET_MYSQL_DSN", targetDSN)
	t.Setenv("SOURCE_DATA_DIR", dataDir)
	targetDataDir := t.TempDir()
	t.Setenv("TARGET_DATA_DIR", targetDataDir)
	t.Setenv("MIGRATION_SOURCE_QUIESCED", "1")
	if err := migrateCore(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if target.Migrator().HasTable("account") {
		t.Fatal("inspect mutated target")
	}
	if err := migrateCore(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if err := coreReady(target); err != nil {
		t.Fatal(err)
	}
	if err := verifyCoreContents(source, target); err != nil {
		t.Fatal(err)
	}
	if err := migrateCore(context.Background(), true); err == nil {
		t.Fatal("reapply should not overwrite verified database")
	}
	redisAddr := os.Getenv("CORE_TEST_REDIS_ADDR")
	rabbit := os.Getenv("CORE_TEST_RABBITMQ_URL")
	if redisAddr == "" || rabbit == "" {
		t.Skip("migration verified; core runtime dependencies not configured")
	}
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr, DB: 14})
	t.Cleanup(func() { rdb.Close() })
	if err := rdb.FlushDB(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
	a := &App{cfg: Config{JWTSecret: strings.Repeat("c", 32), DataDir: targetDataDir, RabbitURL: rabbit, RedisAddr: redisAddr}.defaults(), db: target, redis: rdb}
	if err := a.initCore(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.core.mq.Close(); a.state().publisher.close() })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.coreWorker(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("worker did not stop")
		}
	})
	cookies, _ := sessionFixture(t, a, 92)
	if w := callAPI(a, "POST", "/api/v1/auth/refresh", "", []*http.Cookie{{Name: "owlet_refresh", Value: oldRefresh}}); w.Code != 200 {
		t.Fatalf("migrated refresh: %d %s", w.Code, w.Body.String())
	}
	if w := callAPI(a, "POST", "/api/v1/auth/login", `{"username":"migration_viewer","password":"migration-test-password"}`, nil); w.Code != 200 {
		t.Fatalf("old password login: %d %s", w.Code, w.Body.String())
	}
	for _, path := range []string{"/api/v1/auth/me", "/api/v1/videos?sort=latest", "/api/v1/videos?sort=recommend", "/api/v1/videos?sort=following", "/api/v1/videos/73", "/api/v1/users/51", "/api/v1/messages/51", "/api/v1/tags/migration/videos", "/api/v1/playback-config", "/api/v1/notifications"} {
		if w := callAPI(a, "GET", path, "", cookies); w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	if w := callAPI(a, "GET", "/api/v1/videos?sort=recommend", "", nil); w.Code != 401 {
		t.Fatalf("anonymous recommendation: %d", w.Code)
	}
	if w := callAPI(a, "POST", "/api/v1/video-view-events", `{"video_id":73,"scene":"recommend","request_id":"test-exposed","event_type":"exposed","watch_ms":0,"completed":false}`, cookies); w.Code != 201 {
		t.Fatalf("exposure: %d %s", w.Code, w.Body.String())
	}
	if w := callAPI(a, "GET", "/api/v1/videos?sort=recommend", "", cookies); w.Code != 200 || strings.Contains(w.Body.String(), `"id":73`) {
		t.Fatalf("exposure exclusion: %s", w.Body.String())
	}
	for _, kind := range []string{"like", "favorite"} {
		if w := callAPI(a, "PUT", "/api/v1/videos/73/"+kind, "", cookies); w.Code != 200 {
			t.Fatalf("%s: %d %s", kind, w.Code, w.Body.String())
		}
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		var count int64
		target.Table("interaction_action").Where("user_id=92 AND video_id=73 AND action_type='favorite' AND status=1").Count(&count)
		if count == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("favorite was not persisted by worker")
		}
		time.Sleep(25 * time.Millisecond)
	}
	w := callAPI(a, "POST", "/api/v1/videos/73/comments", `{"body":"new comment @migration_author"}`, cookies)
	if w.Code != 201 {
		t.Fatalf("comment: %d %s", w.Code, w.Body.String())
	}
	var comment Comment
	if err := json.Unmarshal(w.Body.Bytes(), &comment); err != nil {
		t.Fatal(err)
	}
	if w := callAPI(a, "DELETE", fmt.Sprintf("/api/v1/comments/%d", comment.ID), "", cookies); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := callAPI(a, "DELETE", "/api/v1/users/51/follow", "", cookies); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := callAPI(a, "PUT", "/api/v1/users/51/follow", "", cookies); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := callAPI(a, "POST", "/api/v1/messages/51", `{"body":"private message after migration"}`, cookies); w.Code != 201 {
		t.Fatalf("private message: %d %s", w.Code, w.Body.String())
	}
	if w := callAPI(a, "GET", "/api/v1/me/favorites", "", cookies); w.Code != 200 || !strings.Contains(w.Body.String(), `"id":73`) {
		t.Fatalf("favorites collection: %s", w.Body.String())
	}
	authorCookies, _ := sessionFixture(t, a, 51)
	if w := callAPI(a, "PATCH", "/api/v1/notifications/read", `{"id":88}`, authorCookies); w.Code != 200 {
		t.Fatalf("read migrated notification: %s", w.Body.String())
	}
	var readCount int64
	if err := target.Table("user_message").Where("id=88 AND is_read=1 AND read_at IS NOT NULL").Count(&readCount).Error; err != nil || readCount != 1 {
		t.Fatal("notification read states diverged")
	}
	inviteCode := strings.ReplaceAll(uuid.NewString(), "-", "")
	if err := target.Create(&Invite{CodeHash: shaHex(inviteCode), ExpiresAt: time.Now().Add(time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	w = callAPI(a, "POST", "/api/v1/auth/register", fmt.Sprintf(`{"username":"after_migration","password":"registration-test-password","invite":%q}`, inviteCode), nil)
	if w.Code != 201 {
		t.Fatalf("invite registration: %d %s", w.Code, w.Body.String())
	}
	if w := callAPI(a, "PATCH", "/api/v1/me", `{"bio":"updated after migration"}`, cookies); w.Code != 200 {
		t.Fatalf("profile: %s", w.Body.String())
	}
	var srcUser User
	published := callAPI(a, "POST", "/api/v1/videos", fmt.Sprintf(`{"uploadId":%q,"title":"published after migration","description":"#migration"}`, uploadID), cookies)
	if published.Code != 201 {
		t.Fatalf("publish migrated upload: %d %s", published.Code, published.Body.String())
	}
	var publishedVideo Video
	if err := json.Unmarshal(published.Body.Bytes(), &publishedVideo); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(15 * time.Second)
	for {
		var count int64
		if err := target.Table("video_embedding").Where("video_id = ?", publishedVideo.ID).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("published upload did not reach embedding worker")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(uploadPath); err != nil {
		t.Fatal("publishing changed source media")
	}
	if err := source.First(&srcUser, 92).Error; err != nil || srcUser.Bio != "" {
		t.Fatal("source changed")
	}
}

func TestCoreMigrationOlderSchema(t *testing.T) {
	t.Setenv("CORE_TEST_LEGACY_SCHEMA", "1")
	TestCoreMigrationAndCompatibility(t)
}
