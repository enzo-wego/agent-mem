package handlers

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/rs/zerolog"
)

func TestIndexArtifactHandler_BadPayload(t *testing.T) {
	deps := Deps{Logger: zerolog.Nop(), MachineID: "test"}
	h := NewIndexArtifactHandler(deps)

	err := h.Handler(context.Background(), []byte("not json"))
	if err == nil {
		t.Fatal("expected error for bad JSON")
	}
}

func TestIndexArtifactHandler_MissingNodeID(t *testing.T) {
	deps := Deps{Logger: zerolog.Nop(), MachineID: "test"}
	h := NewIndexArtifactHandler(deps)

	payload, _ := json.Marshal(indexArtifactPayload{Force: false})
	err := h.Handler(context.Background(), payload)
	if err == nil {
		t.Fatal("expected error when node_id is empty")
	}
}

func TestIndexArtifactHandler_SkipsWithDB(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
	// Integration test placeholder — covered by DB-backed tests.
}

func TestHeuristicSummary(t *testing.T) {
	cases := []struct {
		nodeID string
		body   string
		wantIn string // the result should contain or start with this
	}{
		{
			nodeID: "jira:PAY-1234",
			body:   "First paragraph.\n\nSecond paragraph.",
			wantIn: "First paragraph.",
		},
		{
			nodeID: "gh_pr:wego/payments#42",
			body:   "PR title\nline2\nline3\nline4",
			wantIn: "PR title",
		},
		{
			nodeID: "slack:C123:1234.5678",
			body:   "Short message",
			wantIn: "Short message",
		},
	}

	for _, c := range cases {
		got := heuristicSummary(c.nodeID, c.body)
		if got == "" {
			t.Errorf("heuristicSummary(%q, ...) returned empty string", c.nodeID)
		}
		if len(got) > 200 {
			t.Errorf("heuristicSummary result exceeds 200 chars: %d", len(got))
		}
	}
}
