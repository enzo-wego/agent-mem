package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/extractor"
	"github.com/agent-mem/agent-mem/internal/graph/fetchers"
	"github.com/agent-mem/agent-mem/internal/graph/identity"
	"github.com/agent-mem/agent-mem/internal/graph/normalizer"
)

// TestFetchBody_JiraMetadataMergedAndCreatedAt covers round 0.1: a fetched Jira
// issue lands status/issuetype/labels/parent on graph.nodes.metadata (merged,
// not replaced — pre-existing keys survive), and created_at is filled from the
// issue's `created` when the node had none.
func TestFetchBody_JiraMetadataMergedAndCreatedAt(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	ctx := context.Background()

	issue := map[string]any{
		"key": "PAY-2400",
		"fields": map[string]any{
			"summary":        "GST invoice numbering",
			"description":    map[string]any{"type": "doc", "content": []any{map[string]any{"type": "paragraph", "content": []any{map[string]any{"type": "text", "text": "Invoices for India GST"}}}}},
			"status":         map[string]any{"name": "In Progress"},
			"issuetype":      map[string]any{"name": "Story"},
			"created":        "2026-03-01T10:00:00.000+0000",
			"updated":        "2026-03-05T10:00:00.000+0000",
			"resolutiondate": nil,
			"labels":         []string{"gst"},
			"assignee":       map[string]any{"accountId": "acc-1", "displayName": "A"},
			"parent":         map[string]any{"key": "PAY-2307"},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/comment"):
			_, _ = w.Write([]byte(`{"total":0,"comments":[]}`))
		case strings.HasSuffix(r.URL.Path, "/remotelink"):
			_, _ = w.Write([]byte(`[]`))
		default:
			_ = json.NewEncoder(w).Encode(issue)
		}
	}))
	defer srv.Close()

	// A referenced-but-never-fetched stub with no created_at and a pre-existing
	// metadata key that must survive the merge.
	if _, err := pool.Exec(ctx, `
		INSERT INTO graph.nodes (id, type, natural_key, metadata, machine_id)
		VALUES ('jira:PAY-2400', 'jira', 'PAY-2400', '{"keep":"me"}'::jsonb, 'test')`); err != nil {
		t.Fatalf("seed node: %v", err)
	}

	norms := normalizer.NewRegistry()
	norms.Register(normalizer.NewJiraNormalizer())
	deps := Deps{
		DB:          pool,
		Logger:      zerolog.Nop(),
		MachineID:   "test-machine",
		Fetchers:    fetchers.NewRegistry(fetchers.Config{JiraBaseURL: srv.URL, HTTPClient: srv.Client()}, zerolog.Nop()),
		Normalizers: norms,
		Extractor:   extractor.New(pool, zerolog.Nop()),
		Identity:    identity.NewService(pool, zerolog.Nop()),
	}
	payload, _ := json.Marshal(fetchBodyPayload{NodeID: "jira:PAY-2400"})
	if err := NewFetchBodyHandler(deps).Handler(ctx, payload); err != nil {
		t.Fatalf("fetch_body: %v", err)
	}

	var meta map[string]any
	var createdAt *time.Time
	var body string
	if err := pool.QueryRow(ctx, `SELECT metadata, created_at, body FROM graph.nodes WHERE id='jira:PAY-2400'`).Scan(&meta, &createdAt, &body); err != nil {
		t.Fatalf("read node: %v", err)
	}
	if body != "Invoices for India GST" {
		t.Errorf("body = %q", body)
	}
	for k, want := range map[string]any{
		"keep":                "me",
		"status":              "In Progress",
		"issuetype":           "Story",
		"created":             "2026-03-01T10:00:00.000+0000",
		"parent_key":          "PAY-2307",
		"assignee_account_id": "acc-1",
		"resolutiondate":      nil,
	} {
		if got, ok := meta[k]; !ok || got != want {
			t.Errorf("metadata[%q] = %#v (present=%v), want %#v", k, got, ok, want)
		}
	}
	if createdAt == nil || !createdAt.Equal(time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("created_at = %v, want 2026-03-01T10:00:00Z", createdAt)
	}
}
