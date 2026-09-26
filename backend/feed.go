package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

func (a *App) optionalUser(c *gin.Context) uint {
	if id, ok := c.Get("userID"); ok {
		return id.(uint)
	}
	return 0
}

func decodeCursor(raw string) (float64, uint, error) {
	if raw == "" {
		return 0, 0, nil
	}
	if len(raw) > 128 {
		return 0, 0, fmt.Errorf("invalid cursor")
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return 0, 0, err
	}
	parts := strings.Split(string(b), ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid cursor")
	}
	score, err := strconv.ParseFloat(parts[0], 64)
	if err != nil || math.IsNaN(score) || math.IsInf(score, 0) || score < 0 {
		return 0, 0, fmt.Errorf("invalid score")
	}
	id, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil || id == 0 {
		return 0, 0, fmt.Errorf("invalid id")
	}
	return score, uint(id), nil
}
func makeCursor(score float64, id uint) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%g:%d", score, id)))
}

func (a *App) feed(c *gin.Context) {
	sort := c.DefaultQuery("sort", "latest")
	if sort != "latest" && sort != "hot" && sort != "likes" && sort != "following" {
		errorJSON(c, 400, "invalid_sort")
		return
	}
	if sort == "likes" && c.Query("pagination") == "keyset" {
		a.likesKeyset(c)
		return
	}
	if sort == "hot" || sort == "likes" {
		a.rankedFeed(c, sort)
		return
	}
	_, id, err := decodeCursor(c.Query("cursor"))
	if err != nil {
		errorJSON(c, 400, "invalid_cursor")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), a.state().cfg.DBTimeout)
	defer cancel()
	uid := uint(0)
	if sort == "following" {
		uid = a.optionalUser(c)
		if uid == 0 {
			errorJSON(c, 401, "login_required")
			return
		}
	}
	videos, err := a.timeline(ctx, uid, id)
	if err != nil {
		errorJSON(c, 503, "feed_failed")
		return
	}
	next := ""
	if len(videos) > 20 {
		videos = videos[:20]
		last := videos[len(videos)-1]
		next = makeCursor(0, last.ID)
	}
	for i := range videos {
		a.hydrateVideo(&videos[i])
	}
	c.JSON(200, gin.H{"items": videos, "nextCursor": next})
}

func (a *App) tagFeed(c *gin.Context) {
	tag := strings.ToLower(c.Param("tag"))
	if len(tag) < 1 || len(tag) > 60 {
		errorJSON(c, 400, "invalid_tag")
		return
	}
	var videos []Video
	q := a.db.Preload("User").Where("id IN (?)", a.db.Model(&VideoTag{}).Select("video_id").Where("tag = ?", tag)).Order("id DESC").Limit(20)
	if cursor := c.Query("cursor"); cursor != "" {
		q = q.Where("id < ?", cursor)
	}
	if q.Find(&videos).Error != nil {
		errorJSON(c, 500, "feed_failed")
		return
	}
	for i := range videos {
		a.hydrateVideo(&videos[i])
	}
	c.JSON(200, videos)
}
