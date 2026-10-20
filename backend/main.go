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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

type App struct {
	cfg         Config
	db          *gorm.DB
	redis       *redis.Client
	runtimeOnce sync.Once
	runtime     *appRuntime
	core        *coreServices
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "migrate-core" {
		apply := len(os.Args) > 2 && os.Args[2] == "apply"
		if err := migrateCore(context.Background(), apply); err != nil {
			log.Fatal(err)
		}
		return
	}
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
	db, err := openDatabase(cfg.MySQLDSN, &gorm.Config{TranslateError: true})
	if err != nil {
		log.Fatal(err)
	}
	if len(os.Args) > 1 && os.Args[1] == "init-core" {
		if err := initializeCoreDatabase(db); err != nil {
			log.Fatal(err)
		}
		sqlDB, _ := db.DB()
		_ = sqlDB.Close()
		return
	}
	err = coreReady(db)
	if err != nil {
		log.Fatal(err)
	}
	if len(os.Args) > 1 && os.Args[1] == "grant-admin" {
		if len(os.Args) != 3 {
			log.Fatal("usage: owlet-video grant-admin USERNAME")
		}
		if err := grantAdmin(db, os.Args[2]); err != nil {
			log.Fatal(err)
		}
		sqlDB, _ := db.DB()
		_ = sqlDB.Close()
		fmt.Println("Administrator access granted")
		return
	}
	sqlDB, err := db.DB()
	if err != nil {
		log.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(16)
	// Retain burst connections within the existing 16-connection ceiling.
	// A much smaller idle pool causes repeated authentication/SET NAMES under load.
	sqlDB.SetMaxIdleConns(16)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, Password: cfg.RedisPassword, DB: cfg.RedisDB, PoolSize: 8, DialTimeout: cfg.RedisTimeout, ReadTimeout: cfg.RedisTimeout, WriteTimeout: cfg.RedisTimeout, PoolTimeout: cfg.RedisTimeout, MaxRetries: -1, ContextTimeoutEnabled: true})
	app := &App{cfg: cfg, db: db, redis: rdb}
	if err := app.initCore(); err != nil {
		log.Fatal(err)
	}
	defer app.core.mq.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var coreDisconnected atomic.Bool
	go func() {
		select {
		case <-ctx.Done():
		case <-app.core.mq.Done():
			log.Print("core broker disconnected; shutting down for supervisor restart")
			coreDisconnected.Store(true)
			stop()
		}
	}()
	defer sqlDB.Close()
	defer rdb.Close()
	mode := "api"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	if mode == "api" || mode == "worker" {
		if err := startProfiler(ctx, mode); err != nil {
			log.Fatal(err)
		}
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
		if err := app.coreWorker(ctx); err != nil && ctx.Err() == nil {
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
	default:
		log.Fatal("mode must be api, worker, invite, or cleanup")
	}
	if coreDisconnected.Load() {
		log.Fatal("core broker connection lost")
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
	a.coreRoutes(r)
	a.adminRoutes(r)
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
	v.GET("/videos", a.softAuth(), a.feed)
	v.GET("/videos/:id", a.videoDetail)
	v.GET("/videos/:id/comments", a.comments)
	v.GET("/tags/:tag/videos", a.softAuth(), a.tagFeed)
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
	auth.PUT("/videos/:id/like", a.rateLimit("like", 30, time.Minute), a.likeVideo)
	auth.GET("/videos/:id/like", a.isLiked)
	auth.DELETE("/videos/:id/like", a.rateLimit("like", 30, time.Minute), a.unlikeVideo)
	auth.GET("/me/likes", a.myLikes)
	auth.POST("/videos/:id/comments", a.rateLimit("comment", 10, time.Minute), a.createComment)
	auth.DELETE("/comments/:id", a.rateLimit("comment", 10, time.Minute), a.deleteComment)
	auth.PUT("/users/:id/follow", a.rateLimit("follow", 20, time.Minute), a.follow)
	auth.DELETE("/users/:id/follow", a.rateLimit("follow", 20, time.Minute), a.unfollow)
	auth.GET("/messages", a.conversations)
	auth.GET("/messages/:peer", a.thread)
	auth.POST("/messages/:peer", a.sendMessage)
	auth.GET("/notifications", a.notifications)
	auth.GET("/notifications/unread", a.unreadCount)
	auth.PATCH("/notifications/read", a.markNotificationsRead)
	auth.GET("/notifications/stream", a.notificationStream)
	return r
}
