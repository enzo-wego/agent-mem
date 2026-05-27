package handlers

import (
	"context"
	"os"
	"testing"

	"github.com/rs/zerolog"
)

func TestRefreshSlackGroupsHandler_MissingToken(t *testing.T) {
	t.Setenv("SLACK_BOT_TOKEN", "")
	deps := Deps{Logger: zerolog.Nop(), MachineID: "test"}
	h := NewRefreshSlackGroupsHandler(deps)

	err := h.Handler(context.Background(), []byte("{}"))
	if err == nil {
		t.Fatal("expected error when SLACK_BOT_TOKEN is not set")
	}
}

func TestRefreshSlackGroupsHandler_WithDB(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
	// Integration test placeholder.
}

func TestIsInterestGroup_Patterns(t *testing.T) {
	if !isInterestGroup("payments-geeks") {
		t.Error("expected payments-geeks to match -geeks")
	}
	if !isInterestGroup("platform-ops") {
		t.Error("expected platform-ops to match -ops")
	}
	if isInterestGroup("general") {
		t.Error("expected general NOT to match")
	}
	if isInterestGroup("geeks-club") {
		t.Error("expected geeks-club NOT to match (prefix, not suffix)")
	}
}
