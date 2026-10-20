package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Faults affect only connections opened through this test-owned proxy.
type faultProxy struct {
	listener net.Listener
	target   string
	mu       sync.Mutex
	enabled  bool
	peers    map[net.Conn]bool
	wg       sync.WaitGroup
}

func newFaultProxy(t *testing.T, target string) *faultProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &faultProxy{listener: ln, target: target, enabled: true, peers: make(map[net.Conn]bool)}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			p.wg.Add(1)
			go p.pipe(c)
		}
	}()
	t.Cleanup(func() { ln.Close(); p.set(false); p.wg.Wait() })
	return p
}
func (p *faultProxy) pipe(c net.Conn) {
	defer p.wg.Done()
	defer c.Close()
	remote, err := net.DialTimeout("tcp", p.target, time.Second)
	if err != nil {
		return
	}
	defer remote.Close()
	p.mu.Lock()
	if !p.enabled {
		p.mu.Unlock()
		return
	}
	p.peers[c] = true
	p.peers[remote] = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.peers, c); delete(p.peers, remote); p.mu.Unlock() }()
	done := make(chan struct{})
	go func() { io.Copy(remote, c); remote.Close(); close(done) }()
	io.Copy(c, remote)
	c.Close()
	<-done
}
func (p *faultProxy) set(enabled bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.enabled = enabled
	if !enabled {
		for c := range p.peers {
			c.Close()
		}
	}
}

func integrationApp(t *testing.T) *App {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN not set")
	}
	gin.SetMode(gin.TestMode)
	db, err := openDatabase(dsn, &gorm.Config{TranslateError: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err = initializeCoreDatabase(db); err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(16)
	t.Cleanup(func() { sqlDB.Close() })
	r := testRedis(env("TEST_REDIS_ADDR", "127.0.0.1:6379"))
	t.Cleanup(func() { r.Close() })
	if err = r.Ping(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: Config{JWTSecret: "test-only-signing-secret-with-32-characters", RabbitURL: os.Getenv("TEST_RABBITMQ_URL")}.defaults(), db: db, redis: r}
	if err := app.initCore(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.core.mq.Close() })
	return app
}
func testRedis(addr string) *redis.Client {
	return redis.NewClient(&redis.Options{Addr: addr, Password: os.Getenv("TEST_REDIS_PASSWORD"), DialTimeout: 200 * time.Millisecond, ReadTimeout: 200 * time.Millisecond, WriteTimeout: 200 * time.Millisecond, PoolTimeout: 200 * time.Millisecond, ContextTimeoutEnabled: true, MaxRetries: -1})
}
func fixtureVideos(t *testing.T, a *App, n int) []Video {
	t.Helper()
	u := User{Username: "test" + uuid.NewString()[:12], PasswordHash: "not-a-login"}
	if err := a.createCoreUser(a.db, &u); err != nil {
		t.Fatal(err)
	}
	videos := make([]Video, n)
	for i := range videos {
		videos[i] = Video{UserID: u.ID, Title: fmt.Sprintf("fixture-%d", i), FilePath: "test.mp4", PublishedAt: time.Now().Add(-time.Hour), LikesCount: 1000000}
	}
	for i := range videos {
		if err := a.createCoreVideo(a.db, &videos[i]); err != nil {
			t.Fatal(err)
		}
		if err := a.db.Table("video_stat").Where("video_id = ?", videos[i].ID).Update("like_count", videos[i].LikesCount).Error; err != nil {
			t.Fatal(err)
		}
	}
	ids := make([]uint, n)
	for i, v := range videos {
		ids[i] = v.ID
	}
	t.Cleanup(func() {
		for _, table := range []string{"interaction_action", "interaction_comment", "video_stat", "video_embedding"} {
			a.db.Exec("DELETE FROM "+table+" WHERE video_id IN ?", ids)
		}
		a.db.Exec("DELETE FROM user_message WHERE user_id = ?", u.ID)
		a.db.Exec("DELETE FROM video WHERE id IN ?", ids)
		a.db.Exec("DELETE FROM account WHERE id = ?", u.ID)
		for _, id := range ids {
			a.invalidateVideo(id)
		}
	})
	return videos
}
func eventually(t *testing.T, timeout time.Duration, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("condition did not recover before timeout")
}

func TestCacheStampedeNegativeLeaseAndStaleFallback(t *testing.T) {
	a := integrationApp(t)
	v := fixtureVideos(t, a, 1)[0]
	ctx := context.Background()
	// Separate App instances exercise the distributed lease as well as local merging.
	b := &App{cfg: a.cfg, db: a.db, redis: a.redis}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			app := a
			if i%2 == 1 {
				app = b
			}
			result, err := app.detail(ctx, v.ID)
			if err != nil || result.value.Video.ID != v.ID {
				t.Errorf("detail: %v %v", result, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if n := a.state().detailLoads.Load() + b.state().detailLoads.Load(); n != 1 {
		t.Fatalf("100 concurrent requests across 2 apps: %d database loads", n)
	}
	t.Log("100 concurrent requests across 2 App instances: 1 detail database load")
	missing := uint(4000000000)
	a.invalidateVideo(missing)
	for _, app := range []*App{a, b} {
		r, err := app.detail(ctx, missing)
		if err != nil || !r.value.Missing {
			t.Fatal("negative cache", err)
		}
	}
	if n := a.state().detailLoads.Load() + b.state().detailLoads.Load(); n != 2 {
		t.Fatal("negative cache did not avoid database read", n)
	}
	a.invalidateVideo(missing)
	key, lock := videoCacheKeys(v.ID)
	a.redis.Del(ctx, key)
	a.redis.Set(ctx, lock, "old-owner", time.Second)
	a.invalidateVideo(v.ID)
	n, err := publishVideoCache.Run(ctx, a.redis, []string{key, lock}, "old-owner", "{}", 1000).Int64()
	if err != nil || n != 0 {
		t.Fatal("revoked owner published stale data", n, err)
	}
	a.redis.Set(ctx, lock, "new-owner", time.Second)
	releaseVideoLock.Run(ctx, a.redis, []string{lock}, "old-owner")
	if a.redis.Get(ctx, lock).Val() != "new-owner" {
		t.Fatal("old owner deleted new lease")
	}
	a.redis.Del(ctx, lock)
	// Force a DB failure while a stale, positive L1 value is still available.
	deadDB, err := gorm.Open(mysql.New(mysql.Config{Conn: nil, DSN: os.Getenv("TEST_MYSQL_DSN"), SkipInitializeWithVersion: true}), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := deadDB.DB()
	sqlDB.Close()
	fallback := &App{cfg: a.cfg, db: deadDB}
	m := &fallback.state().videos
	m.put(v.ID, videoEnvelope{Video: v}, m.generation())
	m.mu.Lock()
	e := m.entries[v.ID]
	local := e.Value.(localVideo)
	local.fresh = time.Now().Add(-time.Second)
	e.Value = local
	m.mu.Unlock()
	result, err := fallback.detail(ctx, v.ID)
	if err != nil || !result.stale || result.value.Video.ID != v.ID {
		t.Fatal("stale fallback", result, err)
	}
	for i := 0; i < cap(fallback.state().dbSlots); i++ {
		fallback.state().dbSlots <- struct{}{}
	}
	if result, err = fallback.detail(ctx, v.ID); err != nil || !result.stale {
		t.Fatal("overload did not use stale value", err)
	}
	if _, err = fallback.detail(ctx, missing); err != errBusy {
		t.Fatal("overload did not reject an uncached request", err)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/healthz", nil)
	fallback.health(c)
	if w.Code != 503 {
		t.Fatal("database failure must fail readiness", w.Code)
	}
}

func TestNotificationStreamRefreshesWithoutRedis(t *testing.T) {
	a := integrationApp(t)
	proxy := newFaultProxy(t, a.redis.Options().Addr)
	r := testRedis(proxy.listener.Addr().String())
	defer r.Close()
	a.redis = r
	proxy.set(false)
	router := gin.New()
	router.GET("/stream", func(c *gin.Context) { c.Set("userID", uint(1)); a.notificationStream(c) })
	server := httptest.NewServer(router)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/stream", nil)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || !strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatal(response.Status)
	}
	scanner := bufio.NewScanner(response.Body)
	n := 0
	for scanner.Scan() {
		if scanner.Text() == "data: refresh" {
			n++
		}
		if n == 2 {
			return
		}
	}
	t.Fatalf("SSE did not refresh during outage: events=%d err=%v", n, scanner.Err())
}
