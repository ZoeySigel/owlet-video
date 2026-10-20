package main

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestJWTKindAndExpiry(t *testing.T) {
	a := &App{cfg: Config{JWTSecret: strings.Repeat("x", 32)}}
	token, err := a.signToken(7, uuid.NewString(), "access", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := a.parseToken(token, "access")
	if err != nil || claims.Subject != "7" {
		t.Fatalf("parse: %v", err)
	}
	if _, err := a.parseToken(token, "refresh"); err == nil {
		t.Fatal("accepted wrong token kind")
	}
}
