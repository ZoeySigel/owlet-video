package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	feed "github.com/ZoeySigel/owlet-video/backend/internal/application/feed"
	domainfeed "github.com/ZoeySigel/owlet-video/backend/internal/domain/feed"
)

func TestCatalogUsesRealCursorBoundariesAndExactOverlap(t *testing.T) {
	for _, videos := range []int{100, 1000, 10000} {
		for _, k := range []int{1, 5, 20} {
			c := makeCatalog(videos, 20, k)
			if len(c.Affected) != k {
				t.Fatalf("videos=%d K=%d actual=%d", videos, k, len(c.Affected))
			}
			seen := map[string]bool{}
			for _, p := range c.Pages {
				if seen[p.Cursor] {
					t.Fatal("duplicate page")
				}
				seen[p.Cursor] = true
				if len(p.IDs) != 20 {
					t.Fatalf("short fixture page: %v", p.IDs)
				}
				for i := 1; i < len(p.IDs); i++ {
					if p.IDs[i] != p.IDs[i-1]-1 {
						t.Fatal("non timeline order")
					}
				}
				if p.Cursor != "" && p.Cursor != cursorFor(p.IDs[0]+1) {
					t.Fatal("cursor is not a real preceding video")
				}
			}
		}
	}
}

func TestTraceIsPairedAndKeepsTheOfferedRate(t *testing.T) {
	for _, scenario := range []string{"read", "count", "card"} {
		a := makeTrace(42, scenario, 100, 10*time.Second, 200, 1000)
		b := makeTrace(42, scenario, 100, 10*time.Second, 200, 1000)
		if !reflect.DeepEqual(a, b) {
			t.Fatal("same seed did not replay same trace")
		}
		if len(a) != 1000 {
			t.Fatalf("planned=%d", len(a))
		}
		writes := 0
		for i, op := range a {
			if op.OffsetNS != int64(i)*int64(10*time.Millisecond) {
				t.Fatal("arrivals depend on request completion")
			}
			if op.Kind != "read" {
				writes++
			}
		}
		want := 0
		if scenario == "count" {
			want = 50
		}
		if scenario == "card" {
			want = 10
		}
		if writes != want {
			t.Fatalf("scenario %s writes=%d want=%d", scenario, writes, want)
		}
	}
}

func fixtureResponse(p pageSpec, viewer int64, users int) *feed.FeedResult {
	r := &feed.FeedResult{Scene: "timeline"}
	for _, id := range p.IDs {
		author := 1 + id%int64(users)
		v := &domainfeed.FeedItem{VideoID: id, AuthorID: author, AuthorNickname: fmt.Sprintf("User %d", author), AuthorAvatarURL: "/validation/avatar.png", Title: videoTitle(id, 0), Description: strings.TrimSpace(strings.Repeat("Cache validation metadata. ", 8)), MediaURL: fmt.Sprintf("/validation/%d.mp4", id), CoverURL: fmt.Sprintf("/validation/%d.jpg", id), LikeCount: 3, FavoriteCount: 1, PublishedAt: publishedAt(id), Favorited: viewer == author}
		for j := int64(0); j < 3; j++ {
			v.Liked = v.Liked || viewer == 1+(id+j)%int64(users)
		}
		r.Items = append(r.Items, v)
	}
	if len(p.IDs) > 0 {
		last := p.IDs[len(p.IDs)-1]
		r.NextCursor = cursorFor(last)
		r.HasMore = last > 1
	}
	return r
}

func TestIndependentOracleRejectsSharedWrongResponses(t *testing.T) {
	c := makeCatalog(1000, 20, 5)
	p := c.Pages[0]
	r := fixtureResponse(p, 1, 100)
	count, title := int64(3), int64(0)
	if err := verifyResult(r, p, 1, 100, c.Target, &count, &title); err != nil {
		t.Fatal(err)
	}
	r.Items = r.Items[1:]
	if err := verifyResult(r, p, 1, 100, c.Target, &count, &title); err == nil {
		t.Fatal("a missing target accepted even if both variants omit it")
	}
	r = fixtureResponse(p, 1, 100)
	r.Items[0], r.Items[1] = r.Items[1], r.Items[0]
	if err := verifyResult(r, p, 1, 100, c.Target, nil, nil); err == nil {
		t.Fatal("wrong ordering accepted")
	}
	r = fixtureResponse(p, 1, 100)
	r.Items[0].Liked = !r.Items[0].Liked
	if err := verifyResult(r, p, 1, 100, c.Target, nil, nil); err == nil {
		t.Fatal("user-state leakage accepted")
	}
}

func TestOracleMeasuresStalenessAndRejectsInventedFuture(t *testing.T) {
	c := makeCatalog(1000, 20, 1)
	r := fixtureResponse(c.Pages[0], 1, 100)
	now := time.Now()
	o := &oracle{target: c.Target}
	o.commit("count", 1, now.Add(-time.Second))
	stale, age, violation := o.inspect(r, now, now.Add(time.Millisecond))
	if !stale || age < time.Second || violation {
		t.Fatalf("stale=%v age=%v violation=%v", stale, age, violation)
	}
	r.Items[0].LikeCount = 999
	if _, _, violation = o.inspect(r, now, now); !violation {
		t.Fatal("invented future count accepted")
	}
	r.Items[0].LikeCount = 4
	r.Items[0].Title = videoTitle(c.Target, 999)
	if _, _, violation = o.inspect(r, now, now); !violation {
		t.Fatal("invented future title accepted")
	}
	r.Items[0].Title = videoTitle(c.Target, 0)
	o.count = []changeRecord{{Version: 1, At: now.Add(-16 * time.Second)}}
	r.Items[0].LikeCount = 3
	if _, _, violation = o.inspect(r, now, now); !violation {
		t.Fatal("15-second freshness violation accepted")
	}
}

func TestSummaryDoesNotApproveIncompleteMatrix(t *testing.T) {
	c := config{Output: t.TempDir(), Profile: "full", Rounds: 3, Rates: []int{20, 100}, Users: 1000, Videos: 10000, PageSize: 20}
	if err := writeSummary(c, nil, []runResult{{ID: "partial", Valid: true, ReadCount: 1, DurationSeconds: 60}}, []gateResult{{Name: "test", Passed: true}}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(c.Output, "summary.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "1 / 36") || !strings.Contains(string(b), "存在未通过或未完成") {
		t.Fatal("incomplete report presented as valid")
	}
}

func TestLoopbackAndOutputSafety(t *testing.T) {
	for _, addr := range []string{"example.com:6379", "0.0.0.0:6379", "10.0.0.1:6379", "127.0.0.1"} {
		if isLoopbackAddress(addr) {
			t.Fatalf("unsafe address accepted: %s", addr)
		}
	}
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := execute(config{Output: dir}); err == nil {
		t.Fatal("existing results accepted")
	}
	b, _ := os.ReadFile(sentinel)
	if string(b) != "keep" {
		t.Fatal("existing results changed")
	}
}
