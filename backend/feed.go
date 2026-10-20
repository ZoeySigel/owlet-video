package main

import (
	"strings"

	"github.com/gin-gonic/gin"
)

func (a *App) optionalUser(c *gin.Context) uint {
	if id, ok := c.Get("userID"); ok {
		return id.(uint)
	}
	return 0
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

// Likes is an Owlet extension over GCFeed statistics; other scenes use GCFeed.
func (a *App) feed(c *gin.Context) {
	if c.Query("sort") == "likes" {
		a.likesKeyset(c)
		return
	}
	a.coreFeed(c)
}
