package main

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestFlightCancellationAndCoalescing(t *testing.T) {
	var group flightGroup
	var loads atomic.Int32
	entered, finish := make(chan struct{}), make(chan struct{})
	loader := func() (any, error) { loads.Add(1); close(entered); <-finish; return 42, nil }
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err, _ := group.do(ctx, "hot", loader); first <- err }()
	<-entered
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	group.mu.Lock()
	f := group.calls["hot"]
	group.mu.Unlock()
	for i := 0; i < 100; i++ {
		_, err, shared := group.do(ctx, "hot", loader)
		if !shared || !errors.Is(err, context.Canceled) {
			t.Fatal("canceled waiters must join without canceling shared work", err)
		}
	}
	close(finish)
	<-f.done
	if f.value != 42 || f.err != nil {
		t.Fatal(f.value, f.err)
	}
	if n := loads.Load(); n != 1 {
		t.Fatalf("100 callers performed %d loads", n)
	}
}

func TestFlightCapacityAndCacheFence(t *testing.T) {
	g := flightGroup{calls: make(map[string]*flight)}
	for i := 0; i < 1024; i++ {
		g.calls[fmt.Sprint(i)] = &flight{done: make(chan struct{})}
	}
	if _, err, _ := g.do(context.Background(), "overflow", nil); !errors.Is(err, errBusy) {
		t.Fatal(err)
	}
	var m videoMemory
	old := m.generation()
	m.invalidate(1)
	m.put(1, videoEnvelope{Video: Video{ID: 1}}, old)
	if _, ok := m.get(1, true); ok {
		t.Fatal("invalidated read repopulated cache")
	}
	for i := uint(1); i <= 513; i++ {
		m.put(i, videoEnvelope{Video: Video{ID: i}}, m.generation())
	}
	if _, ok := m.get(1, true); ok || len(m.entries) != 512 {
		t.Fatal("LRU bound failed")
	}
}

func TestRedisCircuitHalfOpenRecovery(t *testing.T) {
	var b circuitBreaker
	now := time.Now()
	for i := 0; i < 3; i++ {
		if !b.allow(now) {
			t.Fatal("opened early")
		}
		b.result(now, true)
	}
	if b.allow(now.Add(time.Second)) {
		t.Fatal("open circuit accepted call")
	}
	if !b.allow(now.Add(3*time.Second)) || b.allow(now.Add(3*time.Second)) {
		t.Fatal("half-open must allow one probe")
	}
	b.result(now.Add(3*time.Second), false)
	if !b.allow(now.Add(3 * time.Second)) {
		t.Fatal("did not recover")
	}
}

func TestLocalLimitBudgetAndBound(t *testing.T) {
	var l memoryLimiter
	now := time.Now()
	for i := 0; i < 10; i++ {
		ok, err := l.take("ip", 10, time.Hour, now)
		if !ok || err != nil {
			t.Fatal(i, err)
		}
	}
	if ok, _ := l.take("ip", 10, time.Hour, now); ok {
		t.Fatal("budget reset")
	}
	if ok, _ := l.take("ip", 10, time.Hour, now.Add(time.Hour)); !ok {
		t.Fatal("window did not expire")
	}
	for i := 0; i < 9999; i++ {
		_, _ = l.take(fmt.Sprint(i), 1, time.Hour, now)
	}
	if _, err := l.take("overflow", 1, time.Hour, now); !errors.Is(err, errBusy) {
		t.Fatal("limiter unbounded", err)
	}
	if ok, err := l.take("recovered", 1, time.Hour, now.Add(2*time.Hour)); !ok || err != nil {
		t.Fatal(err)
	}
}

func TestRankCursorIntegrity(t *testing.T) {
	a := &App{cfg: Config{JWTSecret: "test-only-signing-secret-with-32-characters"}}
	c := rankCursor{1, "hot", uuid.NewString(), 20, time.Now().Add(time.Minute).Unix()}
	raw := a.signRankCursor(c)
	if got, err := a.parseRankCursor(raw, "hot"); err != nil || got != c {
		t.Fatal(got, err)
	}
	if _, err := a.parseRankCursor(raw, "likes"); err == nil {
		t.Fatal("cross-sort cursor accepted")
	}
	if _, err := a.parseRankCursor(raw+"x", "hot"); err == nil {
		t.Fatal("tampered cursor accepted")
	}
	c.Expires = time.Now().Add(-time.Second).Unix()
	if _, err := a.parseRankCursor(a.signRankCursor(c), "hot"); !errors.Is(err, errCursorExpired) {
		t.Fatal(err)
	}
	c.Expires = time.Now().Add(time.Minute).Unix()
	c.Offset = 21
	if _, err := a.parseRankCursor(a.signRankCursor(c), "hot"); err == nil {
		t.Fatal("unaligned cursor accepted")
	}
}
