package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRankRefreshServesBoundedPreviousSnapshot(t *testing.T) {
	a := integrationApp(t)
	a.cfg.RankLimit = 181
	s := a.state()
	now := time.Now().UTC()
	previous := FeedSnapshot{ID: uuid.NewString(), Sort: "hot", Entries: []byte(`[]`), CreatedAt: now.Add(-s.cfg.RankRefresh), ExpiresAt: now.Add(10 * time.Minute)}
	s.rankMu.Lock()
	s.ranks["hot"] = previous
	s.rankMu.Unlock()
	db, _ := a.db.DB()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// A valid previous snapshot remains usable while rebuilding is unavailable.
	got, err := a.latestRank(context.Background(), "hot")
	if err != nil || got.ID != previous.ID {
		t.Fatalf("previous snapshot: %s %v", got.ID, err)
	}
	eventually(t, 5*time.Second, func() bool { s.rankMu.Lock(); defer s.rankMu.Unlock(); return !s.rankRefreshing["hot"] })
	// Cursor retention must not allow indefinitely stale first-page results.
	s.rankMu.Lock()
	previous.CreatedAt = now.Add(-3 * s.cfg.RankRefresh)
	s.ranks["hot"] = previous
	s.rankMu.Unlock()
	if _, err = a.latestRank(context.Background(), "hot"); err == nil {
		t.Fatal("served stale first page beyond refresh grace")
	}
	// Even a recent snapshot must never be served past its cursor expiry.
	s.rankMu.Lock()
	previous.CreatedAt = now
	previous.ExpiresAt = now.Add(-time.Second)
	s.ranks["hot"] = previous
	s.rankMu.Unlock()
	if _, err = a.latestRank(context.Background(), "hot"); err == nil {
		t.Fatal("served expired snapshot")
	}
}
