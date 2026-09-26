package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestCommentRoundTripAndDuplicateWithEncodedParameters(t *testing.T) {
	a := integrationApp(t)
	v := fixtureVideos(t, a, 1)[0]
	var actor User
	if err := a.db.First(&actor, v.UserID).Error; err != nil {
		t.Fatal(err)
	}
	body := "中文🙂 'quote' \\ backslash ? % \x00 ; DROP TABLE comments; --"
	cmd := InteractionCommand{ID: uuid.NewString(), UserID: actor.ID, TargetID: v.ID, Kind: "comment", Body: body}
	if err := a.db.Create(&cmd).Error; err != nil {
		t.Fatal(err)
	}
	defer a.db.Delete(&cmd)
	for i := 0; i < 2; i++ {
		if err := a.executeInteraction(context.Background(), cmd.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.db.First(&cmd, "id = ?", cmd.ID).Error; err != nil {
		t.Fatal(err)
	}
	defer a.db.Where("id = ?", cmd.EventID).Delete(&Outbox{})
	var result Comment
	if err := json.Unmarshal(cmd.Result, &result); err != nil {
		t.Fatal(err)
	}
	var saved Comment
	if err := a.db.First(&saved, result.ID).Error; err != nil {
		t.Fatal(err)
	}
	if result.Body != body || saved.Body != body || !result.CreatedAt.Equal(saved.CreatedAt) {
		t.Fatalf("comment round trip changed body or timestamp: result=%+v saved=%+v", result, saved)
	}
	if result.User.ID != actor.ID || result.User.Username != actor.Username || result.User.PasswordHash != "" {
		t.Fatalf("unexpected serialized author: %+v", result.User)
	}
	var count int64
	if err := a.db.Model(&Comment{}).Where("video_id = ? AND body = ?", v.ID, body).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("duplicate comment count=%d", count)
	}
	var outbox Outbox
	if err := a.db.First(&outbox, "id = ?", cmd.EventID).Error; err != nil {
		t.Fatal(err)
	}
	var e event
	if err := json.Unmarshal(outbox.Payload, &e); err != nil {
		t.Fatal(err)
	}
	if eventString(e, "body") != body {
		t.Fatal("outbox payload changed")
	}
}
