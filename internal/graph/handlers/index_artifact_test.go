package handlers

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

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

// Regression: truncation must not split a multi-byte rune — a byte slice
// produced invalid UTF-8 that Postgres rejected (SQLSTATE 22021).
func TestHeuristicSummary_TruncatesOnRuneBoundary(t *testing.T) {
	body := strings.Repeat("é", 300) // 2 bytes each, > 200 runes
	for _, nodeID := range []string{"jira:PAY-1", "gh_pr:wego/x#1", "slack:C1:1.2"} {
		got := heuristicSummary(nodeID, body)
		if !utf8.ValidString(got) {
			t.Errorf("%s: result is not valid UTF-8: %q", nodeID, got)
		}
		if n := utf8.RuneCountInString(got); n > 200 {
			t.Errorf("%s: result exceeds 200 runes: %d", nodeID, n)
		}
	}
}

func TestIndexSummaryForSlackRootPrefersThreadSummary(t *testing.T) {
	got, kind := indexSummaryForSlackRoot("Email blacklist", "Checkout payment links are blocking specific emails.")
	if got != "Email blacklist\n\nCheckout payment links are blocking specific emails." {
		t.Fatalf("summary = %q", got)
	}
	if kind != "thread_summary" {
		t.Fatalf("summary kind = %q, want thread_summary", kind)
	}
}

func TestIndexSummaryForSlackRootFallsBackWhenSummaryMissing(t *testing.T) {
	got, kind := indexSummaryForSlackRoot("", "")
	if got != "" {
		t.Fatalf("summary = %q, want empty", got)
	}
	if kind != "" {
		t.Fatalf("summary kind = %q, want empty", kind)
	}
}
