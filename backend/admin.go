package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	accountrepo "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/account"
	videorepo "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/video"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Always read permissions from the authority, rather than trusting a token or
// the session cache. Disabling an account takes effect for existing sessions.
func (a *App) accountAccess(parent context.Context, id uint) (accountrepo.UserModel, error) {
	ctx, cancel := context.WithTimeout(parent, a.state().cfg.DBTimeout)
	defer cancel()
	var u accountrepo.UserModel
	err := a.db.WithContext(ctx).Select("id", "role", "status").First(&u, id).Error
	return u, err
}

func (a *App) adminRoutes(r *gin.Engine) {
	g := r.Group("/api/v1/admin", a.auth(), func(c *gin.Context) {
		if c.GetString("userRole") != "admin" {
			errorJSON(c, 403, "admin_required")
			return
		}
		c.Next()
	})
	g.GET("/overview", a.adminOverview)
	g.GET("/users", a.adminUsers)
	g.PATCH("/users/:id", a.adminUpdateUser)
	g.POST("/users/:id/password", a.rateLimit("admin-password", 20, time.Hour), a.adminPassword)
	g.GET("/videos", a.adminVideos)
	g.PATCH("/videos/:id", a.adminUpdateVideo)
}

func adminPage(c *gin.Context) (int, bool) {
	p, err := strconv.Atoi(c.DefaultQuery("page", "1"))
	if err != nil || p < 1 || p > 100000 {
		errorJSON(c, 400, "invalid_page")
		return 0, false
	}
	return p, true
}

func adminQuery(c *gin.Context) (string, bool) {
	q := strings.TrimSpace(c.Query("q"))
	if len(q) > 160 {
		errorJSON(c, 400, "invalid_search")
		return "", false
	}
	// Treat user input as literal text, including LIKE wildcards.
	q = strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(q)
	return "%" + q + "%", true
}

func (a *App) adminOverview(c *gin.Context) {
	var result struct {
		Users           int64 `json:"users"`
		DisabledUsers   int64 `json:"disabledUsers"`
		Videos          int64 `json:"videos"`
		PublishedVideos int64 `json:"publishedVideos"`
		OfflineVideos   int64 `json:"offlineVideos"`
		MediaBytes      int64 `json:"mediaBytes"`
	}
	err := a.db.WithContext(c.Request.Context()).Raw(`SELECT
	 (SELECT COUNT(*) FROM account) AS users,
	 (SELECT COUNT(*) FROM account WHERE status<>1) AS disabled_users,
	 (SELECT COUNT(*) FROM video WHERE status<>4) AS videos,
	 (SELECT COUNT(*) FROM video WHERE status=2) AS published_videos,
	 (SELECT COUNT(*) FROM video WHERE status=3) AS offline_videos,
	 (SELECT COALESCE(SUM(size),0) FROM video) AS media_bytes`).Scan(&result).Error
	if err != nil {
		errorJSON(c, 503, "admin_unavailable")
		return
	}
	c.JSON(200, result)
}

type adminUser struct {
	ID         uint      `json:"id"`
	Username   string    `json:"username"`
	Bio        string    `json:"bio"`
	AvatarURL  string    `json:"avatarUrl"`
	Role       string    `json:"role"`
	Status     int       `json:"status"`
	CreatedAt  time.Time `json:"createdAt"`
	VideoCount int64     `json:"videoCount"`
}

func (a *App) adminUsers(c *gin.Context) {
	page, ok := adminPage(c)
	if !ok {
		return
	}
	q, ok := adminQuery(c)
	if !ok {
		return
	}
	query := a.db.WithContext(c.Request.Context()).Table("account AS a").Where("(a.account LIKE ? ESCAPE '!' OR CAST(a.id AS CHAR)=?)", q, c.Query("q"))
	if status := c.Query("status"); status != "" {
		if status != "0" && status != "1" {
			errorJSON(c, 400, "invalid_status")
			return
		}
		if status == "0" {
			query = query.Where("a.status<>1")
		} else {
			query = query.Where("a.status=1")
		}
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		errorJSON(c, 503, "admin_unavailable")
		return
	}
	items := []adminUser{}
	err := query.Select(`a.id,a.account AS username,a.bio,a.avatar_url,a.role,a.status,a.created_at,
	 (SELECT COUNT(*) FROM video v WHERE v.author_id=a.id AND v.status<>4) AS video_count`).Order("a.id DESC").Offset((page - 1) * 20).Limit(20).Scan(&items).Error
	if err != nil {
		errorJSON(c, 503, "admin_unavailable")
		return
	}
	c.JSON(200, gin.H{"items": items, "total": total, "page": page, "pageSize": 20})
}

func adminWriteError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		errorJSON(c, 404, "record_not_found")
	case errors.Is(err, gorm.ErrDuplicatedKey):
		errorJSON(c, 409, "username_taken")
	case errors.Is(err, errProtectedAdmin):
		errorJSON(c, 400, "protected_admin")
	default:
		log.Printf("admin write: %v", err)
		errorJSON(c, 503, "admin_unavailable")
	}
}

var errProtectedAdmin = errors.New("protected_admin")

func (a *App) adminUpdateUser(c *gin.Context) {
	id, ok := paramID(c, "id")
	if !ok {
		return
	}
	var body struct {
		Username *string `json:"username"`
		Bio      *string `json:"bio"`
		Status   *int    `json:"status"`
	}
	if c.ShouldBindJSON(&body) != nil || (body.Username == nil && body.Bio == nil && body.Status == nil) {
		errorJSON(c, 400, "invalid_request")
		return
	}
	updates := map[string]any{}
	if body.Username != nil {
		if !usernamePattern.MatchString(*body.Username) {
			errorJSON(c, 400, "invalid_username")
			return
		}
		updates["account"], updates["nickname"] = *body.Username, *body.Username
	}
	if body.Bio != nil {
		if len(*body.Bio) > 500 {
			errorJSON(c, 400, "invalid_profile")
			return
		}
		updates["bio"] = *body.Bio
	}
	if body.Status != nil {
		if *body.Status != 0 && *body.Status != 1 {
			errorJSON(c, 400, "invalid_status")
			return
		}
		updates["status"] = *body.Status
	}
	var revoked []string
	err := a.db.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		var u accountrepo.UserModel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&u, id).Error; err != nil {
			return err
		}
		if body.Status != nil && *body.Status != 1 && u.Role == "admin" {
			return errProtectedAdmin
		}
		if err := tx.Model(&u).Updates(updates).Error; err != nil {
			return err
		}
		if body.Username != nil || (body.Status != nil && *body.Status != 1) {
			var err error
			revoked, err = revokeSessions(tx, id)
			return err
		}
		return nil
	})
	if err != nil {
		adminWriteError(c, err)
		return
	}
	a.retireSessions(revoked)
	a.invalidateAuthor(id)
	log.Printf("admin actor=%d action=update_user target=%d", currentID(c), id)
	c.JSON(200, gin.H{"ok": true, "reauthenticate": id == currentID(c) && body.Username != nil})
}

func (a *App) adminPassword(c *gin.Context) {
	id, ok := paramID(c, "id")
	if !ok {
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if c.ShouldBindJSON(&body) != nil || len(body.Password) < 12 || len(body.Password) > 72 {
		errorJSON(c, 400, "invalid_password")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
	if err != nil {
		errorJSON(c, 503, "admin_unavailable")
		return
	}
	var revoked []string
	err = a.db.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		var u accountrepo.UserModel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&u, id).Error; err != nil {
			return err
		}
		if err := tx.Model(&u).Update("password", string(hash)).Error; err != nil {
			return err
		}
		var err error
		revoked, err = revokeSessions(tx, id)
		return err
	})
	if err != nil {
		adminWriteError(c, err)
		return
	}
	a.retireSessions(revoked)
	log.Printf("admin actor=%d action=reset_password target=%d", currentID(c), id)
	c.JSON(200, gin.H{"ok": true, "reauthenticate": id == currentID(c)})
}

type adminVideo struct {
	ID            uint      `json:"id"`
	UserID        uint      `json:"userId"`
	Username      string    `json:"username"`
	Title         string    `json:"title"`
	Description   string    `json:"description"`
	Status        int       `json:"status"`
	PlayURL       string    `json:"playUrl"`
	CoverURL      string    `json:"coverUrl"`
	Size          int64     `json:"size"`
	LikesCount    int64     `json:"likesCount"`
	CommentsCount int64     `json:"commentsCount"`
	CreatedAt     time.Time `json:"createdAt"`
}

func (a *App) adminVideos(c *gin.Context) {
	page, ok := adminPage(c)
	if !ok {
		return
	}
	q, ok := adminQuery(c)
	if !ok {
		return
	}
	query := a.db.WithContext(c.Request.Context()).Table("video AS v").Joins("LEFT JOIN account a ON a.id=v.author_id").Where("v.status<>4").Where("(v.title LIKE ? ESCAPE '!' OR a.account LIKE ? ESCAPE '!' OR CAST(v.id AS CHAR)=?)", q, q, c.Query("q"))
	if status := c.Query("status"); status != "" {
		if status != "2" && status != "3" {
			errorJSON(c, 400, "invalid_status")
			return
		}
		query = query.Where("v.status=?", status)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		errorJSON(c, 503, "admin_unavailable")
		return
	}
	items := []adminVideo{}
	err := query.Joins("LEFT JOIN video_stat s ON s.video_id=v.id").Select(`v.id,v.author_id AS user_id,a.account AS username,v.title,v.description,v.status,v.media_url AS play_url,v.cover_url,v.size,v.created_at,COALESCE(s.like_count,0) AS likes_count,COALESCE(s.comment_count,0) AS comments_count`).Order("v.id DESC").Offset((page - 1) * 20).Limit(20).Scan(&items).Error
	if err != nil {
		errorJSON(c, 503, "admin_unavailable")
		return
	}
	c.JSON(200, gin.H{"items": items, "total": total, "page": page, "pageSize": 20})
}

func (a *App) adminUpdateVideo(c *gin.Context) {
	id, ok := paramID(c, "id")
	if !ok {
		return
	}
	var body struct {
		Title       *string `json:"title"`
		Description *string `json:"description"`
		Status      *int    `json:"status"`
	}
	if c.ShouldBindJSON(&body) != nil || (body.Title == nil && body.Description == nil && body.Status == nil) {
		errorJSON(c, 400, "invalid_request")
		return
	}
	updates := map[string]any{}
	if body.Title != nil {
		v := strings.TrimSpace(*body.Title)
		if len(v) == 0 || len(v) > 160 {
			errorJSON(c, 400, "invalid_video")
			return
		}
		updates["title"] = v
	}
	if body.Description != nil {
		if len(*body.Description) > 2000 {
			errorJSON(c, 400, "invalid_video")
			return
		}
		updates["description"] = *body.Description
	}
	if body.Status != nil {
		if *body.Status != 2 && *body.Status != 3 {
			errorJSON(c, 400, "invalid_status")
			return
		}
		updates["status"] = *body.Status
	}
	err := a.db.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		var v videorepo.VideoModel
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("status<>4").First(&v, id).Error; err != nil {
			return err
		}
		if err := tx.Model(&v).Updates(updates).Error; err != nil {
			return err
		}
		if body.Title != nil {
			v.Title = strings.TrimSpace(*body.Title)
		}
		if body.Description != nil {
			v.Description = *body.Description
		}
		if body.Status != nil {
			v.Status = *body.Status
		}
		if body.Title != nil || body.Description != nil {
			if err := tx.Where("video_id=?", id).Delete(&VideoTag{}).Error; err != nil {
				return err
			}
			seen := map[string]bool{}
			for _, raw := range tagPattern.FindAllString(v.Title+" "+v.Description, -1) {
				tag := strings.ToLower(strings.TrimPrefix(raw, "#"))
				if seen[tag] {
					continue
				}
				seen[tag] = true
				if err := tx.Create(&VideoTag{VideoID: id, Tag: tag}).Error; err != nil {
					return err
				}
			}
		}
		if v.Status == 2 {
			return enqueue(tx, "video.published", map[string]any{"videoId": id, "userId": v.AuthorID})
		}
		return nil
	})
	if err != nil {
		adminWriteError(c, err)
		return
	}
	a.invalidateVideo(id)
	_ = a.withRedis(context.Background(), func(ctx context.Context) error { return a.redis.Del(ctx, fmt.Sprintf("video:card:v1:%d", id)).Err() })
	log.Printf("admin actor=%d action=update_video target=%d", currentID(c), id)
	c.JSON(200, gin.H{"ok": true})
}

func grantAdmin(db *gorm.DB, username string) error {
	var u accountrepo.UserModel
	if err := db.Where("account=? AND status=1", username).First(&u).Error; err != nil {
		return err
	}
	return db.Model(&u).Update("role", "admin").Error
}
