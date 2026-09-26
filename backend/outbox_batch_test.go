package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
)

func TestOutboxBatchConfirmsAndRetries(t *testing.T) {
	a := integrationApp(t)
	if a.cfg.RabbitURL == "" {
		t.Skip("TEST_RABBITMQ_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := dialBroker(ctx, a.cfg.RabbitURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	if err = declareBroker(ch); err != nil {
		t.Fatal(err)
	}
	if err = ch.Confirm(false); err != nil {
		t.Fatal(err)
	}
	confirms := ch.NotifyPublish(make(chan amqp.Confirmation, outboxBatchSize))
	returned := ch.NotifyReturn(make(chan amqp.Return, outboxBatchSize))
	rows := make([]Outbox, outboxBatchSize)
	ids := make([]string, len(rows))
	for i := range rows {
		id := uuid.NewString()
		raw, _ := json.Marshal(event{ID: id, Kind: "test.batch"})
		rows[i] = Outbox{ID: id, Kind: "test.batch", Payload: raw}
		ids[i] = id
	}
	if err = a.db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}
	defer a.db.Where("id IN ?", ids).Delete(&Outbox{})
	defer a.db.Where("id IN ?", ids).Delete(&ProcessedEvent{})
	// A closed confirmation stream must never mark uncertain messages sent.
	closed := make(chan amqp.Confirmation)
	close(closed)
	if err = a.publishOutboxBatch(ctx, ch, closed, returned, rows[:1]); err == nil {
		t.Fatal("accepted missing confirmation")
	}
	var n int64
	a.db.Model(&Outbox{}).Where("id IN ? AND published_at IS NOT NULL", ids).Count(&n)
	if n != 0 {
		t.Fatal("marked unconfirmed batch published")
	}
	// Drain the real confirmation for that deliberately uncertain attempt.
	select {
	case <-confirms:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err = a.publishOutboxBatch(ctx, ch, confirms, returned, rows); err != nil {
			t.Fatal(err)
		}
	}
	a.db.Model(&Outbox{}).Where("id IN ? AND published_at IS NOT NULL", ids).Count(&n)
	if n != int64(len(rows)) {
		t.Fatalf("published=%d", n)
	}
	// Consume both retries and verify durable deduplication across all events.
	for _, row := range rows {
		var e event
		if err = json.Unmarshal(row.Payload, &e); err != nil {
			t.Fatal(err)
		}
		for attempt := 0; attempt < 2; attempt++ {
			if err = a.processEvent(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
	}
	a.db.Model(&ProcessedEvent{}).Where("id IN ?", ids).Count(&n)
	if n != int64(len(rows)) {
		t.Fatalf("deduplicated=%d", n)
	}
}
