package main

import (
	"context"
	"log"
	"sync"
	"time"
)

// Rate and in-flight limits are per API process. Pressure is sampled from the
// shared durable database; its one-second cache is a soft, not exact, queue cap.
type writeAdmission struct {
	mu                             sync.Mutex
	slots                          chan struct{}
	tokens                         float64
	refilled                       time.Time
	checked                        time.Time
	lastAlert                      time.Time
	reason                         string
	pendingCommands, pendingOutbox int
	oldestSeconds                  float64
	rejected                       uint64
}

func (g *writeAdmission) view() map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	state := "ok"
	if g.checked.IsZero() || time.Since(g.checked) > 10*time.Second {
		state = "unknown"
	} else if g.reason != "" {
		state = "degraded"
	}
	return map[string]any{"status": state, "reason": g.reason, "checkedAt": g.checked,
		"pendingCommandsCapped": g.pendingCommands, "pendingOutboxCapped": g.pendingOutbox,
		"oldestSeconds": g.oldestSeconds, "rejected": g.rejected}
}

func (a *App) admitWrite(ctx context.Context) (func(), string) {
	g := &a.state().writes
	cfg := a.state().cfg
	g.mu.Lock()
	if g.slots == nil {
		g.slots = make(chan struct{}, 16)
	}
	now := time.Now()
	if g.refilled.IsZero() {
		g.tokens = float64(cfg.WriteBurst)
	} else {
		g.tokens = min(float64(cfg.WriteBurst), g.tokens+now.Sub(g.refilled).Seconds()*float64(cfg.WriteRPS))
	}
	g.refilled = now
	if g.tokens < 1 {
		g.rejected++
		g.mu.Unlock()
		return nil, "write_rate_limited"
	}
	g.tokens--
	select {
	case g.slots <- struct{}{}:
	default:
		g.rejected++
		g.mu.Unlock()
		return nil, "write_busy"
	}
	g.mu.Unlock()
	release := func() { <-g.slots }
	if reason := a.checkWritePressure(ctx); reason != "" {
		g.mu.Lock()
		g.rejected++
		g.mu.Unlock()
		release()
		return nil, reason
	}
	return release, ""
}

func (a *App) checkWritePressure(parent context.Context) string {
	g := &a.state().writes
	cfg := a.state().cfg
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.checked.IsZero() && time.Since(g.checked) < time.Second {
		return g.reason
	}
	ctx, cancel := context.WithTimeout(parent, min(cfg.DBTimeout, 500*time.Millisecond))
	defer cancel()
	var commands, outbox []time.Time
	err := a.db.WithContext(ctx).Model(&InteractionCommand{}).Where("completed_at IS NULL").Order("created_at").Limit(cfg.WriteMaxPending).Pluck("created_at", &commands).Error
	if err == nil {
		err = a.db.WithContext(ctx).Model(&Outbox{}).Where("published_at IS NULL").Order("created_at").Limit(cfg.WriteMaxPending).Pluck("created_at", &outbox).Error
	}
	now := time.Now()
	old := g.reason
	g.reason = ""
	g.checked = now
	g.pendingCommands, g.pendingOutbox = len(commands), len(outbox)
	g.oldestSeconds = 0
	for _, rows := range [][]time.Time{commands, outbox} {
		if len(rows) > 0 {
			g.oldestSeconds = max(g.oldestSeconds, now.Sub(rows[0]).Seconds())
		}
	}
	if err != nil {
		g.reason = "write_pressure_unavailable"
	} else if len(commands) >= cfg.WriteMaxPending || len(outbox) >= cfg.WriteMaxPending || g.oldestSeconds >= cfg.WriteMaxAge.Seconds() {
		g.reason = "write_backlog"
	}
	if old != g.reason || (g.reason != "" && now.Sub(g.lastAlert) >= 30*time.Second) {
		log.Printf("write_pressure reason=%q pending_commands_capped=%d pending_outbox_capped=%d oldest_seconds=%.1f rejected=%d", g.reason, len(commands), len(outbox), g.oldestSeconds, g.rejected)
		g.lastAlert = now
	}
	return g.reason
}

func (a *App) monitorWrites(ctx context.Context) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		a.checkWritePressure(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
