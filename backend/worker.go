package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net"
	"regexp"
	"sync"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"gorm.io/gorm"
)

type event struct {
	ID   string         `json:"id"`
	Kind string         `json:"kind"`
	Data map[string]any `json:"data"`
}

func enqueue(tx *gorm.DB, kind string, data map[string]any) error {
	e := event{ID: uuid.NewString(), Kind: kind, Data: data}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return tx.Create(&Outbox{ID: e.ID, Kind: kind, Payload: b}).Error
}
func eventUint(e event, key string) uint     { n, _ := e.Data[key].(float64); return uint(n) }
func eventString(e event, key string) string { s, _ := e.Data[key].(string); return s }

func declareBroker(ch *amqp.Channel) error {
	if err := ch.ExchangeDeclare("owlet.events", "topic", true, false, false, false, nil); err != nil {
		return err
	}
	if err := ch.ExchangeDeclare("owlet.dlx", "direct", true, false, false, false, nil); err != nil {
		return err
	}
	if _, err := ch.QueueDeclare("owlet.events", true, false, false, false, amqp.Table{"x-dead-letter-exchange": "owlet.dlx"}); err != nil {
		return err
	}
	if err := ch.QueueBind("owlet.events", "events", "owlet.events", false, nil); err != nil {
		return err
	}
	if _, err := ch.QueueDeclare("owlet.retry", true, false, false, false, amqp.Table{"x-message-ttl": int32(10000), "x-dead-letter-exchange": "owlet.events", "x-dead-letter-routing-key": "events"}); err != nil {
		return err
	}
	if _, err := ch.QueueDeclare("owlet.dead", true, false, false, false, nil); err != nil {
		return err
	}
	return ch.QueueBind("owlet.dead", "events", "owlet.dlx", false, nil)
}

// Run maintenance independently of RabbitMQ; reconnect failed broker sessions
// with capped exponential backoff instead of relying only on systemd restarts.
func (a *App) runWorker(ctx context.Context) error {
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	maintenanceDone := make(chan struct{})
	go func() { defer close(maintenanceDone); a.rankMaintenance(work) }()
	defer func() { cancel(); <-maintenanceDone }()
	delay := time.Second
	for work.Err() == nil {
		started := time.Now()
		err := a.runBrokerSession(work)
		if work.Err() != nil {
			return nil
		}
		if err == nil {
			return nil
		}
		if time.Since(started) > time.Minute {
			delay = time.Second
		}
		wait := delay + time.Duration(rand.IntN(250))*time.Millisecond
		log.Printf("broker session unavailable; retrying in %s", wait)
		timer := time.NewTimer(wait)
		select {
		case <-work.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		delay = min(delay*2, 30*time.Second)
	}
	return nil
}
func dialBroker(ctx context.Context, url string) (*amqp.Connection, error) {
	var transport net.Conn
	conn, err := amqp.DialConfig(url, amqp.Config{Heartbeat: 10 * time.Second, Dial: func(network, addr string) (net.Conn, error) {
		var e error
		transport, e = (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, addr)
		if e == nil {
			e = transport.SetDeadline(time.Now().Add(3 * time.Second))
			if e != nil {
				transport.Close()
			}
		}
		return transport, e
	}})
	if err != nil {
		if transport != nil {
			transport.Close()
		}
		return nil, err
	}
	if transport != nil {
		_ = transport.SetDeadline(time.Time{})
	}
	return conn, nil
}
func (a *App) runBrokerSession(ctx context.Context) error {
	conn, err := dialBroker(ctx, a.cfg.RabbitURL)
	if err != nil {
		return err
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	defer ch.Close()
	if err := declareBroker(ch); err != nil {
		return err
	}
	if err := ch.Confirm(false); err != nil {
		return err
	}
	confirm := ch.NotifyPublish(make(chan amqp.Confirmation, 1))
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 3)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); errCh <- a.publishOutbox(workCtx, ch, confirm) }()
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errCh <- a.consumeEvents(workCtx, conn) }()
	}
	select {
	case <-ctx.Done():
		cancel()
		_ = conn.Close()
		wg.Wait()
		return nil
	case err := <-errCh:
		cancel()
		_ = conn.Close()
		wg.Wait()
		return err
	}
}

func (a *App) publishOutbox(ctx context.Context, ch *amqp.Channel, confirm <-chan amqp.Confirmation) error {
	ticker := time.NewTicker(600 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		var rows []Outbox
		if err := a.db.WithContext(ctx).Where("published_at IS NULL").Order("created_at ASC").Limit(20).Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			if err := ch.PublishWithContext(ctx, "owlet.events", "events", true, false, amqp.Publishing{DeliveryMode: amqp.Persistent, ContentType: "application/json", MessageId: row.ID, Body: row.Payload}); err != nil {
				return err
			}
			select {
			case ack := <-confirm:
				if !ack.Ack {
					return errors.New("broker rejected publish")
				}
			case <-ctx.Done():
				return nil
			case <-time.After(5 * time.Second):
				return errors.New("broker confirm timeout")
			}
			now := time.Now()
			if err := a.db.WithContext(ctx).Model(&Outbox{}).Where("id = ? AND published_at IS NULL", row.ID).Update("published_at", &now).Error; err != nil {
				return err
			}
		}
	}
}

func (a *App) consumeEvents(ctx context.Context, conn *amqp.Connection) error {
	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	defer ch.Close()
	if err := ch.Qos(4, 0, false); err != nil {
		return err
	}
	if err := ch.Confirm(false); err != nil {
		return err
	}
	confirm := ch.NotifyPublish(make(chan amqp.Confirmation, 1))
	deliveries, err := ch.Consume("owlet.events", "", false, false, false, false, nil)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case d, open := <-deliveries:
			if !open {
				return errors.New("broker delivery channel closed")
			}
			var e event
			if json.Unmarshal(d.Body, &e) != nil || e.ID == "" {
				_ = d.Reject(false)
				continue
			}
			if err := a.processEvent(ctx, e); err == nil {
				_ = d.Ack(false)
				continue
			} else {
				log.Printf("event %s failed: %v", e.ID, err)
			}
			attempt := 0
			if raw, ok := d.Headers["attempt"]; ok {
				switch n := raw.(type) {
				case int32:
					attempt = int(n)
				case int64:
					attempt = int(n)
				}
			}
			if attempt >= 3 {
				_ = d.Reject(false)
				continue
			}
			err = ch.PublishWithContext(ctx, "", "owlet.retry", true, false, amqp.Publishing{DeliveryMode: amqp.Persistent, ContentType: "application/json", MessageId: e.ID, Body: d.Body, Headers: amqp.Table{"attempt": int32(attempt + 1)}})
			if err == nil {
				select {
				case ack := <-confirm:
					if !ack.Ack {
						err = errors.New("broker rejected retry")
					}
				case <-ctx.Done():
					return nil
				case <-time.After(5 * time.Second):
					err = errors.New("retry confirm timeout")
				}
			}
			if err != nil {
				_ = d.Nack(false, true)
			} else {
				_ = d.Ack(false)
			}
		}
	}
}

var mentionPattern = regexp.MustCompile(`@[a-zA-Z0-9_]{3,40}`)

func (a *App) processEvent(ctx context.Context, e event) error {
	var notifyUsers []uint
	err := a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		err := tx.Create(&ProcessedEvent{ID: e.ID}).Error
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return nil
		}
		if err != nil {
			return err
		}
		actor := eventUint(e, "userId")
		owner := eventUint(e, "ownerId")
		video := eventUint(e, "videoId")
		createNotice := func(target uint, kind, body string) error {
			if target == 0 || target == actor {
				return nil
			}
			n := Notification{UserID: target, ActorID: actor, Kind: kind, Body: body}
			if video > 0 {
				n.VideoID = &video
			}
			if err := tx.Create(&n).Error; err != nil {
				return err
			}
			notifyUsers = append(notifyUsers, target)
			return nil
		}
		switch e.Kind {
		case "video.liked":
			return createNotice(owner, "like", "赞了你的视频")
		case "video.commented":
			if err := createNotice(owner, "comment", "评论了你的视频"); err != nil {
				return err
			}
			seen := map[uint]bool{}
			for _, m := range mentionPattern.FindAllString(eventString(e, "body"), -1) {
				var u User
				if tx.Where("username = ?", m[1:]).First(&u).Error == nil && !seen[u.ID] {
					seen[u.ID] = true
					if err := createNotice(u.ID, "mention", "在评论中提到了你"); err != nil {
						return err
					}
				}
			}
		case "user.followed":
			return createNotice(owner, "follow", "关注了你")
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Cache and realtime signals are optional side effects. A Redis outage must
	// not dead-letter an event whose durable notification already committed.
	videoID := eventUint(e, "videoId")
	if videoID > 0 {
		a.invalidateVideo(videoID)
	}
	for _, uid := range notifyUsers {
		_ = a.withRedis(ctx, func(ctx context.Context) error {
			return a.redis.Publish(ctx, fmt.Sprintf("notifications:%d", uid), e.ID).Err()
		})
	}
	return nil
}
