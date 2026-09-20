package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestCursorRoundTrip(t *testing.T) {
	for _, score := range []float64{0, 1, 3.75, 9999} {
		raw := makeCursor(score, 42)
		got, id, err := decodeCursor(raw)
		if err != nil || got != score || id != 42 {
			t.Fatalf("round trip: %v %v %v", got, id, err)
		}
	}
	if _, _, err := decodeCursor("garbage"); err == nil {
		t.Fatal("invalid cursor accepted")
	}
}

func TestJWTKindAndExpiry(t *testing.T) {
	a := &App{cfg: Config{JWTSecret: strings.Repeat("x", 32)}}
	token, err := a.signToken(7, uuid.NewString(), "access", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := a.parseToken(token, "access")
	if err != nil || claims.Subject != "7" {
		t.Fatalf("parse: %v", err)
	}
	if _, err := a.parseToken(token, "refresh"); err == nil {
		t.Fatal("accepted wrong token kind")
	}
}

func TestIntegrationFlow(t *testing.T) {
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN not set")
	}
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{TranslateError: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&User{}, &Invite{}, &Session{}, &Video{}, &Upload{}, &Like{}, &Comment{}, &Follow{}, &VideoTag{}, &Message{}, &Notification{}, &Outbox{}, &ProcessedEvent{}, &FeedSnapshot{}); err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: env("TEST_REDIS_ADDR", "127.0.0.1:6379"), Password: os.Getenv("TEST_REDIS_PASSWORD")})
	defer rdb.Close()
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
	cfg := Config{JWTSecret: strings.Repeat("x", 32), RabbitURL: os.Getenv("TEST_RABBITMQ_URL"), DataDir: t.TempDir(), MaxMediaBytes: 1 << 30}
	a := &App{cfg: cfg, db: db, redis: rdb}
	router := a.router()
	call := func(method, path string, body []byte, cookies []*http.Cookie, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		for _, cookie := range cookies {
			req.AddCookie(cookie)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	register := func() (User, []*http.Cookie) {
		code := uuid.NewString() + uuid.NewString()
		if err := db.Create(&Invite{CodeHash: shaHex(code), ExpiresAt: time.Now().Add(time.Hour)}).Error; err != nil {
			t.Fatal(err)
		}
		name := "u" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
		body, _ := json.Marshal(map[string]string{"username": name, "password": "correct-horse-battery-staple", "invite": code})
		w := call("POST", "/api/v1/auth/register", body, nil, nil)
		if w.Code != 201 {
			t.Fatalf("register %d: %s", w.Code, w.Body.String())
		}
		var u User
		if err := json.Unmarshal(w.Body.Bytes(), &u); err != nil {
			t.Fatal(err)
		}
		return u, w.Result().Cookies()
	}
	author, authorCookies := register()
	_, fanCookies := register()
	videoBytes := []byte{0, 0, 0, 20, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm', 0, 0, 0, 0, 0, 0, 0, 0}
	sum := md5.Sum(videoBytes)
	hash := hex.EncodeToString(sum[:])
	initBody, _ := json.Marshal(map[string]any{"md5": hash, "size": len(videoBytes), "chunks": 1})
	w := call("POST", "/api/v1/uploads", initBody, authorCookies, nil)
	if w.Code != 201 {
		t.Fatalf("init %d: %s", w.Code, w.Body.String())
	}
	var init struct{ ID string }
	_ = json.Unmarshal(w.Body.Bytes(), &init)
	w = call("PUT", "/api/v1/uploads/"+init.ID+"/chunks/0", videoBytes, authorCookies, map[string]string{"X-Chunk-MD5": hash})
	if w.Code != 200 {
		t.Fatalf("chunk %d: %s", w.Code, w.Body.String())
	}
	w = call("POST", "/api/v1/uploads/"+init.ID+"/complete", nil, authorCookies, nil)
	if w.Code != 200 {
		t.Fatalf("complete %d: %s", w.Code, w.Body.String())
	}
	publish, _ := json.Marshal(map[string]string{"uploadId": init.ID, "title": "North wind", "description": "A note from #north"})
	w = call("POST", "/api/v1/videos", publish, authorCookies, nil)
	if w.Code != 201 {
		t.Fatalf("publish %d: %s", w.Code, w.Body.String())
	}
	var video Video
	_ = json.Unmarshal(w.Body.Bytes(), &video)
	if video.ID == 0 || video.PlayURL == "" {
		t.Fatal("video missing id/url")
	}
	w = call("GET", "/api/v1/videos?sort=latest", nil, nil, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "North wind") {
		t.Fatalf("feed %d: %s", w.Code, w.Body.String())
	}
	w = call("PUT", fmt.Sprintf("/api/v1/videos/%d/like", video.ID), nil, fanCookies, nil)
	if w.Code != 200 {
		t.Fatalf("like %d: %s", w.Code, w.Body.String())
	}
	w = call("POST", fmt.Sprintf("/api/v1/videos/%d/comments", video.ID), []byte(`{"body":"Beautiful @`+author.Username+`"}`), fanCookies, nil)
	if w.Code != 201 {
		t.Fatalf("comment %d: %s", w.Code, w.Body.String())
	}
	w = call("PUT", fmt.Sprintf("/api/v1/users/%d/follow", author.ID), nil, fanCookies, nil)
	if w.Code != 200 {
		t.Fatalf("follow %d: %s", w.Code, w.Body.String())
	}
	w = call("POST", fmt.Sprintf("/api/v1/messages/%d", author.ID), []byte(`{"body":"hello"}`), fanCookies, nil)
	if w.Code != 201 {
		t.Fatalf("message %d: %s", w.Code, w.Body.String())
	}
	if cfg.RabbitURL != "" {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- a.runWorker(ctx) }()
		defer func() { cancel(); <-done }()
		deadline := time.Now().Add(12 * time.Second)
		for time.Now().Before(deadline) {
			var n int64
			db.Model(&Notification{}).Where("user_id = ?", author.ID).Count(&n)
			if n >= 3 {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		var n int64
		db.Model(&Notification{}).Where("user_id = ?", author.ID).Count(&n)
		if n < 3 {
			t.Fatalf("expected notifications from worker, got %d", n)
		}
		snapshot, err := a.latestRank(context.Background(), "hot")
		if err != nil {
			t.Fatal(err)
		}
		_, rankKey := snapshotKeys(snapshot.ID)
		if _, err := rdb.ZScore(context.Background(), rankKey, fmt.Sprintf("%020d", video.ID)).Result(); err != nil {
			t.Fatalf("hot ZSET missing video: %v", err)
		}
		var row Outbox
		if err := db.Where("kind = ?", "video.liked").Order("created_at DESC").First(&row).Error; err != nil {
			t.Fatal(err)
		}
		var eventCopy event
		if err := json.Unmarshal(row.Payload, &eventCopy); err != nil {
			t.Fatal(err)
		}
		before := n
		if err := a.processEvent(context.Background(), eventCopy); err != nil {
			t.Fatal(err)
		}
		db.Model(&Notification{}).Where("user_id = ?", author.ID).Count(&n)
		if n != before {
			t.Fatalf("duplicate event created notification: before=%d after=%d", before, n)
		}
		conn, err := amqp.Dial(cfg.RabbitURL)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		channel, err := conn.Channel()
		if err != nil {
			t.Fatal(err)
		}
		defer channel.Close()
		marker := uuid.NewString()
		if err := channel.PublishWithContext(context.Background(), "owlet.events", "events", false, false, amqp.Publishing{DeliveryMode: amqp.Persistent, MessageId: marker, Body: []byte("invalid-json")}); err != nil {
			t.Fatal(err)
		}
		found := false
		deadDeadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadDeadline) {
			delivery, ok, err := channel.Get("owlet.dead", false)
			if err != nil {
				t.Fatal(err)
			}
			if ok {
				_ = delivery.Ack(false)
				if delivery.MessageId == marker {
					found = true
					break
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		if !found {
			t.Fatal("rejected event did not reach dead letter queue")
		}
	}
}
