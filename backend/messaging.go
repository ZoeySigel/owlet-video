package main

import (
	"context"
	"fmt"
	"github.com/redis/go-redis/v9"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func (a *App) conversations(c *gin.Context) {
	uid := currentID(c)
	var messages []Message
	if a.db.Where("sender_id = ? OR recipient_id = ?", uid, uid).Order("id DESC").Limit(500).Find(&messages).Error != nil {
		errorJSON(c, 500, "query_failed")
		return
	}
	seen := map[uint]bool{}
	peers := []User{}
	for _, m := range messages {
		peer := m.SenderID
		if peer == uid {
			peer = m.RecipientID
		}
		if !seen[peer] {
			seen[peer] = true
			var u User
			if a.db.First(&u, peer).Error == nil {
				peers = append(peers, u)
			}
		}
	}
	c.JSON(200, peers)
}

func (a *App) thread(c *gin.Context) {
	peer, ok := paramID(c, "peer")
	if !ok {
		return
	}
	uid := currentID(c)
	var messages []Message
	if a.db.Where("(sender_id = ? AND recipient_id = ?) OR (sender_id = ? AND recipient_id = ?)", uid, peer, peer, uid).Order("id DESC").Limit(50).Find(&messages).Error != nil {
		errorJSON(c, 500, "query_failed")
		return
	}
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}
	c.JSON(200, messages)
}

func (a *App) sendMessage(c *gin.Context) {
	peer, ok := paramID(c, "peer")
	if !ok {
		return
	}
	uid := currentID(c)
	if peer == uid {
		errorJSON(c, 400, "invalid_recipient")
		return
	}
	var body struct {
		Body string `json:"body"`
	}
	if c.ShouldBindJSON(&body) != nil {
		errorJSON(c, 400, "invalid_message")
		return
	}
	body.Body = strings.TrimSpace(body.Body)
	if len(body.Body) == 0 || len(body.Body) > 2000 {
		errorJSON(c, 400, "invalid_message")
		return
	}
	var n int64
	a.db.Model(&Follow{}).Where("(follower_id = ? AND following_id = ?) OR (follower_id = ? AND following_id = ?)", uid, peer, peer, uid).Count(&n)
	if n == 0 {
		errorJSON(c, 403, "relationship_required")
		return
	}
	m := Message{SenderID: uid, RecipientID: peer, Body: body.Body}
	if a.db.Create(&m).Error != nil {
		errorJSON(c, 500, "send_failed")
		return
	}
	c.JSON(201, m)
}

func (a *App) notifications(c *gin.Context) {
	list := []Notification{}
	if a.db.Where("user_id = ?", currentID(c)).Order("id DESC").Limit(50).Find(&list).Error != nil {
		errorJSON(c, 500, "query_failed")
		return
	}
	c.JSON(200, list)
}
func (a *App) unreadCount(c *gin.Context) {
	var n int64
	a.db.Model(&Notification{}).Where("user_id = ? AND read_at IS NULL", currentID(c)).Count(&n)
	c.JSON(200, gin.H{"count": n})
}
func (a *App) markNotificationsRead(c *gin.Context) {
	var body struct {
		ID *uint `json:"id"`
	}
	if c.ShouldBindJSON(&body) != nil {
		errorJSON(c, 400, "invalid_request")
		return
	}
	q := a.db.Model(&Notification{}).Where("user_id = ? AND read_at IS NULL", currentID(c))
	if body.ID != nil {
		q = q.Where("id = ?", *body.ID)
	}
	now := time.Now()
	if q.Update("read_at", &now).Error != nil {
		errorJSON(c, 500, "update_failed")
		return
	}
	c.JSON(200, gin.H{"ok": true})
}

func (a *App) notificationStream(c *gin.Context) {
	channel := "notifications:" + strconv.FormatUint(uint64(currentID(c)), 10)
	var pubsub *redis.PubSub
	var messages <-chan *redis.Message
	subscribe := func() {
		p := a.redis.Subscribe(c.Request.Context(), channel)
		ctx, cancel := context.WithTimeout(c.Request.Context(), a.state().cfg.RedisTimeout)
		_, err := p.Receive(ctx)
		cancel()
		if err != nil {
			p.Close()
			return
		}
		pubsub = p
		messages = p.Channel()
	}
	subscribe()
	defer func() {
		if pubsub != nil {
			pubsub.Close()
		}
	}()
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return
	}
	refresh := func() { fmt.Fprint(c.Writer, "event: notification\ndata: refresh\n\n"); flusher.Flush() }
	refresh()
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-ticker.C:
			// The database remains authoritative; periodic refresh also repairs lost
			// Pub/Sub signals after reconnect, regardless of whether Redis is healthy.
			refresh()
			if pubsub == nil {
				subscribe()
			}
		case _, open := <-messages:
			if !open {
				pubsub.Close()
				pubsub = nil
				messages = nil
				continue
			}
			refresh()
		}
	}
}
