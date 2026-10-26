package test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	applicationfeed "github.com/ZoeySigel/owlet-video/backend/internal/application/feed"
	domainfeed "github.com/ZoeySigel/owlet-video/backend/internal/domain/feed"
)

type unavailableHotWindowCache struct{ *memoryFeedCache }

func (c unavailableHotWindowCache) ListHotWindowPage(context.Context, time.Time, int, int) ([]*domainfeed.FeedPageItem, error) {
	return nil, errors.New("redis unavailable")
}

func TestHotFeedFallback(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		name := "empty window"
		if unavailable {
			name = "unavailable window"
		}
		t.Run(name, func(t *testing.T) {
			repo := newMemoryFeedRepo(seedHotFeedItems())
			cache := newMemoryFeedCache()
			var feedCache applicationfeed.FeedCache = cache
			if unavailable {
				feedCache = unavailableHotWindowCache{cache}
			}
			router := newFeedRouterWithService(applicationfeed.New(repo, applicationfeed.WithFeedCache(feedCache)))
			response := performJSONRequest(router, http.MethodGet, "/api/feed-items?scene=hot&limit=2", "", "")
			requireStatus(t, response, http.StatusOK)
			var first feedAPIResponse
			decodeJSON(t, response, &first)
			if len(first.Items) != 2 || first.Items[0].VideoID != 1 || first.Items[1].VideoID != 2 || !first.HasMore || first.NextCursor == "" {
				t.Fatalf("unexpected fallback page: %+v", first)
			}

			// New interactions must not switch an existing fallback cursor to window ranking.
			cache.SetHotWindowItems([]*domainfeed.FeedPageItem{{VideoID: 2, HotScore: 100}})
			response = performJSONRequest(router, http.MethodGet, "/api/feed-items?scene=hot&limit=2&cursor="+first.NextCursor, "", "")
			requireStatus(t, response, http.StatusOK)
			var second feedAPIResponse
			decodeJSON(t, response, &second)
			if len(second.Items) != 1 || second.Items[0].VideoID != 3 || second.HasMore || repo.HotCalls() != 2 {
				t.Fatalf("unexpected fallback continuation: %+v, repo calls=%d", second, repo.HotCalls())
			}
		})
	}
}

func TestHotFeedEmptyRepository(t *testing.T) {
	router := newFeedRouterWithService(applicationfeed.New(newMemoryFeedRepo(nil), applicationfeed.WithFeedCache(newMemoryFeedCache())))
	response := performJSONRequest(router, http.MethodGet, "/api/feed-items?scene=hot&limit=2", "", "")
	requireStatus(t, response, http.StatusOK)
	var page feedAPIResponse
	decodeJSON(t, response, &page)
	if len(page.Items) != 0 || page.HasMore || page.NextCursor != "" {
		t.Fatalf("unexpected empty repository response: %+v", page)
	}
}

func TestHotFeedExhaustedWindowDoesNotRestart(t *testing.T) {
	repo := newMemoryFeedRepo(seedHotFeedItems())
	cache := newMemoryFeedCache()
	cache.SetHotWindowItems([]*domainfeed.FeedPageItem{{VideoID: 2, HotScore: 80}})
	router := newFeedRouterWithService(applicationfeed.New(repo, applicationfeed.WithFeedCache(cache)))
	response := performJSONRequest(router, http.MethodGet, "/api/feed-items?scene=hot&limit=2", "", "")
	requireStatus(t, response, http.StatusOK)
	var first feedAPIResponse
	decodeJSON(t, response, &first)
	if len(first.Items) != 1 || first.Items[0].VideoID != 2 || first.NextCursor == "" {
		t.Fatalf("unexpected window page: %+v", first)
	}
	cache.SetHotWindowItems(nil)
	response = performJSONRequest(router, http.MethodGet, "/api/feed-items?scene=hot&limit=2&cursor="+first.NextCursor, "", "")
	requireStatus(t, response, http.StatusOK)
	var second feedAPIResponse
	decodeJSON(t, response, &second)
	if len(second.Items) != 0 || second.HasMore || second.NextCursor != "" || repo.HotCalls() != 0 {
		t.Fatalf("exhausted window restarted ranking: %+v", second)
	}
}
