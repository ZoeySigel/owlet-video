package main

import (
	"strconv"

	"github.com/gin-gonic/gin"
)

func paramID(c *gin.Context, key string) (uint, bool) {
	id, err := strconv.ParseUint(c.Param(key), 10, 64)
	if err != nil || id == 0 {
		errorJSON(c, 400, "invalid_id")
		return 0, false
	}
	return uint(id), true
}

func (a *App) likeVideo(c *gin.Context) { a.interaction(c, "like") }

func (a *App) unlikeVideo(c *gin.Context) { a.interaction(c, "unlike") }

func (a *App) isLiked(c *gin.Context) {
	id, ok := paramID(c, "id")
	if !ok {
		return
	}
	var n int64
	a.db.Model(&Like{}).Where("user_id = ? AND video_id = ?", currentID(c), id).Count(&n)
	c.JSON(200, gin.H{"liked": n > 0})
}

func (a *App) myLikes(c *gin.Context) {
	var videos []Video
	q := a.db.Preload("User").Where("id IN (?)", a.db.Model(&Like{}).Select("video_id").Where("user_id = ?", currentID(c))).Order("id DESC").Limit(30)
	if q.Find(&videos).Error != nil {
		errorJSON(c, 500, "query_failed")
		return
	}
	for i := range videos {
		a.hydrateVideo(&videos[i])
	}
	c.JSON(200, videos)
}

func (a *App) comments(c *gin.Context) {
	id, ok := paramID(c, "id")
	if !ok {
		return
	}
	var list []Comment
	q := a.db.Preload("User").Where("video_id = ?", id).Order("id ASC").Limit(50)
	if cursor := c.Query("cursor"); cursor != "" {
		q = q.Where("id > ?", cursor)
	}
	if q.Find(&list).Error != nil {
		errorJSON(c, 500, "query_failed")
		return
	}
	c.JSON(200, list)
}

func (a *App) createComment(c *gin.Context) { a.interaction(c, "comment") }

func (a *App) deleteComment(c *gin.Context) { a.interaction(c, "delete_comment") }

func (a *App) follow(c *gin.Context) { a.interaction(c, "follow") }

func (a *App) unfollow(c *gin.Context) { a.interaction(c, "unfollow") }

func (a *App) relationList(c *gin.Context, followers bool) {
	id, ok := paramID(c, "id")
	if !ok {
		return
	}
	var ids []uint
	q := a.db.Model(&Follow{}).Limit(100)
	if followers {
		q = q.Select("follower_id").Where("following_id = ?", id)
	} else {
		q = q.Select("following_id").Where("follower_id = ?", id)
	}
	if q.Scan(&ids).Error != nil {
		errorJSON(c, 500, "query_failed")
		return
	}
	list := []User{}
	if len(ids) > 0 {
		a.db.Where("id IN ?", ids).Find(&list)
	}
	c.JSON(200, list)
}
func (a *App) followers(c *gin.Context) { a.relationList(c, true) }
func (a *App) following(c *gin.Context) { a.relationList(c, false) }
