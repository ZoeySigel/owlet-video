package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

type App struct {
	cfg         Config
	db          *gorm.DB
	redis       *redis.Client
	runtimeOnce sync.Once
	runtime     *appRuntime
}

func main() {
	cfg := configFromEnv().defaults()
	if len(cfg.JWTSecret) < 32 || cfg.MySQLDSN == "" || cfg.RabbitURL == "" {
		log.Fatal("JWT_SECRET (32+ chars), MYSQL_DSN and RABBITMQ_URL are required")
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "tmp"), 0700); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "media"), 0750); err != nil {
		log.Fatal(err)
	}
	db, err := gorm.Open(mysql.Open(cfg.MySQLDSN), &gorm.Config{TranslateError: true})
	if err != nil {
		log.Fatal(err)
	}
	if err := db.AutoMigrate(&User{}, &Invite{}, &Session{}, &Video{}, &Upload{}, &Like{}, &Comment{}, &Follow{}, &VideoTag{}, &Message{}, &Notification{}, &Outbox{}, &ProcessedEvent{}, &FeedSnapshot{}); err != nil {
		log.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		log.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(16)
	sqlDB.SetMaxIdleConns(4)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, Password: cfg.RedisPassword, PoolSize: 8, DialTimeout: cfg.RedisTimeout, ReadTimeout: cfg.RedisTimeout, WriteTimeout: cfg.RedisTimeout, PoolTimeout: cfg.RedisTimeout, MaxRetries: -1, ContextTimeoutEnabled: true})
	app := &App{cfg: cfg, db: db, redis: rdb}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	defer sqlDB.Close()
	defer rdb.Close()
	mode := "api"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	switch mode {
	case "api":
		server := &http.Server{Addr: cfg.Addr, Handler: app.router(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
		go func() {
			<-ctx.Done()
			c, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			_ = server.Shutdown(c)
		}()
		log.Printf("API listening on %s", cfg.Addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	case "worker":
		if err := app.runWorker(ctx); err != nil && ctx.Err() == nil {
			log.Fatal(err)
		}
	case "invite":
		code := strings.ReplaceAll(uuid.NewString(), "-", "")
		invite := Invite{CodeHash: shaHex(code), ExpiresAt: time.Now().Add(14 * 24 * time.Hour)}
		if err := db.Create(&invite).Error; err != nil {
			log.Fatal(err)
		}
		fmt.Printf("One-time invite (expires %s): %s\n", invite.ExpiresAt.Format(time.RFC3339), code)
	case "cleanup":
		if err := app.cleanupUploads(); err != nil {
			log.Fatal(err)
		}
	case "seed":
		if os.Getenv("SEED_TEST_DATA") != "1" {
			log.Fatal("seed requires SEED_TEST_DATA=1")
		}
		if err := app.seedTestData(os.Args[2:]); err != nil {
			log.Fatal(err)
		}
	default:
		log.Fatal("mode must be api, worker, invite, cleanup, or seed")
	}
}

func shaHex(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func errorJSON(c *gin.Context, status int, code string) {
	c.AbortWithStatusJSON(status, gin.H{"error": code})
}
func currentID(c *gin.Context) uint { return c.MustGet("userID").(uint) }

func (a *App) router() *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery(), gin.Logger(), a.sameOrigin())
	_ = r.SetTrustedProxies([]string{"127.0.0.1", "::1"})
	r.GET("/livez", func(c *gin.Context) { c.JSON(200, gin.H{"status": "ok"}) })
	r.GET("/healthz", a.health)
	v := r.Group("/api/v1")
	v.POST("/auth/register", a.rateLimit("register", 20, time.Hour), a.register)
	v.POST("/auth/login", a.rateLimit("login", 60, time.Hour), a.login)
	v.POST("/auth/refresh", a.refresh)
	v.POST("/auth/logout", a.logout)
	v.GET("/auth/me", a.auth(), a.me)
	v.GET("/users/:id", a.userProfile)
	v.GET("/users/:id/videos", a.userVideos)
	v.GET("/videos", a.feed)
	v.GET("/videos/:id", a.videoDetail)
	v.GET("/videos/:id/comments", a.comments)
	v.GET("/tags/:tag/videos", a.tagFeed)
	v.GET("/users/:id/followers", a.followers)
	v.GET("/users/:id/following", a.following)
	auth := v.Group("")
	auth.Use(a.auth())
	auth.PATCH("/me", a.updateMe)
	auth.PATCH("/auth/password", a.changePassword)
	auth.POST("/me/avatar", a.uploadAvatar)
	auth.POST("/covers", a.uploadCover)
	auth.POST("/uploads", a.initUpload)
	auth.GET("/uploads/:id", a.uploadStatus)
	auth.PUT("/uploads/:id/chunks/:index", a.uploadChunk)
	auth.POST("/uploads/:id/complete", a.completeUpload)
	auth.POST("/videos", a.publishVideo)
	auth.PUT("/videos/:id/like", a.likeVideo)
	auth.GET("/videos/:id/like", a.isLiked)
	auth.DELETE("/videos/:id/like", a.unlikeVideo)
	auth.GET("/me/likes", a.myLikes)
	auth.POST("/videos/:id/comments", a.createComment)
	auth.DELETE("/comments/:id", a.deleteComment)
	auth.PUT("/users/:id/follow", a.follow)
	auth.DELETE("/users/:id/follow", a.unfollow)
	auth.GET("/messages", a.conversations)
	auth.GET("/messages/:peer", a.thread)
	auth.POST("/messages/:peer", a.sendMessage)
	auth.GET("/notifications", a.notifications)
	auth.GET("/notifications/unread", a.unreadCount)
	auth.PATCH("/notifications/read", a.markNotificationsRead)
	auth.GET("/notifications/stream", a.notificationStream)
	return r
}
