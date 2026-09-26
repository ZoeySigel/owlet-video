package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
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
	if err = db.AutoMigrate(&InteractionCommand{}, &User{}, &Video{}, &Like{}, &Comment{}, &Follow{}, &Notification{}, &Outbox{}, &ProcessedEvent{}, &FeedSnapshot{}, &Session{}); err != nil {
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
	app := &App{cfg: Config{JWTSecret: "test-only-signing-secret-with-32-characters", RabbitURL: os.Getenv("TEST_RABBITMQ_URL"), RankLimit: 173}.defaults(), db: db, redis: r}
	t.Cleanup(func() { app.state().publisher.close() })
	return app
}
func testRedis(addr string) *redis.Client {
	return redis.NewClient(&redis.Options{Addr: addr, Password: os.Getenv("TEST_REDIS_PASSWORD"), DialTimeout: 200 * time.Millisecond, ReadTimeout: 200 * time.Millisecond, WriteTimeout: 200 * time.Millisecond, PoolTimeout: 200 * time.Millisecond, ContextTimeoutEnabled: true, MaxRetries: -1})
}
func fixtureVideos(t *testing.T, a *App, n int) []Video {
	t.Helper()
	u := User{Username: "test" + uuid.NewString()[:12], PasswordHash: "not-a-login"}
	if err := a.db.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	videos := make([]Video, n)
	for i := range videos {
		videos[i] = Video{UserID: u.ID, Title: fmt.Sprintf("fixture-%d", i), FilePath: "test.mp4", PublishedAt: time.Now().Add(-time.Hour), LikesCount: 1000000}
	}
	if err := a.db.Create(&videos).Error; err != nil {
		t.Fatal(err)
	}
	ids := make([]uint, n)
	for i, v := range videos {
		ids[i] = v.ID
	}
	t.Cleanup(func() {
		a.db.Where("video_id IN ?", ids).Delete(&Like{})
		a.db.Where("video_id IN ?", ids).Delete(&Comment{})
		a.db.Where("user_id = ?", u.ID).Delete(&Notification{})
		a.db.Where("id IN ?", ids).Delete(&Video{})
		a.db.Delete(&u)
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

func TestRollingWindowAndRemoval(t *testing.T) {
	a := integrationApp(t)
	v := fixtureVideos(t, a, 3)
	now := time.Now().UTC().Truncate(time.Second)
	likes := []Like{{UserID: v[0].UserID, VideoID: v[0].ID, CreatedAt: now.Add(-23 * time.Hour)}, {UserID: v[1].UserID, VideoID: v[1].ID, CreatedAt: now.Add(-25 * time.Hour)}}
	comment := Comment{UserID: v[0].UserID, VideoID: v[0].ID, Body: "test", CreatedAt: now.Add(-time.Hour)}
	if err := a.db.Create(&likes).Error; err != nil {
		t.Fatal(err)
	}
	if err := a.db.Create(&comment).Error; err != nil {
		t.Fatal(err)
	}
	scores := func(at time.Time) map[uint]int64 {
		rows, err := a.computeRanking(context.Background(), "hot", at)
		if err != nil {
			t.Fatal(err)
		}
		m := map[uint]int64{}
		for _, r := range rows {
			m[r.ID] = r.Score
		}
		return m
	}
	if s := scores(now); s[v[0].ID] != 8 || s[v[1].ID] != 0 {
		t.Fatal(s)
	}
	if s := scores(now.Add(2 * time.Hour)); s[v[0].ID] != 5 {
		t.Fatalf("expired like still counted: %v", s)
	}
	if err := a.db.Delete(&comment).Error; err != nil {
		t.Fatal(err)
	}
	if s := scores(now); s[v[0].ID] != 3 {
		t.Fatal("deleted comment still counted", s)
	}
	if s := scores(now.Add(25 * time.Hour)); s[v[0].ID] != 0 {
		t.Fatal("window did not empty without new events", s)
	}
}

func TestSnapshotPagingSurvivesChangesAndRedisLoss(t *testing.T) {
	a := integrationApp(t)
	v := fixtureVideos(t, a, 55)
	proxy := newFaultProxy(t, a.redis.Options().Addr)
	r := testRedis(proxy.listener.Addr().String())
	defer r.Close()
	a.redis = r
	router := a.router()
	type page struct {
		Items      []Video
		NextCursor string
	}
	get := func(cursor string) (page, int) {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/videos?sort=likes&cursor="+url.QueryEscape(cursor), nil))
		var p page
		if w.Code == 200 {
			if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
				t.Fatal(err)
			}
		}
		return p, w.Code
	}
	p, status := get("")
	if status != 200 || len(p.Items) != 20 {
		t.Fatal(status, p)
	}
	snapshot, err := a.parseRankCursor(p.NextCursor, "likes")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.db.Where("id = ?", snapshot.Snapshot).Delete(&FeedSnapshot{}) })
	expected := make([]uint, 55)
	for i := range v {
		expected[i] = v[len(v)-1-i].ID
	}
	actual := []uint{}
	for _, row := range p.Items {
		actual = append(actual, row.ID)
	}
	if err := a.db.Model(&Video{}).Where("id = ?", v[0].ID).Update("likes_count", 2000000).Error; err != nil {
		t.Fatal(err)
	}
	proxy.set(false)
	for p.NextCursor != "" {
		p, status = get(p.NextCursor)
		if status != 200 {
			t.Fatal("fallback failed", status)
		}
		for _, row := range p.Items {
			if row.UserID == v[0].UserID {
				actual = append(actual, row.ID)
			}
		}
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("unstable page order: %v want %v", actual, expected)
	}
	// Remove only this test's cached snapshot; durable fallback must rebuild it.
	proxy.set(true)
	metadata, key := snapshotKeys(snapshot.Snapshot)
	direct := testRedis(env("TEST_REDIS_ADDR", "127.0.0.1:6379"))
	defer direct.Close()
	direct.Del(context.Background(), metadata, key)
	eventually(t, 5*time.Second, func() bool {
		return a.withRedis(context.Background(), func(ctx context.Context) error { return r.Ping(ctx).Err() }) == nil
	})
	p, status = get(a.signRankCursor(snapshot))
	if status != 200 || len(p.Items) != 20 {
		t.Fatal(status)
	}
	if n := direct.ZCard(context.Background(), key).Val(); n < 55 {
		t.Fatal("rank index did not reconstruct", n)
	}
	broken, _ := json.Marshal(FeedSnapshot{ID: uuid.NewString(), Sort: "likes", Entries: []byte("[]"), ExpiresAt: time.Now().Add(time.Minute)})
	if err := direct.Set(context.Background(), metadata, broken, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if got, err := a.snapshot(context.Background(), snapshot.Snapshot, "likes"); err != nil || got.ID != snapshot.Snapshot {
		t.Fatal("corrupt cache metadata prevented durable fallback", got.ID, err)
	}
	snapshot.Expires = time.Now().Add(-time.Second).Unix()
	if _, status = get(a.signRankCursor(snapshot)); status != 410 {
		t.Fatal("expiry status", status)
	}
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

func TestRedisOutagePreservesNotificationsAndRateLimits(t *testing.T) {
	a := integrationApp(t)
	v := fixtureVideos(t, a, 1)[0]
	proxy := newFaultProxy(t, a.redis.Options().Addr)
	r := testRedis(proxy.listener.Addr().String())
	defer r.Close()
	a.redis = r
	router := gin.New()
	router.GET("/limited", a.rateLimit("test-"+uuid.NewString(), 2, time.Minute), func(c *gin.Context) { c.Status(204) })
	router.GET("/healthz", a.health)
	request := func(path string) int {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w.Code
	}
	if s := request("/limited"); s != 204 {
		t.Fatal(s)
	}
	proxy.set(false)
	if s := request("/limited"); s != 204 {
		t.Fatal("fallback denied available budget", s)
	}
	if s := request("/limited"); s != 429 {
		t.Fatal("outage reset rate limit", s)
	}
	if s := request("/healthz"); s != 200 {
		t.Fatal("Redis outage made API unavailable", s)
	}
	e := event{ID: uuid.NewString(), Kind: "video.liked", Data: map[string]any{"userId": float64(v.UserID + 100000), "ownerId": float64(v.UserID), "videoId": float64(v.ID)}}
	defer a.db.Where("id = ?", e.ID).Delete(&ProcessedEvent{})
	for i := 0; i < 2; i++ {
		if err := a.processEvent(context.Background(), e); err != nil {
			t.Fatal("optional Redis failed durable event", err)
		}
	}
	var n int64
	a.db.Model(&Notification{}).Where("user_id = ?", v.UserID).Count(&n)
	if n != 1 {
		t.Fatal("notification lost or duplicated", n)
	}
	proxy.set(true)
	eventually(t, 5*time.Second, func() bool {
		return a.withRedis(context.Background(), func(ctx context.Context) error { return r.Ping(ctx).Err() }) == nil
	})
}

func TestWorkerReconnectDrainsOutbox(t *testing.T) {
	a := integrationApp(t)
	if a.cfg.RabbitURL == "" {
		t.Skip("TEST_RABBITMQ_URL not set")
	}
	endpoint, err := url.Parse(a.cfg.RabbitURL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := newFaultProxy(t, endpoint.Host)
	endpoint.Host = proxy.listener.Addr().String()
	a.cfg.RabbitURL = endpoint.String()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.runWorker(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(8 * time.Second):
			t.Error("worker did not stop")
		}
	}()
	marker := func() string {
		id := uuid.NewString()
		payload, _ := json.Marshal(event{ID: id, Kind: "test.recovery"})
		if err := a.db.Create(&Outbox{ID: id, Kind: "test.recovery", Payload: payload}).Error; err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { a.db.Where("id = ?", id).Delete(&Outbox{}); a.db.Where("id = ?", id).Delete(&ProcessedEvent{}) })
		return id
	}
	processed := func(id string) bool {
		var n int64
		a.db.Model(&ProcessedEvent{}).Where("id = ?", id).Count(&n)
		return n == 1
	}
	first := marker()
	eventually(t, 8*time.Second, func() bool { return processed(first) })
	proxy.set(false)
	second := marker()
	time.Sleep(300 * time.Millisecond)
	if processed(second) {
		t.Fatal("event bypassed unavailable broker")
	}
	proxy.set(true)
	eventually(t, 12*time.Second, func() bool { return processed(second) })
	eventually(t, time.Second, func() bool {
		var n int64
		a.db.Model(&Outbox{}).Where("id = ? AND published_at IS NOT NULL", second).Count(&n)
		return n == 1
	})
	t.Log("worker reconnected and consumed the event persisted during broker outage")
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
