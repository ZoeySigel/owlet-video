package main

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func TestWriteAdmissionConcurrentBudget(t *testing.T) {
	a := &App{cfg: Config{WriteRPS: 1, WriteBurst: 5}}
	a.state().writes.checked = time.Now() // Isolate rate accounting from SQL.
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, reason := a.admitWrite(context.Background())
			if reason == "" {
				accepted.Add(1)
				release()
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 5 {
		t.Fatalf("accepted=%d", accepted.Load())
	}
	if got := a.state().writes.view()["rejected"]; got != uint64(45) {
		t.Fatalf("rejected=%v", got)
	}
}

func TestWritePressureRejectsBeforePersistenceAndRecovers(t *testing.T) {
	base := integrationApp(t)
	// Roll back an isolated view of the shared test fixture; no production data.
	tx := base.db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	for _, table := range []string{"interaction_commands", "outboxes"} {
		if err := tx.Exec("DELETE FROM " + table).Error; err != nil {
			t.Fatal(err)
		}
	}
	a := &App{db: tx, cfg: Config{WriteMaxPending: 2, WriteMaxAge: time.Second}}
	refresh := func() string {
		a.state().writes.checked = time.Time{}
		return a.checkWritePressure(context.Background())
	}
	if reason := refresh(); reason != "" {
		t.Fatal(reason)
	}
	old := InteractionCommand{ID: uuid.NewString(), UserID: 1, Kind: "comment", CreatedAt: time.Now().Add(-time.Minute)}
	if err := tx.Create(&old).Error; err != nil {
		t.Fatal(err)
	}
	if reason := refresh(); reason != "write_backlog" {
		t.Fatal(reason)
	}
	r := gin.New()
	r.POST("/comments/:id", func(c *gin.Context) { c.Set("userID", uint(1)); a.interaction(c, "comment") })
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/comments/1", strings.NewReader(`{"body":"must not be persisted"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != 503 || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var n int64
	tx.Model(&InteractionCommand{}).Count(&n)
	if n != 1 {
		t.Fatalf("rejected request persisted: %d", n)
	}
	if err := tx.Delete(&old).Error; err != nil {
		t.Fatal(err)
	}
	if reason := refresh(); reason != "" {
		t.Fatal("did not recover", reason)
	}
	for i := 0; i < 2; i++ {
		if err := tx.Create(&Outbox{ID: uuid.NewString(), Kind: "test", Payload: []byte(`{}`)}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if reason := refresh(); reason != "write_backlog" {
		t.Fatal("outbox cap ignored", reason)
	}
	if err := tx.Exec("DELETE FROM outboxes").Error; err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	a.state().writes.checked = time.Time{}
	if reason := a.checkWritePressure(canceled); reason != "write_pressure_unavailable" {
		t.Fatal("DB failure did not close admission", reason)
	}
	if reason := refresh(); reason != "" {
		t.Fatal("DB recovery", reason)
	}
}
