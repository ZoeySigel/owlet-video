package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	feedapp "github.com/ZoeySigel/owlet-video/backend/internal/application/feed"
	domainfeed "github.com/ZoeySigel/owlet-video/backend/internal/domain/feed"
	"golang.org/x/crypto/bcrypt"
)

func TestAdminPermissionsAndAccountLifecycle(t *testing.T) {
	a := integrationApp(t)
	owner := fixtureVideos(t, a, 1)[0].UserID
	target := fixtureVideos(t, a, 1)[0].UserID
	var ownerName string
	a.db.Table("account").Select("account").Where("id=?", owner).Scan(&ownerName)
	ownerCookies, _ := sessionFixture(t, a, owner)
	targetCookies, _ := sessionFixture(t, a, target)
	if w := callAPI(a, "GET", "/api/v1/admin/users", "", nil); w.Code != 401 {
		t.Fatalf("anonymous: %d", w.Code)
	}
	for _, path := range []string{"/overview", "/users", "/videos"} {
		if w := callAPI(a, "GET", "/api/v1/admin"+path, "", targetCookies); w.Code != 403 {
			t.Fatalf("nonadmin %s: %d", path, w.Code)
		}
	}
	if w := callAPI(a, "PATCH", fmt.Sprintf("/api/v1/admin/users/%d", target), `{"role":"admin","status":0}`, targetCookies); w.Code != 403 {
		t.Fatal("unprivileged write accepted")
	}
	if err := grantAdmin(a.db, ownerName); err != nil {
		t.Fatal(err)
	}
	if w := callAPI(a, "GET", "/api/v1/auth/me", "", ownerCookies); w.Code != 200 || !strings.Contains(w.Body.String(), `"isAdmin":true`) {
		t.Fatal("role change not observed", w.Body.String())
	}
	for _, path := range []string{"/overview", "/users", "/videos"} {
		w := callAPI(a, "GET", "/api/v1/admin"+path, "", ownerCookies)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "password") || strings.Contains(w.Body.String(), "refresh_hash") {
			t.Fatal("credential field exposed")
		}
	}
	if w := callAPI(a, "PATCH", fmt.Sprintf("/api/v1/admin/users/%d", owner), `{"status":0}`, ownerCookies); w.Code != 400 {
		t.Fatal("admin disabled itself", w.Body.String())
	}
	if w := callAPI(a, "GET", "/api/v1/admin/users?page=-1", "", ownerCookies); w.Code != 400 {
		t.Fatal("bad pagination accepted")
	}
	if w := callAPI(a, "GET", "/api/v1/admin/users?q=%25%27%20OR%201%3D1--", "", ownerCookies); w.Code != 200 || !strings.Contains(w.Body.String(), `"total":0`) {
		t.Fatal("literal search failed", w.Body.String())
	}
	// Warm session cache first, then disable. A cached access token must fail.
	if w := callAPI(a, "GET", "/api/v1/auth/me", "", targetCookies); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	path := fmt.Sprintf("/api/v1/admin/users/%d", target)
	if w := callAPI(a, "PATCH", path, `{"status":0}`, ownerCookies); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := callAPI(a, "GET", "/api/v1/auth/me", "", targetCookies); w.Code != 401 && w.Code != 403 {
		t.Fatal("disabled cached session accepted", w.Code)
	}
	if w := callAPI(a, "POST", "/api/v1/auth/refresh", "", targetCookies); w.Code != 403 {
		t.Fatal("disabled refresh accepted", w.Code)
	}
	if w := callAPI(a, "PATCH", path, `{"status":1,"bio":"管理员更新的资料"}`, ownerCookies); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := callAPI(a, "POST", path+"/password", `{"password":"short"}`, ownerCookies); w.Code != 400 {
		t.Fatal("short password accepted")
	}
	if w := callAPI(a, "POST", path+"/password", `{"password":"new-admin-test-password"}`, ownerCookies); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var record struct {
		Account, Password string
		Status            int
	}
	if err := a.db.Table("account").Where("id=?", target).Take(&record).Error; err != nil {
		t.Fatal(err)
	}
	if record.Status != 1 || bcrypt.CompareHashAndPassword([]byte(record.Password), []byte("new-admin-test-password")) != nil {
		t.Fatal("password or restore not persisted")
	}
	body, _ := json.Marshal(map[string]string{"username": record.Account, "password": "new-admin-test-password"})
	if w := callAPI(a, "POST", "/api/v1/auth/login", string(body), nil); w.Code != 200 {
		t.Fatal("reset password login failed", w.Body.String())
	}
	// Forging isAdmin on ordinary profile updates never grants permissions.
	if w := callAPI(a, "PATCH", path, `{"role":"admin"}`, ownerCookies); w.Code != 400 {
		t.Fatal("unknown-only patch accepted")
	}
}

func TestAdminVideoModerationWithWarmCaches(t *testing.T) {
	a := integrationApp(t)
	v := fixtureVideos(t, a, 1)[0]
	if err := a.db.Table("account").Where("id=?", v.UserID).Update("role", "admin").Error; err != nil {
		t.Fatal(err)
	}
	cookies, _ := sessionFixture(t, a, v.UserID)
	// Prime public detail, timeline page and card caches.
	if w := callAPI(a, "GET", fmt.Sprintf("/api/v1/videos/%d", v.ID), "", nil); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	request := feedapp.FeedRequest{Scene: domainfeed.SceneTimeline, Limit: 100}
	if _, err := a.core.feed.GetFeed(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/v1/admin/videos/%d", v.ID)
	if w := callAPI(a, "PATCH", path, `{"status":4}`, cookies); w.Code != 400 {
		t.Fatal("destructive status accepted")
	}
	if w := callAPI(a, "PATCH", path, `{"status":3}`, cookies); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	pendingID := fmt.Sprintf("moderated-publish-%d", v.ID)
	if err := a.processEvent(context.Background(), event{ID: pendingID, Kind: "video.published", Data: map[string]any{"videoId": float64(v.ID)}}); err != nil {
		t.Fatal("offline publication would be retried/dead-lettered", err)
	}
	t.Cleanup(func() { a.db.Delete(&ProcessedEvent{}, "id=?", pendingID) })
	if w := callAPI(a, "GET", fmt.Sprintf("/api/v1/videos/%d", v.ID), "", nil); w.Code != 404 {
		t.Fatal("offline detail served", w.Body.String())
	}
	page, err := a.core.feed.GetFeed(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range page.Items {
		if item.VideoID == int64(v.ID) {
			t.Fatal("offline video leaked through warm feed")
		}
	}
	w := callAPI(a, "GET", fmt.Sprintf("/api/v1/admin/videos?q=%d&status=3", v.ID), "", cookies)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":3`) {
		t.Fatal("offline video not manageable", w.Body.String())
	}
	if w := callAPI(a, "PATCH", path, `{"title":"更新的视频 #admin","description":"重新整理","status":2}`, cookies); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = callAPI(a, "GET", fmt.Sprintf("/api/v1/videos/%d", v.ID), "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "更新的视频") {
		t.Fatal("restore failed", w.Body.String())
	}
	var tags int64
	a.db.Model(&VideoTag{}).Where("video_id=? AND tag='admin'", v.ID).Count(&tags)
	if tags != 1 {
		t.Fatal("edited tags not rebuilt")
	}
	t.Cleanup(func() { a.db.Where("video_id=?", v.ID).Delete(&VideoTag{}) })
}
