package main

import (
	"container/list"
	"context"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

var errBusy = errors.New("service_busy")
var errRedisOpen = errors.New("redis_circuit_open")

// A bounded single-flight group. Cancellation of a waiter never cancels work
// shared by other requests; each loader must impose its own timeout.
type flight struct {
	done  chan struct{}
	value any
	err   error
}
type flightGroup struct {
	mu    sync.Mutex
	calls map[string]*flight
}

func (g *flightGroup) do(ctx context.Context, key string, load func() (any, error)) (any, error, bool) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = make(map[string]*flight)
	}
	f, shared := g.calls[key]
	if !shared {
		if len(g.calls) >= 1024 {
			g.mu.Unlock()
			return nil, errBusy, false
		}
		f = &flight{done: make(chan struct{})}
		g.calls[key] = f
		go func() {
			f.value, f.err = load()
			g.mu.Lock()
			delete(g.calls, key)
			close(f.done)
			g.mu.Unlock()
		}()
	}
	g.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err(), shared
	case <-f.done:
		return f.value, f.err, shared
	}
}

type circuitBreaker struct {
	mu       sync.Mutex
	failures int
	until    time.Time
	probing  bool
}

func (b *circuitBreaker) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.until.IsZero() {
		return true
	}
	if now.Before(b.until) || b.probing {
		return false
	}
	b.probing = true
	return true
}
func (b *circuitBreaker) result(now time.Time, failed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probing = false
	if !failed {
		if !b.until.IsZero() {
			log.Print("redis circuit recovered")
		}
		b.failures = 0
		b.until = time.Time{}
		return
	}
	b.failures++
	if b.failures >= 3 {
		if b.until.IsZero() {
			log.Print("redis circuit open; using bounded fallback")
		}
		b.until = now.Add(2 * time.Second)
	}
}

type videoEnvelope struct {
	Video   Video `json:"video"`
	Missing bool  `json:"missing,omitempty"`
}
type localVideo struct {
	id           uint
	value        videoEnvelope
	fresh, stale time.Time
}
type videoMemory struct {
	mu      sync.Mutex
	entries map[uint]*list.Element
	order   *list.List
	epoch   uint64
}

func (m *videoMemory) get(id uint, stale bool) (videoEnvelope, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.entries[id]; e != nil {
		v := e.Value.(localVideo)
		until := v.fresh
		if stale {
			until = v.stale
		}
		if time.Now().Before(until) {
			m.order.MoveToFront(e)
			return v.value, true
		}
		if time.Now().After(v.stale) {
			m.order.Remove(e)
			delete(m.entries, id)
		}
	}
	return videoEnvelope{}, false
}
func (m *videoMemory) generation() uint64 { m.mu.Lock(); defer m.mu.Unlock(); return m.epoch }
func (m *videoMemory) put(id uint, value videoEnvelope, generation uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.epoch != generation {
		return
	}
	if m.entries == nil {
		m.entries = make(map[uint]*list.Element)
		m.order = list.New()
	}
	if e := m.entries[id]; e != nil {
		m.order.Remove(e)
		delete(m.entries, id)
	}
	now := time.Now()
	stale := now.Add(time.Minute)
	if value.Missing {
		stale = now.Add(time.Second)
	}
	m.entries[id] = m.order.PushFront(localVideo{id, value, now.Add(time.Second), stale})
	if m.order.Len() > 512 {
		e := m.order.Back()
		delete(m.entries, e.Value.(localVideo).id)
		m.order.Remove(e)
	}
}
func (m *videoMemory) invalidate(id uint) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.epoch++
	if e := m.entries[id]; e != nil {
		m.order.Remove(e)
		delete(m.entries, id)
	}
}

type localLimit struct {
	count   int64
	expires time.Time
}
type memoryLimiter struct {
	mu      sync.Mutex
	entries map[string]localLimit
}

func (l *memoryLimiter) take(key string, max int64, window time.Duration, now time.Time) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.entries == nil {
		l.entries = make(map[string]localLimit)
	}
	v, exists := l.entries[key]
	if !exists && len(l.entries) >= 10000 {
		for k, e := range l.entries {
			if !now.Before(e.expires) {
				delete(l.entries, k)
			}
		}
		if len(l.entries) >= 10000 {
			return false, errBusy
		}
	}
	if !now.Before(v.expires) {
		v = localLimit{expires: now.Add(window)}
	}
	v.count++
	l.entries[key] = v
	return v.count <= max, nil
}

type appRuntime struct {
	cfg                                                             Config
	flights                                                         flightGroup
	breaker                                                         circuitBreaker
	videos                                                          videoMemory
	limiter                                                         memoryLimiter
	dbSlots                                                         chan struct{}
	rankMu                                                          sync.Mutex
	ranks                                                           map[string]FeedSnapshot
	detailLoads, sharedLoads, staleReads, redisFailures, rankBuilds atomic.Uint64
}

func (a *App) state() *appRuntime {
	a.runtimeOnce.Do(func() {
		a.runtime = &appRuntime{cfg: a.cfg.defaults(), dbSlots: make(chan struct{}, 12), ranks: make(map[string]FeedSnapshot)}
	})
	return a.runtime
}
func (a *App) withRedis(ctx context.Context, fn func(context.Context) error) error {
	s := a.state()
	if a.redis == nil || !s.breaker.allow(time.Now()) {
		return errRedisOpen
	}
	callCtx, cancel := context.WithTimeout(ctx, s.cfg.RedisTimeout)
	defer cancel()
	err := fn(callCtx)
	failed := err != nil && !errors.Is(err, redis.Nil)
	s.breaker.result(time.Now(), failed)
	if failed {
		s.redisFailures.Add(1)
	}
	return err
}
func (a *App) dbSlot(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case a.state().dbSlots <- struct{}{}:
		return func() { <-a.state().dbSlots }, nil
	default:
		return nil, errBusy
	}
}
func (a *App) health(c *gin.Context) {
	sqlDB, err := a.db.DB()
	ctx, cancel := context.WithTimeout(c.Request.Context(), time.Second)
	defer cancel()
	if err != nil || sqlDB.PingContext(ctx) != nil {
		c.JSON(503, gin.H{"status": "unavailable", "database": "unavailable"})
		return
	}
	status, cache := "ok", "ok"
	if a.withRedis(c.Request.Context(), func(ctx context.Context) error { return a.redis.Ping(ctx).Err() }) != nil {
		status, cache = "degraded", "unavailable"
	}
	c.JSON(200, gin.H{"status": status, "database": "ok", "redis": cache})
}
