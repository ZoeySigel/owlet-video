package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	embedding "github.com/ZoeySigel/owlet-video/backend/internal/application/embedding"
	exposure "github.com/ZoeySigel/owlet-video/backend/internal/application/exposure"
	feed "github.com/ZoeySigel/owlet-video/backend/internal/application/feed"
	interaction "github.com/ZoeySigel/owlet-video/backend/internal/application/interaction"
	message "github.com/ZoeySigel/owlet-video/backend/internal/application/message"
	playback "github.com/ZoeySigel/owlet-video/backend/internal/application/playback"
	recommendation "github.com/ZoeySigel/owlet-video/backend/internal/application/recommendation"
	relation "github.com/ZoeySigel/owlet-video/backend/internal/application/relation"
	video "github.com/ZoeySigel/owlet-video/backend/internal/application/video"
	domainfeed "github.com/ZoeySigel/owlet-video/backend/internal/domain/feed"
	cache "github.com/ZoeySigel/owlet-video/backend/internal/infra/cache"
	config "github.com/ZoeySigel/owlet-video/backend/internal/infra/config"
	metrics "github.com/ZoeySigel/owlet-video/backend/internal/infra/metrics"
	mq "github.com/ZoeySigel/owlet-video/backend/internal/infra/mq"
	accountrepo "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/account"
	embedrepo "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/embedding"
	exposurerepo "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/exposure"
	feedrepo "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/feed"
	interactionrepo "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/interaction"
	messagerepo "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/message"
	playbackrepo "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/playback"
	recommendationrepo "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/recommendation"
	relationrepo "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/relation"
	videorepo "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/video"
	exposurehttp "github.com/ZoeySigel/owlet-video/backend/internal/interfaces/http/exposure"
	feedhttp "github.com/ZoeySigel/owlet-video/backend/internal/interfaces/http/feed"
	messagehttp "github.com/ZoeySigel/owlet-video/backend/internal/interfaces/http/message"
	middleware "github.com/ZoeySigel/owlet-video/backend/internal/interfaces/http/middleware"
	playbackhttp "github.com/ZoeySigel/owlet-video/backend/internal/interfaces/http/playback"
	router "github.com/ZoeySigel/owlet-video/backend/internal/interfaces/http/router"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gorm.io/gorm"
)

type coreServices struct {
	feed        *feed.Service
	interaction *interaction.Service
	relation    *relation.Service
	message     *message.Service
	video       *video.Service
	exposure    *exposure.Service
	playback    *playback.Service
	cache       *cache.FeedCache
	mq          *mq.RabbitMQ
}

func (a *App) initCore() error {
	if err := coreReady(a.db); err != nil {
		return err
	}
	fc := cache.NewFeedCache(a.redis)
	fr := feedrepo.New(a.db)
	c := &coreServices{cache: fc}
	var err error
	c.mq, err = mq.NewRabbitMQ(config.RabbitMQConfig{URL: a.cfg.RabbitURL, InteractionExchange: "owlet.core.interaction", ActionChangedQueue: "owlet.core.action_changed", VideoExchange: "owlet.core.video", VideoPublishedQueue: "owlet.core.published", VideoEmbeddingQueue: "owlet.core.embedding", ExposureExchange: "owlet.core.exposure", ViewEventRecordedQueue: "owlet.core.view_events"})
	if err != nil {
		return fmt.Errorf("initialize core message broker: %w", err)
	}
	c.message = message.New(messagerepo.New(a.db))
	writer := router.NewMessageWriter(c.message)
	c.interaction = interaction.New(interactionrepo.New(a.db), interaction.WithHotScoreRecorder(fc), interaction.WithStatCache(fc), interaction.WithAsyncActionPipeline(fc, c.mq), interaction.WithMessageWriter(writer))
	c.relation = relation.New(relationrepo.New(a.db), relation.WithMessageWriter(writer), relation.WithFollowFeedBackfiller(router.NewFollowFeedBackfiller(fr, fc)))
	c.feed = feed.New(fr, feed.WithRecommender(recommendation.New(recommendationrepo.New(a.db))), feed.WithFeedCache(fc))
	c.video = video.New(videorepo.New(a.db), video.WithPublishedEventPublisher(c.mq))
	// Recommendation reads durable view events directly. The upstream optional
	// event publisher has no profile consumer in this deployment; leave it off
	// rather than accumulate an unconsumed queue indefinitely.
	c.exposure = exposure.New(exposurerepo.New(a.db))
	c.playback = playback.New(playbackrepo.New(a.db))
	a.core = c
	return nil
}

func (a *App) coreWorker(ctx context.Context) error {
	if err := interaction.NewActionWorker(interactionrepo.New(a.db), a.core.mq).Start(ctx); err != nil {
		return err
	}
	fr := feedrepo.New(a.db)
	if err := video.NewFanoutWorker(fr, a.core.mq, a.core.cache, video.NewFeedPreheater(fr, a.core.cache)).Start(ctx); err != nil {
		return err
	}
	if err := embedding.NewVideoEmbeddingWorker(embedding.New(embedrepo.New(a.db), nil), a.core.mq).Start(ctx); err != nil {
		return err
	}
	go func() {
		if err := metrics.RunServer(ctx, "127.0.0.1:9091"); err != nil {
			log.Printf("worker metrics: %v", err)
		}
	}()
	// Legacy outbox handles only retained extension events in core mode.
	return a.runWorker(ctx)
}

func (a *App) coreRoutes(r *gin.Engine) {
	r.Use(metrics.HTTPMiddleware())
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))
	r.GET("/api/v1/capabilities", func(c *gin.Context) {
		c.JSON(200, gin.H{"engine": "gcfeed", "recommendation": true, "favorites": true, "baseline": coreBaseline})
	})
	identity := func(c *gin.Context) {
		if id := a.optionalUser(c); id > 0 {
			c.Set(middleware.ContextUserIDKey, int64(id))
			c.Set(middleware.ContextRoleKey, "user")
		}
		c.Next()
	}
	public := r.Group("/api/v1", a.softAuth(), identity)
	fh := feedhttp.New(a.core.feed)
	public.GET("/feed-items", fh.ListFeedItems)
	public.POST("/feed-queries", fh.Query)
	private := r.Group("/api/v1", a.auth(), identity)
	eh := exposurehttp.New(a.core.exposure)
	private.POST("/video-view-events", eh.CreateViewEvent)
	ph := playbackhttp.New(a.core.playback)
	private.GET("/playback-config", ph.GetConfig)
	private.GET("/preload-videos", ph.ListPreloadVideos)
	private.POST("/playback-qos-reports", ph.CreateQoSReport)
	mh := messagehttp.New(a.core.message)
	private.GET("/message-center", mh.List)
	private.PATCH("/message-center", mh.MarkRead)
	private.GET("/message-stats/unread", mh.CountUnread)
	private.PUT("/videos/:id/favorite", func(c *gin.Context) { a.coreInteraction(c, "favorite") })
	private.DELETE("/videos/:id/favorite", func(c *gin.Context) { a.coreInteraction(c, "unfavorite") })
	private.GET("/videos/:id/favorite", a.coreIsFavorite)
	private.GET("/me/favorites", a.coreFavorites)
	private.DELETE("/videos/:id", func(c *gin.Context) {
		id, ok := paramID(c, "id")
		if !ok {
			return
		}
		if err := a.core.video.Delete(c.Request.Context(), int64(currentID(c)), int64(id)); err != nil {
			coreError(c, err)
			return
		}
		a.invalidateVideo(id)
		c.JSON(200, gin.H{"ok": true})
	})
}

func coreError(c *gin.Context, err error) {
	status := 503
	s := err.Error()
	if strings.Contains(s, "not found") {
		status = 404
	} else if strings.Contains(s, "permission") || strings.Contains(s, "forbidden") {
		status = 403
	} else if strings.Contains(s, "invalid") || strings.Contains(s, "empty") || strings.Contains(s, "too long") || strings.Contains(s, "required") {
		status = 400
	}
	if errors.Is(err, domainfeed.ErrViewerRequired) {
		status = 401
	}
	if status == 503 {
		log.Printf("core operation failed: %v", err)
		s = "operation_failed"
	}
	errorJSON(c, status, s)
}

func (a *App) coreFeed(c *gin.Context) {
	sort := c.DefaultQuery("sort", "latest")
	scene := domainfeed.Scene(sort)
	if sort == "latest" {
		scene = domainfeed.SceneTimeline
	}
	if (sort == "recommend" || sort == "following") && a.optionalUser(c) == 0 {
		errorJSON(c, 401, "login_required")
		return
	}
	result, err := a.core.feed.GetFeed(c.Request.Context(), feed.FeedRequest{Scene: scene, ViewerID: int64(a.optionalUser(c)), Cursor: c.Query("cursor"), Limit: 20, ClientContext: map[string]string{"request_id": c.GetHeader("X-Request-ID")}})
	if err != nil {
		coreError(c, err)
		return
	}
	items := make([]Video, 0, len(result.Items))
	for _, v := range result.Items {
		items = append(items, Video{ID: uint(v.VideoID), UserID: uint(v.AuthorID), User: User{ID: uint(v.AuthorID), Username: v.AuthorNickname, AvatarURL: v.AuthorAvatarURL}, Title: v.Title, Description: v.Description, PlayURL: v.MediaURL, CoverURL: v.CoverURL, LikesCount: int64(v.LikeCount), CommentsCount: int64(v.CommentCount), FavoritesCount: int64(v.FavoriteCount), PublishedAt: v.PublishedAt})
	}
	next := result.NextCursor
	if !result.HasMore {
		next = ""
	}
	c.JSON(200, gin.H{"items": items, "nextCursor": next})
}

func (a *App) coreInteraction(c *gin.Context, kind string) {
	id, ok := paramID(c, "id")
	if !ok {
		return
	}
	uid := int64(currentID(c))
	ctx := c.Request.Context()
	key := c.GetHeader("Idempotency-Key")
	switch kind {
	case "like", "unlike", "favorite", "unfavorite":
		var out *interaction.ActionResult
		var err error
		switch kind {
		case "like":
			out, err = a.core.interaction.Like(ctx, uid, int64(id), key)
		case "unlike":
			out, err = a.core.interaction.Unlike(ctx, uid, int64(id), key)
		case "favorite":
			out, err = a.core.interaction.Favorite(ctx, uid, int64(id), key)
		default:
			out, err = a.core.interaction.Unfavorite(ctx, uid, int64(id), key)
		}
		if err != nil {
			coreError(c, err)
			return
		}
		a.invalidateVideo(id)
		c.JSON(200, gin.H{"liked": out.Active, "favorited": out.Active, "likesCount": out.LikeCount, "favoritesCount": out.FavoriteCount})
	case "comment":
		var body struct {
			Body string `json:"body"`
		}
		if c.ShouldBindJSON(&body) != nil {
			errorJSON(c, 400, "invalid_comment")
			return
		}
		out, err := a.core.interaction.CreateComment(ctx, uid, int64(id), body.Body, key)
		if err != nil {
			coreError(c, err)
			return
		}
		var result Comment
		if err := a.db.WithContext(ctx).Preload("User").First(&result, out.Comment.ID).Error; err != nil {
			coreError(c, err)
			return
		}
		a.invalidateVideo(id)
		a.coreMentions(ctx, result)
		c.JSON(201, result)
	case "delete_comment":
		var old Comment
		_ = a.db.First(&old, id).Error
		if _, err := a.core.interaction.DeleteComment(ctx, int64(id), uid, "user"); err != nil {
			coreError(c, err)
			return
		}
		a.invalidateVideo(old.VideoID)
		c.JSON(200, gin.H{"ok": true})
	case "follow", "unfollow":
		var out *relation.FollowResult
		var err error
		if kind == "follow" {
			out, err = a.core.relation.Follow(ctx, uid, int64(id), key)
		} else {
			out, err = a.core.relation.Unfollow(ctx, uid, int64(id), key)
		}
		if err != nil {
			coreError(c, err)
			return
		}
		c.JSON(200, gin.H{"following": out.Following})
	default:
		errorJSON(c, 400, "invalid_action")
	}
}

func (a *App) coreMentions(ctx context.Context, v Comment) {
	seen := map[uint]bool{}
	for _, m := range mentionPattern.FindAllString(v.Body, -1) {
		var u User
		if a.db.WithContext(ctx).Where("username = ?", m[1:]).First(&u).Error == nil && u.ID != v.UserID && !seen[u.ID] {
			seen[u.ID] = true
			key := fmt.Sprintf("comment:%d:mention:%d", v.ID, u.ID)
			_ = a.db.WithContext(ctx).Where("user_id = ? AND event_id = ?", u.ID, key).FirstOrCreate(&messagerepo.MessageModel{UserID: int64(u.ID), ActorID: int64(v.UserID), VideoID: &v.VideoID, Type: "mention", Title: "提及", Content: "在评论中提到了你", EventID: &key}).Error
		}
	}
}

func (a *App) coreIsFavorite(c *gin.Context) {
	id, ok := paramID(c, "id")
	if !ok {
		return
	}
	var n int64
	err := a.db.Model(&interactionrepo.ActionModel{}).Where("user_id = ? AND video_id = ? AND action_type = 'favorite' AND status = 1", currentID(c), id).Count(&n).Error
	if err != nil {
		coreError(c, err)
		return
	}
	c.JSON(200, gin.H{"favorited": n > 0})
}
func (a *App) coreFavorites(c *gin.Context) {
	items := []Video{}
	err := a.db.Preload("User").Where("id IN (?)", a.db.Model(&interactionrepo.ActionModel{}).Select("video_id").Where("user_id = ? AND action_type = 'favorite' AND status=1", currentID(c))).Order("id DESC").Limit(100).Find(&items).Error
	if err != nil {
		coreError(c, err)
		return
	}
	for i := range items {
		a.hydrateVideo(&items[i])
	}
	c.JSON(200, items)
}

func (a *App) createCoreUser(tx *gorm.DB, u *User) error {
	m := accountrepo.UserModel{Account: u.Username, Nickname: u.Username, Password: u.PasswordHash, Bio: u.Bio, AvatarURL: u.AvatarURL, Status: 1, Role: "user"}
	if err := tx.Create(&m).Error; err != nil {
		return err
	}
	u.ID = uint(m.ID)
	u.CreatedAt = m.CreatedAt
	return nil
}

func (a *App) createCoreVideo(tx *gorm.DB, v *Video) error {
	m := videorepo.VideoModel{AuthorID: int64(v.UserID), Title: v.Title, Description: v.Description, MediaURL: mediaURL(v.FilePath), CoverURL: mediaURL(v.CoverPath), Status: 2, PublishedAt: &v.PublishedAt, FilePath: v.FilePath, CoverPath: v.CoverPath, Size: v.Size}
	if err := tx.Create(&m).Error; err != nil {
		return err
	}
	v.ID = uint(m.ID)
	v.CreatedAt = m.CreatedAt
	return tx.Create(&videorepo.VideoStatModel{VideoID: m.ID}).Error
}

func (a *App) publishCoreVideo(v Video) error {
	e := &video.PublishedEvent{EventID: fmt.Sprintf("owlet-video:%d", v.ID), VideoID: int64(v.ID), AuthorID: int64(v.UserID), Title: v.Title, Description: v.Description, MediaURL: mediaURL(v.FilePath), CoverURL: mediaURL(v.CoverPath), PublishedAt: v.PublishedAt, OccurredAt: time.Now()}
	return a.core.mq.PublishVideoPublished(context.Background(), e)
}
