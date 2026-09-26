package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// The absolute lease starts BEFORE the database read. On a failed revocation
// cache write we wait out this lease before reporting success, so a recovered
// Redis cannot resurrect a revoked session, including across API instances.
const sessionLease = 5 * time.Second

var errInvalidSession = errors.New("invalid_session")

type cachedSession struct {
	Session Session
	Until   time.Time
}

func sessionKey(id string) string { return "auth:session:v1:" + id }
func (a *App) loadSession(ctx context.Context, id string) (Session, error) {
	var cached cachedSession
	var raw string
	err := a.withRedis(ctx, func(ctx context.Context) error {
		var e error
		raw, e = a.redis.Get(ctx, sessionKey(id)).Result()
		return e
	})
	if err == nil {
		if raw == "revoked" {
			return Session{}, errInvalidSession
		}
		if json.Unmarshal([]byte(raw), &cached) == nil && cached.Session.ID == id && time.Now().Before(cached.Until) {
			return cached.Session, nil
		}
	}
	until := time.Now().Add(sessionLease)
	var s Session
	dbCtx, cancel := context.WithTimeout(ctx, a.state().cfg.DBTimeout)
	defer cancel()
	if err := a.db.WithContext(dbCtx).First(&s, "id = ?", id).Error; err != nil {
		return s, err
	}
	if s.RevokedAt != nil || !time.Now().Before(s.ExpiresAt) {
		return s, errInvalidSession
	}
	rawBytes, _ := json.Marshal(cachedSession{s, until})
	if ttl := time.Until(until); ttl > 0 {
		_ = a.withRedis(ctx, func(ctx context.Context) error { return a.redis.SetNX(ctx, sessionKey(id), rawBytes, ttl).Err() })
	}
	return s, nil
}
func (a *App) retireSessions(ids []string) {
	deadline := time.Now().Add(sessionLease)
	failed := false
	for _, id := range ids {
		if a.withRedis(context.Background(), func(ctx context.Context) error {
			return a.redis.Set(ctx, sessionKey(id), "revoked", refreshTTL+accessTTL).Err()
		}) != nil {
			failed = true
		}
	}
	if failed {
		if wait := time.Until(deadline); wait > 0 {
			time.Sleep(wait)
		}
	}
}
func revokeSessions(tx *gorm.DB, uid uint) ([]string, error) {
	var ids []string
	if err := tx.Model(&Session{}).Where("user_id = ? AND revoked_at IS NULL", uid).Pluck("id", &ids).Error; err != nil {
		return nil, err
	}
	now := time.Now()
	if len(ids) > 0 {
		if err := tx.Model(&Session{}).Where("id IN ?", ids).Update("revoked_at", now).Error; err != nil {
			return nil, err
		}
	}
	return ids, nil
}
func accessCredential(c *gin.Context) (string, bool) {
	if header := c.GetHeader("Authorization"); header != "" {
		parts := strings.Fields(header)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			return parts[1], true
		}
		return "", true
	}
	raw, err := c.Cookie("owlet_access")
	return raw, err != http.ErrNoCookie
}
func (a *App) authenticate(c *gin.Context, optional bool) {
	raw, present := accessCredential(c)
	if !present && optional {
		c.Next()
		return
	}
	if !present {
		errorJSON(c, 401, "login_required")
		return
	}
	claims, err := a.parseToken(raw, "access")
	if err != nil {
		errorJSON(c, 401, "invalid_session")
		return
	}
	uid, err := strconv.ParseUint(claims.Subject, 10, 64)
	if err != nil || uid == 0 {
		errorJSON(c, 401, "invalid_session")
		return
	}
	s, err := a.loadSession(c.Request.Context(), claims.SessionID)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) && !errors.Is(err, errInvalidSession) {
		c.Header("Retry-After", "1")
		errorJSON(c, 503, "auth_unavailable")
		return
	}
	if err != nil || s.UserID != uint(uid) || s.RevokedAt != nil || !time.Now().Before(s.ExpiresAt) {
		errorJSON(c, 401, "invalid_session")
		return
	}
	c.Set("userID", uint(uid))
	c.Set("sessionID", s.ID)
	c.Next()
}
func (a *App) softAuth() gin.HandlerFunc { return func(c *gin.Context) { a.authenticate(c, true) } }
