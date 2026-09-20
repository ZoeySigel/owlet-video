package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9_]{3,40}$`)

type tokenClaims struct {
	Kind      string `json:"kind"`
	SessionID string `json:"sid"`
	jwt.RegisteredClaims
}

func (a *App) signToken(userID uint, sessionID, kind string, ttl time.Duration) (string, error) {
	claims := tokenClaims{Kind: kind, SessionID: sessionID, RegisteredClaims: jwt.RegisteredClaims{Subject: strconv.FormatUint(uint64(userID), 10), IssuedAt: jwt.NewNumericDate(time.Now()), ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)), ID: uuid.NewString()}}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(a.cfg.JWTSecret))
}

func (a *App) parseToken(raw, kind string) (*tokenClaims, error) {
	claims := &tokenClaims{}
	parsed, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		if t.Method.Alg() != jwt.SigningMethodHS256.Alg() {
			return nil, errors.New("unexpected signing method")
		}
		return []byte(a.cfg.JWTSecret), nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	if err != nil || !parsed.Valid || claims.Kind != kind {
		return nil, errors.New("invalid token")
	}
	return claims, nil
}

func (a *App) setCookies(c *gin.Context, access, refresh string) {
	set := func(name, value, path string, maxAge int) {
		http.SetCookie(c.Writer, &http.Cookie{Name: name, Value: value, Path: path, MaxAge: maxAge, HttpOnly: true, Secure: a.cfg.SecureCookies, SameSite: http.SameSiteLaxMode})
	}
	set("owlet_access", access, "/api/v1", int(accessTTL.Seconds()))
	set("owlet_refresh", refresh, "/api/v1/auth", int(refreshTTL.Seconds()))
}

func (a *App) clearCookies(c *gin.Context) {
	a.setCookies(c, "", "")
	for _, name := range []string{"owlet_access", "owlet_refresh"} {
		path := "/api/v1"
		if name == "owlet_refresh" {
			path = "/api/v1/auth"
		}
		http.SetCookie(c.Writer, &http.Cookie{Name: name, Path: path, MaxAge: -1, HttpOnly: true, Secure: a.cfg.SecureCookies, SameSite: http.SameSiteLaxMode})
	}
}

func (a *App) newSession(c *gin.Context, userID uint) error {
	sid := uuid.NewString()
	access, err := a.signToken(userID, sid, "access", accessTTL)
	if err != nil {
		return err
	}
	refresh, err := a.signToken(userID, sid, "refresh", refreshTTL)
	if err != nil {
		return err
	}
	s := Session{ID: sid, UserID: userID, RefreshHash: shaHex(refresh), ExpiresAt: time.Now().Add(refreshTTL)}
	if err := a.db.Create(&s).Error; err != nil {
		return err
	}
	a.setCookies(c, access, refresh)
	return nil
}

func (a *App) auth() gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, err := c.Cookie("owlet_access")
		if err != nil {
			errorJSON(c, 401, "login_required")
			return
		}
		claims, err := a.parseToken(raw, "access")
		if err != nil {
			errorJSON(c, 401, "invalid_session")
			return
		}
		uid, err := strconv.ParseUint(claims.Subject, 10, 64)
		if err != nil {
			errorJSON(c, 401, "invalid_session")
			return
		}
		var s Session
		if a.db.Select("id", "user_id", "expires_at", "revoked_at").First(&s, "id = ?", claims.SessionID).Error != nil || s.UserID != uint(uid) || s.RevokedAt != nil || time.Now().After(s.ExpiresAt) {
			errorJSON(c, 401, "invalid_session")
			return
		}
		c.Set("userID", uint(uid))
		c.Set("sessionID", claims.SessionID)
		c.Next()
	}
}

func (a *App) sameOrigin() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead && c.Request.Method != http.MethodOptions {
			if raw := c.GetHeader("Origin"); raw != "" {
				u, err := url.Parse(raw)
				if err != nil || !strings.EqualFold(u.Host, c.Request.Host) {
					errorJSON(c, 403, "origin_mismatch")
					return
				}
			}
		}
		c.Next()
	}
}

var limitScript = redis.NewScript(`local n=redis.call('INCR',KEYS[1]); if n==1 then redis.call('PEXPIRE',KEYS[1],ARGV[1]) end; return n`)

func (a *App) rateLimit(key string, max int64, window time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		limitKey := "limit:" + key + ":" + c.ClientIP()
		// Track the same budget locally even while Redis is healthy. Falling back
		// therefore never starts a fresh local allowance in the middle of an outage.
		allowed, localErr := a.state().limiter.take(limitKey, max, window, time.Now())
		if localErr != nil {
			errorJSON(c, 503, "rate_limit_unavailable")
			return
		}
		if !allowed {
			errorJSON(c, 429, "rate_limited")
			return
		}
		var result int64
		err := a.withRedis(c.Request.Context(), func(ctx context.Context) error {
			var e error
			result, e = limitScript.Run(ctx, a.redis, []string{limitKey}, window.Milliseconds()).Int64()
			return e
		})
		if err == nil && result > max {
			errorJSON(c, 429, "rate_limited")
			return
		}
		c.Next()
	}
}

func (a *App) register(c *gin.Context) {
	var body struct{ Username, Password, Invite string }
	if c.ShouldBindJSON(&body) != nil || !usernamePattern.MatchString(body.Username) || len(body.Password) < 12 || len(body.Password) > 72 || len(body.Invite) < 20 {
		errorJSON(c, 400, "invalid_registration")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(body.Password), bcrypt.DefaultCost)
	if err != nil {
		errorJSON(c, 500, "password_error")
		return
	}
	var user User
	err = a.db.Transaction(func(tx *gorm.DB) error {
		var invite Invite
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("code_hash = ? AND used_by IS NULL AND expires_at > ?", shaHex(body.Invite), time.Now()).First(&invite).Error; err != nil {
			return err
		}
		user = User{Username: body.Username, PasswordHash: string(hash)}
		if err := tx.Create(&user).Error; err != nil {
			return err
		}
		return tx.Model(&invite).Update("used_by", user.ID).Error
	})
	if err != nil {
		errorJSON(c, 400, "invalid_invite_or_username")
		return
	}
	if a.newSession(c, user.ID) != nil {
		errorJSON(c, 500, "session_error")
		return
	}
	c.JSON(201, user)
}

func (a *App) login(c *gin.Context) {
	var body struct{ Username, Password string }
	if c.ShouldBindJSON(&body) != nil {
		errorJSON(c, 400, "invalid_request")
		return
	}
	var user User
	if a.db.Where("username = ?", body.Username).First(&user).Error != nil || bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(body.Password)) != nil {
		errorJSON(c, 401, "invalid_credentials")
		return
	}
	if a.newSession(c, user.ID) != nil {
		errorJSON(c, 500, "session_error")
		return
	}
	c.JSON(200, user)
}

func (a *App) refresh(c *gin.Context) {
	raw, err := c.Cookie("owlet_refresh")
	if err != nil {
		errorJSON(c, 401, "invalid_session")
		return
	}
	claims, err := a.parseToken(raw, "refresh")
	if err != nil {
		errorJSON(c, 401, "invalid_session")
		return
	}
	uid, err := strconv.ParseUint(claims.Subject, 10, 64)
	if err != nil {
		errorJSON(c, 401, "invalid_session")
		return
	}
	access, err := a.signToken(uint(uid), claims.SessionID, "access", accessTTL)
	if err != nil {
		errorJSON(c, 500, "session_error")
		return
	}
	refresh, err := a.signToken(uint(uid), claims.SessionID, "refresh", refreshTTL)
	if err != nil {
		errorJSON(c, 500, "session_error")
		return
	}
	err = a.db.Transaction(func(tx *gorm.DB) error {
		var s Session
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&s, "id = ?", claims.SessionID).Error; err != nil {
			return err
		}
		if s.UserID != uint(uid) || s.RevokedAt != nil || time.Now().After(s.ExpiresAt) || subtle.ConstantTimeCompare([]byte(s.RefreshHash), []byte(shaHex(raw))) != 1 {
			return errors.New("invalid refresh")
		}
		return tx.Model(&s).Updates(map[string]any{"refresh_hash": shaHex(refresh), "expires_at": time.Now().Add(refreshTTL)}).Error
	})
	if err != nil {
		a.clearCookies(c)
		errorJSON(c, 401, "invalid_session")
		return
	}
	a.setCookies(c, access, refresh)
	c.JSON(200, gin.H{"ok": true})
}

func (a *App) logout(c *gin.Context) {
	if raw, err := c.Cookie("owlet_refresh"); err == nil {
		if claims, e := a.parseToken(raw, "refresh"); e == nil {
			now := time.Now()
			_ = a.db.Model(&Session{}).Where("id = ?", claims.SessionID).Update("revoked_at", &now).Error
		}
	}
	a.clearCookies(c)
	c.JSON(200, gin.H{"ok": true})
}

func (a *App) me(c *gin.Context) {
	var u User
	if a.db.First(&u, currentID(c)).Error != nil {
		errorJSON(c, 404, "user_not_found")
		return
	}
	c.JSON(200, u)
}

func (a *App) updateMe(c *gin.Context) {
	var body struct {
		Bio      *string `json:"bio"`
		Username *string `json:"username"`
	}
	if c.ShouldBindJSON(&body) != nil {
		errorJSON(c, 400, "invalid_profile")
		return
	}
	updates := map[string]any{}
	if body.Bio != nil {
		if len(*body.Bio) > 500 {
			errorJSON(c, 400, "invalid_profile")
			return
		}
		updates["bio"] = *body.Bio
	}
	if body.Username != nil {
		if !usernamePattern.MatchString(*body.Username) {
			errorJSON(c, 400, "invalid_username")
			return
		}
		updates["username"] = *body.Username
	}
	if len(updates) == 0 {
		errorJSON(c, 400, "empty_update")
		return
	}
	if a.db.Model(&User{}).Where("id = ?", currentID(c)).Updates(updates).Error != nil {
		errorJSON(c, 409, "update_failed")
		return
	}
	a.me(c)
}

func (a *App) changePassword(c *gin.Context) {
	var body struct {
		OldPassword string `json:"oldPassword"`
		NewPassword string `json:"newPassword"`
	}
	if c.ShouldBindJSON(&body) != nil || len(body.NewPassword) < 12 || len(body.NewPassword) > 72 {
		errorJSON(c, 400, "invalid_password")
		return
	}
	var u User
	if a.db.First(&u, currentID(c)).Error != nil || bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(body.OldPassword)) != nil {
		errorJSON(c, 401, "invalid_credentials")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(body.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		errorJSON(c, 500, "password_error")
		return
	}
	if err := a.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&User{}).Where("id = ?", u.ID).Update("password_hash", string(hash)).Error; err != nil {
			return err
		}
		now := time.Now()
		return tx.Model(&Session{}).Where("user_id = ? AND id <> ?", u.ID, c.GetString("sessionID")).Update("revoked_at", &now).Error
	}); err != nil {
		errorJSON(c, 500, "password_error")
		return
	}
	c.JSON(200, gin.H{"ok": true})
}

func (a *App) userProfile(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		errorJSON(c, 400, "invalid_id")
		return
	}
	var u User
	if a.db.First(&u, id).Error != nil {
		errorJSON(c, 404, "user_not_found")
		return
	}
	var videos, followers, following, likes int64
	a.db.Model(&Video{}).Where("user_id = ?", id).Count(&videos)
	a.db.Model(&Follow{}).Where("following_id = ?", id).Count(&followers)
	a.db.Model(&Follow{}).Where("follower_id = ?", id).Count(&following)
	a.db.Model(&Like{}).Joins("JOIN videos ON videos.id = likes.video_id").Where("videos.user_id = ?", id).Count(&likes)
	c.JSON(200, gin.H{"user": u, "videos": videos, "followers": followers, "following": following, "likes": likes})
}
