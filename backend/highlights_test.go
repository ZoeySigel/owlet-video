package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

func TestSoftAuthAndProfilerIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	a := &App{cfg: Config{JWTSecret: strings.Repeat("x", 32)}}
	r := gin.New()
	r.GET("/public", a.softAuth(), func(c *gin.Context) { c.Status(204) })
	for _, header := range []string{"", "Bearer invalid", "Basic invalid"} {
		req := httptest.NewRequest("GET", "/public", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		want := 401
		if header == "" {
			want = 204
		}
		if w.Code != want {
			t.Fatalf("header=%q: %d", header, w.Code)
		}
	}
	w := httptest.NewRecorder()
	profilerMux().ServeHTTP(w, httptest.NewRequest("GET", "/debug/pprof/goroutine?debug=1", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "goroutine") {
		t.Fatal("missing profiling handlers")
	}
	w = httptest.NewRecorder()
	a.router().ServeHTTP(w, httptest.NewRequest("GET", "/debug/pprof/", nil))
	if w.Code != 404 {
		t.Fatal("profiler exposed publicly")
	}
}

func sessionFixture(t *testing.T, a *App, uid uint) ([]*http.Cookie, string) {
	t.Helper()
	sid := uuid.NewString()
	access, err := a.signToken(uid, sid, "access", accessTTL)
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := a.signToken(uid, sid, "refresh", refreshTTL)
	if err != nil {
		t.Fatal(err)
	}
	s := Session{ID: sid, UserID: uid, RefreshHash: shaHex(refresh), ExpiresAt: time.Now().Add(refreshTTL)}
	if err = a.db.Create(&s).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.db.Delete(&Session{}, "id = ?", sid); a.redis.Del(context.Background(), sessionKey(sid)) })
	return []*http.Cookie{{Name: "owlet_access", Value: access}, {Name: "owlet_refresh", Value: refresh}}, sid
}
func callAPI(a *App, method, path, body string, cookies []*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	a.router().ServeHTTP(w, req)
	return w
}

func TestSessionCacheHealingRevocationAndRename(t *testing.T) {
	a := integrationApp(t)
	v := fixtureVideos(t, a, 1)[0]
	cookies, sid := sessionFixture(t, a, v.UserID)
	if w := callAPI(a, "GET", "/api/v1/auth/me", "", cookies); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if a.redis.Exists(context.Background(), sessionKey(sid)).Val() != 1 {
		t.Fatal("session cache not filled")
	}
	var sessionQueries atomic.Int64
	callback := "session-query-" + uuid.NewString()
	if err := a.db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "sessions" {
			sessionQueries.Add(1)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.db.Callback().Query().Remove(callback) })
	if _, err := a.loadSession(context.Background(), sid); err != nil {
		t.Fatal(err)
	}
	if sessionQueries.Load() != 0 {
		t.Fatal("warm auth cache still queried sessions")
	}
	a.redis.Del(context.Background(), sessionKey(sid))
	if _, err := a.loadSession(context.Background(), sid); err != nil {
		t.Fatal(err)
	}
	if sessionQueries.Load() != 1 {
		t.Fatal("missing auth cache did not query and heal")
	}
	w := callAPI(a, "PATCH", "/api/v1/me", `{"username":"renamed`+uuid.NewString()[:8]+`"}`, cookies)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := callAPI(a, "GET", "/api/v1/auth/me", "", cookies); w.Code != 401 {
		t.Fatal("old access token survived rename")
	}
	if w := callAPI(a, "POST", "/api/v1/auth/refresh", "", cookies); w.Code != 401 {
		t.Fatal("old refresh survived rename")
	}
	fresh := w.Result().Cookies()
	if w := callAPI(a, "GET", "/api/v1/auth/me", "", fresh); w.Code != 200 {
		t.Fatal("new rename session invalid", w.Body.String())
	}
	// Revoke while Redis is disconnected; its old cache must not resurrect.
	proxy := newFaultProxy(t, a.redis.Options().Addr)
	r := testRedis(proxy.listener.Addr().String())
	defer r.Close()
	a.redis = r
	if w := callAPI(a, "GET", "/api/v1/auth/me", "", fresh); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	proxy.set(false)
	if w := callAPI(a, "POST", "/api/v1/auth/logout", "", fresh); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	proxy.set(true)
	if w := callAPI(a, "GET", "/api/v1/auth/me", "", fresh); w.Code != 401 {
		t.Fatal("revoked session resurrected")
	}
}

func TestFollowingTimelineInvalidation(t *testing.T) {
	a := integrationApp(t)
	videos := fixtureVideos(t, a, 3)
	visitor := User{Username: "follow" + uuid.NewString()[:8], PasswordHash: "unused"}
	if err := a.db.Create(&visitor).Error; err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rows, err := a.timeline(ctx, visitor.ID, 0)
	if err != nil || len(rows) != 0 {
		t.Fatal("initial following", err)
	}
	for _, kind := range []string{"follow", "unfollow"} {
		cmd := InteractionCommand{ID: uuid.NewString(), UserID: visitor.ID, TargetID: videos[0].UserID, Kind: kind}
		if err := a.db.Create(&cmd).Error; err != nil {
			t.Fatal(err)
		}
		if err := a.executeInteraction(ctx, cmd.ID); err != nil {
			t.Fatal(err)
		}
		rows, err = a.timeline(ctx, visitor.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		if kind == "follow" {
			want = 3
		}
		if len(rows) != want {
			t.Fatalf("%s stale following cache: %d", kind, len(rows))
		}
	}
}

func TestLikesKeysetTies(t *testing.T) {
	a := integrationApp(t)
	v := fixtureVideos(t, a, 45)
	cursor := ""
	seen := map[uint]bool{}
	ordered := []uint{}
	for page := 0; page < 10; page++ {
		w := callAPI(a, "GET", "/api/v1/videos?sort=likes&pagination=keyset&cursor="+cursor, "", nil)
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		var result struct {
			Items      []Video
			NextCursor string
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		for _, row := range result.Items {
			if seen[row.ID] {
				t.Fatal("duplicate tie", row.ID)
			}
			seen[row.ID] = true
			if row.UserID == v[0].UserID {
				ordered = append(ordered, row.ID)
			}
		}
		cursor = result.NextCursor
		if cursor == "" {
			break
		}
	}
	if len(ordered) != len(v) {
		t.Fatal("missing tied rows", len(ordered))
	}
	for i, id := range ordered {
		if id != v[len(v)-1-i].ID {
			t.Fatal("tie not ordered by ID")
		}
	}
	if w := callAPI(a, "GET", "/api/v1/videos?sort=likes&pagination=keyset&cursor=bad", "", nil); w.Code != 400 {
		t.Fatal("invalid keyset accepted")
	}
}

func TestPasswordRevokesEverySession(t *testing.T) {
	a := integrationApp(t)
	v := fixtureVideos(t, a, 1)[0]
	hash, _ := bcrypt.GenerateFromPassword([]byte("old-password-123"), bcrypt.MinCost)
	if err := a.db.Model(&User{}).Where("id = ?", v.UserID).Update("password_hash", string(hash)).Error; err != nil {
		t.Fatal(err)
	}
	one, _ := sessionFixture(t, a, v.UserID)
	two, _ := sessionFixture(t, a, v.UserID)
	for _, cookies := range [][]*http.Cookie{one, two} {
		if w := callAPI(a, "GET", "/api/v1/auth/me", "", cookies); w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	w := callAPI(a, "PATCH", "/api/v1/auth/password", `{"oldPassword":"old-password-123","newPassword":"new-password-123"}`, one)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, cookies := range [][]*http.Cookie{one, two} {
		if w := callAPI(a, "GET", "/api/v1/auth/me", "", cookies); w.Code != 401 {
			t.Fatal("session survived password change")
		}
	}
}

func TestTimelineHotColdBoundaryAndRecovery(t *testing.T) {
	a := integrationApp(t)
	videos := fixtureVideos(t, a, 1005)
	ctx := context.Background()
	a.invalidateTimeline(0)
	rows, err := a.timeline(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 21 || rows[0].ID != videos[1004].ID {
		t.Fatal("hot head mismatch")
	}
	// Start in hot data and cross the 1000-entry floor into cold SQL data.
	before := videos[10].ID
	rows, err = a.timeline(ctx, 0, before)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := a.coldTimeline(ctx, 0, before, 21)
	if err != nil {
		t.Fatal(err)
	}
	ids := func(rows []Video) []uint {
		result := []uint{}
		for _, v := range rows {
			result = append(result, v.ID)
		}
		return result
	}
	if !reflect.DeepEqual(ids(rows), ids(expected)) {
		t.Fatalf("boundary: %v != %v", ids(rows), ids(expected))
	}
	epoch, _ := a.timelineEpoch(ctx, 0)
	key, _, _ := timelineKeys(0, epoch)
	a.redis.Del(ctx, key)
	rows, err = a.timeline(ctx, 0, 0)
	if err != nil || rows[0].ID != videos[1004].ID {
		t.Fatal("partial eviction skipped rows", err)
	}
}

func TestInteractionIdempotencyAndAccountLimits(t *testing.T) {
	a := integrationApp(t)
	videos := fixtureVideos(t, a, 2)
	uid := videos[0].UserID
	cmd := InteractionCommand{ID: uuid.NewString(), UserID: uid, TargetID: videos[0].ID, Kind: "comment", Body: "only once"}
	if err := a.db.Create(&cmd).Error; err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- a.executeInteraction(context.Background(), cmd.ID) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var n int64
	a.db.Model(&Comment{}).Where("video_id = ?", videos[0].ID).Count(&n)
	if n != 1 {
		t.Fatal("duplicate comment", n)
	}
	var v Video
	a.db.First(&v, videos[0].ID)
	if v.CommentsCount != 1 {
		t.Fatal("duplicate counter", v.CommentsCount)
	}
	cookies, _ := sessionFixture(t, a, uid)
	// Invalid requests consume the same per-account budget on the real routes.
	for i := 0; i < 11; i++ {
		w := callAPI(a, "POST", fmt.Sprintf("/api/v1/videos/%d/comments", v.ID), `{"body":""}`, cookies)
		want := 400
		if i == 10 {
			want = 429
		}
		if w.Code != want {
			t.Fatalf("comment limit %d: %d", i, w.Code)
		}
	}
	other := User{Username: "limit" + uuid.NewString()[:8], PasswordHash: "unused"}
	if err := a.db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	otherCookies, _ := sessionFixture(t, a, other.ID)
	if w := callAPI(a, "POST", fmt.Sprintf("/api/v1/videos/%d/comments", v.ID), `{"body":""}`, otherCookies); w.Code != 400 {
		t.Fatal("account budgets coupled by IP")
	}
}

func TestHotSnapshotScoresAndBucketDeduplication(t *testing.T) {
	a := integrationApp(t)
	v := fixtureVideos(t, a, 3)
	ctx := context.Background()
	now := time.Now().UTC()
	likes := []Like{{UserID: v[0].UserID, VideoID: v[0].ID, CreatedAt: now.Add(-time.Hour)}, {UserID: v[0].UserID, VideoID: v[1].ID, CreatedAt: now.Add(-25 * time.Hour)}}
	if err := a.db.Create(&likes).Error; err != nil {
		t.Fatal(err)
	}
	comment := Comment{UserID: v[0].UserID, VideoID: v[0].ID, Body: "bucket", CreatedAt: now.Add(-2 * time.Minute)}
	if err := a.db.Create(&comment).Error; err != nil {
		t.Fatal(err)
	}
	got, err := a.bucketRanking(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	scores := map[uint]int64{}
	for _, row := range got {
		scores[row.ID] = row.Score
	}
	for id, expected := range map[uint]int64{v[0].ID: 8, v[1].ID: 0, v[2].ID: 0} {
		if score, ok := scores[id]; !ok || score != expected {
			t.Fatalf("video %d: got %d (present=%v), want %d", id, score, ok, expected)
		}
	}
	e := event{ID: uuid.NewString(), Data: map[string]any{"videoId": float64(v[2].ID), "delta": float64(3), "occurredAt": float64(now.UnixMilli())}}
	for i := 0; i < 2; i++ {
		if err := a.updateHotBucket(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	key := fmt.Sprintf("hot:live:{events}:%d", now.Unix()/60)
	member := fmt.Sprintf("%020d", v[2].ID)
	if score := a.redis.ZScore(ctx, key, member).Val(); score != 3 {
		t.Fatal("duplicate hot delta", score)
	}
	e.ID = uuid.NewString()
	e.Data["delta"] = float64(-3)
	if err := a.updateHotBucket(ctx, e); err != nil {
		t.Fatal(err)
	}
	if score := a.redis.ZScore(ctx, key, member).Val(); score != 0 {
		t.Fatal("removal did not undo original bucket", score)
	}
}

func TestAsyncWorkerAndBrokerFallback(t *testing.T) {
	a := integrationApp(t)
	a.cfg.RankLimit = 174 // Keep worker snapshots separate from the 173-entry paging fixture.
	if a.cfg.RabbitURL == "" {
		t.Skip("TEST_RABBITMQ_URL not set")
	}
	v := fixtureVideos(t, a, 1)[0]
	cookies, _ := sessionFixture(t, a, v.UserID)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.runWorker(ctx) }()
	defer func() { cancel(); <-done }()
	request := httptest.NewRequest("POST", fmt.Sprintf("/api/v1/videos/%d/comments", v.ID), bytes.NewBufferString(`{"body":"async"}`))
	for _, c := range cookies {
		request.AddCookie(c)
	}
	request.Header.Set("Prefer", "respond-async")
	request.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.router().ServeHTTP(w, request)
	if w.Code != 202 {
		t.Fatalf("async: %d %s", w.Code, w.Body.String())
	}
	var accepted struct{ OperationID string }
	if err := json.Unmarshal(w.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, func() bool {
		return callAPI(a, "GET", "/api/v1/interactions/"+accepted.OperationID, "", cookies).Code == 200
	})
	// A separate API instance with an unavailable broker executes durably itself.
	fallback := &App{cfg: a.cfg, db: a.db, redis: a.redis}
	defer fallback.state().publisher.close()
	fallback.cfg.RabbitURL = "amqp://guest:guest@127.0.0.1:1/"
	w = callAPI(fallback, "PUT", fmt.Sprintf("/api/v1/videos/%d/like", v.ID), "", cookies)
	if w.Code != 200 || w.Header().Get("X-Interaction-Execution") != "fallback" {
		t.Fatalf("fallback: %d %s", w.Code, w.Body.String())
	}
	var count int64
	a.db.Model(&Like{}).Where("user_id = ? AND video_id = ?", v.UserID, v.ID).Count(&count)
	if count != 1 {
		t.Fatal("fallback lost write")
	}
}
