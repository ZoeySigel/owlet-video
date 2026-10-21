package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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
	a := &App{cfg: Config{JWTSecret: strings.Repeat("x", 32)}, core: &coreServices{}}
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
