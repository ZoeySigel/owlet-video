package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// A durable command and its outbox event are committed before publishing.
// Worker execution and timeout fallback lock this same row; results and all
// business writes commit together, so late/duplicate deliveries are harmless.
type InteractionCommand struct {
	ID           string `gorm:"primaryKey;size:36"`
	UserID       uint   `gorm:"index;not null"`
	TargetID     uint
	Kind         string `gorm:"size:30;not null"`
	Body         string `gorm:"size:1000"`
	Result       []byte `gorm:"type:blob"`
	Status       int
	EventID      string `gorm:"size:36"`
	EventPayload []byte `gorm:"-" json:"-"`
	CompletedAt  *time.Time
	CreatedAt    time.Time
}

type commandConnection struct {
	mu        sync.Mutex
	conn      *amqp.Connection
	ch        *amqp.Channel
	confirmed <-chan amqp.Confirmation
	returned  <-chan amqp.Return
}

func (p *commandConnection) reset() {
	if p.conn != nil {
		_ = p.conn.Close()
	}
	p.conn = nil
	p.ch = nil
}
func (p *commandConnection) close() { p.mu.Lock(); defer p.mu.Unlock(); p.reset() }

// Independent bounded lanes avoid serializing all requests on one confirm.
type commandPublisher struct {
	next  atomic.Uint64
	lanes [4]commandConnection
}

func (p *commandPublisher) close() {
	for i := range p.lanes {
		p.lanes[i].close()
	}
}

func (a *App) publishCommand(ctx context.Context, e event) (result error) {
	if a.cfg.RabbitURL == "" {
		return errors.New("broker unavailable")
	}
	pool := &a.state().publisher
	p := &pool.lanes[(pool.next.Add(1)-1)%uint64(len(pool.lanes))]
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	defer func() {
		if result != nil {
			p.reset()
		}
	}()
	if p.conn == nil || p.conn.IsClosed() {
		p.reset()
		var err error
		p.conn, err = dialBroker(ctx, a.cfg.RabbitURL)
		if err != nil {
			return err
		}
		p.ch, err = p.conn.Channel()
		if err != nil {
			return err
		}
		if err = declareBroker(p.ch); err != nil {
			return err
		}
		if err = p.ch.Confirm(false); err != nil {
			return err
		}
		p.confirmed = p.ch.NotifyPublish(make(chan amqp.Confirmation, 1))
		p.returned = p.ch.NotifyReturn(make(chan amqp.Return, 1))
	}
	conn := p.conn
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	ch, confirmed, returned := p.ch, p.confirmed, p.returned
	raw, _ := json.Marshal(e)
	if err := ch.PublishWithContext(ctx, "owlet.events", "events", true, false, amqp.Publishing{DeliveryMode: amqp.Persistent, ContentType: "application/json", MessageId: e.ID, Body: raw}); err != nil {
		return err
	}
	select {
	case <-returned:
		return errors.New("command unroutable")
	case ack := <-confirmed:
		if !ack.Ack {
			return errors.New("command rejected")
		}
		select {
		case <-returned:
			return errors.New("command unroutable")
		default:
			return nil
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *App) interaction(c *gin.Context, kind string) {
	target, ok := paramID(c, "id")
	if !ok {
		return
	}
	cmd := InteractionCommand{ID: uuid.NewString(), UserID: currentID(c), TargetID: target, Kind: kind}
	if kind == "comment" {
		var body struct {
			Body string `json:"body"`
		}
		if c.ShouldBindJSON(&body) != nil {
			errorJSON(c, 400, "invalid_comment")
			return
		}
		cmd.Body = strings.TrimSpace(body.Body)
		if len(cmd.Body) < 1 || len(cmd.Body) > 1000 {
			errorJSON(c, 400, "invalid_comment")
			return
		}
	}
	if (kind == "follow" || kind == "unfollow") && target == cmd.UserID {
		errorJSON(c, 400, "cannot_follow_self")
		return
	}
	e := event{ID: cmd.ID, Kind: "interaction.requested", Data: map[string]any{"commandId": cmd.ID}}
	raw, _ := json.Marshal(e)
	if err := a.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&cmd).Error; err != nil {
			return err
		}
		return tx.Create(&Outbox{ID: e.ID, Kind: e.Kind, Payload: raw}).Error
	}); err != nil {
		errorJSON(c, 503, "interaction_unavailable")
		return
	}
	work, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	publishCtx, stop := context.WithTimeout(work, time.Second)
	err := a.publishCommand(publishCtx, e)
	if err == nil {
		// Broker has confirmed the durable message. Avoid sending it again
		// through the Outbox scanner; update failure safely leaves a retry.
		_ = a.db.WithContext(publishCtx).Model(&Outbox{}).Where("id = ? AND published_at IS NULL", e.ID).Update("published_at", time.Now()).Error
	}
	stop()
	if err == nil && strings.EqualFold(c.GetHeader("Prefer"), "respond-async") {
		c.Header("Location", "/api/v1/interactions/"+cmd.ID)
		c.Header("Preference-Applied", "respond-async")
		c.JSON(202, gin.H{"operationId": cmd.ID, "status": "pending"})
		return
	}
	completed := false
	if err == nil {
		deadline := time.NewTimer(700 * time.Millisecond)
		pollInterval := 40 * time.Millisecond
		tick := time.NewTimer(pollInterval)
	wait:
		for {
			select {
			case <-deadline.C:
				break wait
			case <-tick.C:
				if a.db.WithContext(work).First(&cmd, "id = ?", cmd.ID).Error == nil && cmd.CompletedAt != nil {
					completed = true
					break wait
				}
				// Bound polling pressure while a command is still queued.
				pollInterval = min(2*pollInterval, 200*time.Millisecond)
				tick.Reset(pollInterval)
			case <-work.Done():
				break wait
			}
		}
		deadline.Stop()
		tick.Stop()
	}
	if !completed {
		if err = a.executeInteraction(work, cmd.ID); err != nil {
			errorJSON(c, 503, "interaction_pending")
			return
		}
		if a.db.WithContext(work).First(&cmd, "id = ?", cmd.ID).Error != nil {
			errorJSON(c, 503, "interaction_pending")
			return
		}
		c.Header("X-Interaction-Execution", "fallback")
	} else {
		c.Header("X-Interaction-Execution", "worker")
	}
	a.invalidateInteraction(cmd)
	if cmd.Status == 204 {
		c.Status(204)
	} else {
		c.Data(cmd.Status, "application/json; charset=utf-8", cmd.Result)
	}
}

func (a *App) interactionStatus(c *gin.Context) {
	if _, err := uuid.Parse(c.Param("id")); err != nil {
		errorJSON(c, 400, "invalid_id")
		return
	}
	var cmd InteractionCommand
	if a.db.Where("id = ? AND user_id = ?", c.Param("id"), currentID(c)).First(&cmd).Error != nil {
		errorJSON(c, 404, "operation_not_found")
		return
	}
	if cmd.CompletedAt == nil {
		c.JSON(202, gin.H{"operationId": cmd.ID, "status": "pending"})
		return
	}
	c.JSON(200, gin.H{"operationId": cmd.ID, "status": "completed", "httpStatus": cmd.Status, "result": json.RawMessage(cmd.Result)})
}

func (a *App) executeInteraction(ctx context.Context, id string) error {
	var cmd InteractionCommand
	err := a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&cmd, "id = ?", id).Error; err != nil {
			return err
		}
		if cmd.CompletedAt != nil {
			return nil
		}
		// Serialize commands from an actor, then lock the target before counter writes.
		var actor User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&actor, cmd.UserID).Error; err != nil {
			return err
		}
		result, status, err := applyInteraction(tx, &cmd, actor)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			status = 404
			result = gin.H{"error": "target_not_found"}
		} else if err != nil {
			return err
		}
		raw, err := json.Marshal(result)
		if err != nil {
			return err
		}
		now := time.Now()
		cmd.Result = raw
		cmd.Status = status
		cmd.CompletedAt = &now
		return tx.Model(&cmd).Updates(map[string]any{"result": raw, "status": status, "completed_at": now, "event_id": cmd.EventID}).Error
	})
	if err == nil {
		a.invalidateInteraction(cmd)
		// Apply heat directly as well as through the outbox. The Redis event ID
		// fence makes the two paths idempotent and covers broker downtime.
		if cmd.EventID != "" {
			payload := cmd.EventPayload
			if len(payload) == 0 {
				// A repeated command may need to repair an earlier cache failure.
				var row Outbox
				if a.db.WithContext(ctx).First(&row, "id = ?", cmd.EventID).Error == nil {
					payload = row.Payload
				}
			}
			if len(payload) > 0 {
				var e event
				if json.Unmarshal(payload, &e) == nil {
					_ = a.updateHotBucket(ctx, e)
				}
			}
		}
	}
	return err
}
func (a *App) invalidateInteraction(cmd InteractionCommand) {
	if cmd.Kind == "follow" || cmd.Kind == "unfollow" {
		a.invalidateTimeline(cmd.UserID)
		return
	}
	if cmd.Kind == "delete_comment" {
		var result struct {
			VideoID uint `json:"videoId"`
		}
		_ = json.Unmarshal(cmd.Result, &result)
		if result.VideoID > 0 {
			a.invalidateVideo(result.VideoID)
		}
	} else {
		a.invalidateVideo(cmd.TargetID)
	}
}
func applyInteraction(tx *gorm.DB, cmd *InteractionCommand, actor User) (any, int, error) {
	uid, id := cmd.UserID, cmd.TargetID
	if cmd.Kind == "follow" || cmd.Kind == "unfollow" {
		var target User
		if err := tx.First(&target, id).Error; err != nil {
			return nil, 0, err
		}
		if cmd.Kind == "follow" {
			row := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&Follow{FollowerID: uid, FollowingID: id})
			if row.Error != nil {
				return nil, 0, row.Error
			}
			if row.RowsAffected > 0 {
				if err := enqueue(tx, "user.followed", map[string]any{"userId": uid, "ownerId": id}); err != nil {
					return nil, 0, err
				}
			}
		} else {
			if err := tx.Where("follower_id = ? AND following_id = ?", uid, id).Delete(&Follow{}).Error; err != nil {
				return nil, 0, err
			}
		}
		return gin.H{"following": cmd.Kind == "follow"}, 200, nil
	}
	var comment Comment
	if cmd.Kind == "delete_comment" {
		if err := tx.Where("id = ? AND user_id = ?", id, uid).First(&comment).Error; err != nil {
			return nil, 0, err
		}
		id = comment.VideoID
	}
	var v Video
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&v, id).Error; err != nil {
		return nil, 0, err
	}
	data := map[string]any{"videoId": id, "userId": uid, "ownerId": v.UserID}
	// Match the DATETIME(3) persistence precision when returning the inserted row.
	occurred := time.Now().UTC().Truncate(time.Millisecond)
	var kind string
	var result any
	status := 200
	delta := 0
	column := ""
	switch cmd.Kind {
	case "like":
		like := Like{UserID: uid, VideoID: id, CreatedAt: occurred}
		row := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&like)
		if row.Error != nil {
			return nil, 0, row.Error
		}
		result = gin.H{"liked": true}
		if row.RowsAffected == 0 {
			return result, status, nil
		}
		kind = "video.liked"
		delta = 1
		column = "likes_count"
	case "unlike":
		var old Like
		if err := tx.Where("user_id = ? AND video_id = ?", uid, id).First(&old).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return gin.H{"liked": false}, 200, nil
		} else if err != nil {
			return nil, 0, err
		}
		occurred = old.CreatedAt
		row := tx.Delete(&old)
		if row.Error != nil {
			return nil, 0, row.Error
		}
		result = gin.H{"liked": false}
		if row.RowsAffected == 0 {
			return result, status, nil
		}
		kind = "video.unliked"
		delta = -1
		column = "likes_count"
	case "comment":
		comment = Comment{UserID: uid, VideoID: id, Body: cmd.Body, CreatedAt: occurred}
		if err := tx.Create(&comment).Error; err != nil {
			return nil, 0, err
		}
		// The actor was read under a row lock in this same transaction. Assign
		// after Create so GORM cannot issue an unnecessary association write.
		comment.User = actor
		result = comment
		status = 201
		kind = "video.commented"
		delta = 1
		column = "comments_count"
		data["body"] = cmd.Body
	case "delete_comment":
		occurred = comment.CreatedAt
		row := tx.Delete(&comment)
		if row.Error != nil {
			return nil, 0, row.Error
		}
		if row.RowsAffected == 0 {
			return gin.H{"videoId": id}, 204, nil
		}
		result = gin.H{"videoId": id}
		status = 204
		kind = "video.comment_deleted"
		delta = -1
		column = "comments_count"
	default:
		return nil, 0, fmt.Errorf("invalid interaction kind: %s", cmd.Kind)
	}
	weight := 3
	if column == "comments_count" {
		weight = 5
	}
	if err := tx.Model(&Video{}).Where("id = ?", id).Updates(map[string]any{column: gorm.Expr("GREATEST("+column+" + ?,0)", delta), "popularity": gorm.Expr("GREATEST(popularity + ?,0)", delta*weight)}).Error; err != nil {
		return nil, 0, err
	}
	data["delta"] = delta * weight
	data["occurredAt"] = occurred.UnixMilli()
	e := event{ID: uuid.NewString(), Kind: kind, Data: data}
	raw, err := json.Marshal(e)
	if err != nil {
		return nil, 0, err
	}
	cmd.EventID = e.ID
	cmd.EventPayload = raw
	if err := tx.Create(&Outbox{ID: e.ID, Kind: kind, Payload: raw}).Error; err != nil {
		return nil, 0, err
	}
	return result, status, nil
}
