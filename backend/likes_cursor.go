package main

import (
	"encoding/base64"
	"encoding/json"

	"github.com/gin-gonic/gin"
)

type likesPosition struct {
	Likes int64 `json:"likes"`
	ID    uint  `json:"id"`
}

// Optional live keyset paging; the default signed snapshot mode remains stable
// even when like counts change between requests. This mode only breaks ties.
func (a *App) likesKeyset(c *gin.Context) {
	q := a.db.WithContext(c.Request.Context()).Preload("User").Model(&Video{})
	if raw := c.Query("cursor"); raw != "" {
		b, err := base64.RawURLEncoding.DecodeString(raw)
		var pos likesPosition
		if len(raw) > 128 || err != nil || json.Unmarshal(b, &pos) != nil || pos.ID == 0 || pos.Likes < 0 {
			errorJSON(c, 400, "invalid_cursor")
			return
		}
		q = q.Where("likes_count < ? OR (likes_count = ? AND id < ?)", pos.Likes, pos.Likes, pos.ID)
	}
	rows := []Video{}
	if q.Order("likes_count DESC, id DESC").Limit(21).Find(&rows).Error != nil {
		errorJSON(c, 503, "feed_failed")
		return
	}
	next := ""
	if len(rows) > 20 {
		rows = rows[:20]
		last := rows[len(rows)-1]
		b, _ := json.Marshal(likesPosition{last.LikesCount, last.ID})
		next = base64.RawURLEncoding.EncodeToString(b)
	}
	for i := range rows {
		a.hydrateVideo(&rows[i])
	}
	c.JSON(200, gin.H{"items": rows, "nextCursor": next})
}
