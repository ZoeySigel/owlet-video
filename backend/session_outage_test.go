package main

import (
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestAuthDatabaseOutageIsRetryableAndBounded(t *testing.T) {
	// Accept TCP but never finish the MySQL handshake, exercising a stalled
	// dependency rather than only an immediate connection-refused error.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var peers []net.Conn
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			peers = append(peers, c)
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		<-done
		for _, c := range peers {
			c.Close()
		}
	})
	db, err := gorm.Open(mysql.New(mysql.Config{DSN: "test:test@tcp(" + ln.Addr().String() + ")/test", SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { sqlDB.Close() })
	a := &App{db: db, cfg: Config{JWTSecret: strings.Repeat("t", 32), DBTimeout: 75 * time.Millisecond}}
	token, err := a.signToken(1, "missing-session", "access", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	r.GET("/protected", a.auth(), func(c *gin.Context) { c.Status(204) })
	req := httptest.NewRequest("GET", "/protected", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	start := time.Now()
	r.ServeHTTP(w, req)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "auth_unavailable") || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if time.Since(start) > time.Second {
		t.Fatal("session query exceeded timeout budget")
	}
	w = httptest.NewRecorder()
	req.Header.Set("Authorization", "Bearer invalid")
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("invalid credential accepted: %d", w.Code)
	}
}
