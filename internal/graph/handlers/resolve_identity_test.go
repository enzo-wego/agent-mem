package handlers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rs/zerolog"
)

func TestResolveIdentityHandler_BadPayload(t *testing.T) {
	deps := Deps{Logger: zerolog.Nop(), MachineID: "test"}
	h := NewResolveIdentityHandler(deps)

	err := h.Handler(context.Background(), []byte("not json"))
	if err == nil {
		t.Fatal("expected error for bad JSON")
	}
}

func TestResolveIdentityHandler_MissingPersonID(t *testing.T) {
	deps := Deps{Logger: zerolog.Nop(), MachineID: "test"}
	h := NewResolveIdentityHandler(deps)

	payload, _ := json.Marshal(resolveIdentityPayload{Source: "slack", ExternalID: "U123"})
	err := h.Handler(context.Background(), payload)
	if err == nil {
		t.Fatal("expected error when person_id is 0")
	}
}

func TestFetchUserInfo_UnknownSource(t *testing.T) {
	_, _, err := fetchUserInfo(context.Background(), "unknown_source", "id123")
	if err == nil {
		t.Fatal("expected error for unknown source")
	}
}

func TestFetchUserInfo_SlackMissingToken(t *testing.T) {
	t.Setenv("SLACK_BOT_TOKEN", "")
	_, _, err := fetchUserInfo(context.Background(), "slack", "U123")
	if err == nil {
		t.Fatal("expected error when SLACK_BOT_TOKEN is not set")
	}
}

func TestFetchUserInfo_JiraMissingToken(t *testing.T) {
	t.Setenv("JIRA_TOKEN", "")
	_, _, err := fetchUserInfo(context.Background(), "jira", "accountid123")
	if err == nil {
		t.Fatal("expected error when JIRA_TOKEN is not set")
	}
}
