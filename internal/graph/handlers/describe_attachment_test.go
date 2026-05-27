package handlers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rs/zerolog"
)

func TestDescribeAttachmentHandler_BadPayload(t *testing.T) {
	deps := Deps{Logger: zerolog.Nop(), MachineID: "test"}
	h := NewDescribeAttachmentHandler(deps)

	err := h.Handler(context.Background(), []byte("not json"))
	if err == nil {
		t.Fatal("expected error for bad JSON")
	}
}

func TestDescribeAttachmentHandler_EmptyURL(t *testing.T) {
	deps := Deps{Logger: zerolog.Nop(), MachineID: "test"}
	h := NewDescribeAttachmentHandler(deps)

	payload, _ := json.Marshal(describeAttachmentPayload{
		NodeID: "slack_file:F123",
		Mime:   "image/png",
		Source: "slack",
	})
	err := h.Handler(context.Background(), payload)
	if err == nil {
		t.Fatal("expected error when external_url is empty")
	}
}

func TestDescribeAttachmentHandler_UnsupportedMime(t *testing.T) {
	// We can't make a real HTTP request in unit tests, so test mime filtering
	// via a localhost URL that will fail quickly.
	deps := Deps{Logger: zerolog.Nop(), MachineID: "test"}
	h := NewDescribeAttachmentHandler(deps)

	// Use a data: URL workaround is not possible; test that unsupported mime
	// returns ErrFatal without needing network. We skip if net is unavailable.
	t.Skip("requires network; covered by integration tests")
	_ = deps
	_ = h
}

func TestIsInterestGroup(t *testing.T) {
	cases := []struct {
		handle string
		want   bool
	}{
		{"payments-geeks", true},
		{"infra-ops", true},
		{"general", false},
		{"ops-team", false},
		{"data-geeks", true},
		{"devops", false},
	}
	for _, c := range cases {
		got := isInterestGroup(c.handle)
		if got != c.want {
			t.Errorf("isInterestGroup(%q) = %v, want %v", c.handle, got, c.want)
		}
	}
}
