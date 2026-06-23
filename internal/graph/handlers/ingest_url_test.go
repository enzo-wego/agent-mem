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

	"github.com/agent-mem/agent-mem/internal/graph/fetchers"
)

func TestIngestURL_UnknownURL(t *testing.T) {
	deps := Deps{
		Logger:    zerolog.Nop(),
		MachineID: "test",
		Fetchers:  fetchers.NewRegistry(fetchers.Config{}, zerolog.Nop()),
	}
	handler := NewIngestURLHandler(deps)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"url":"https://unknown.example.com/foo"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestIngestURL_MissingURL(t *testing.T) {
	deps := Deps{
		Logger:    zerolog.Nop(),
		MachineID: "test",
		Fetchers:  fetchers.NewRegistry(fetchers.Config{}, zerolog.Nop()),
	}
	handler := NewIngestURLHandler(deps)

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing url, got %d: %s", w.Code, w.Body.String())
	}
}

func TestIngestURL_Slack_QueuesFetch(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)

	deps := Deps{
		DB:        pool,
		Logger:    zerolog.Nop(),
		MachineID: "test-machine",
		Fetchers:  fetchers.NewRegistry(fetchers.Config{}, zerolog.Nop()),
	}
	handler := NewIngestURLHandler(deps)

	body := `{"url":"https://wego.slack.com/archives/CUV9EAYGY/p1779251276315399","scope_hint":"slack:CUV9EAYGY"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var resp ingestURLResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Outcome != "queued_for_fetch" {
		t.Errorf("outcome = %q, want queued_for_fetch", resp.Outcome)
	}
	if len(resp.JobsEnqueued) != 2 {
		t.Errorf("jobs_enqueued = %d, want 2", len(resp.JobsEnqueued))
	}

	expectedNodeID := "slack:CUV9EAYGY:1779251276.315399"
	if resp.NodeID != expectedNodeID {
		t.Errorf("node_id = %q, want %q", resp.NodeID, expectedNodeID)
	}

	// Verify jobs were inserted.
	var jobCount int
	pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM graph.jobs WHERE type IN ('fetch_body','index_artifact') AND payload::text LIKE $1`,
		"%"+expectedNodeID+"%",
	).Scan(&jobCount)
	if jobCount < 2 {
		t.Errorf("expected at least 2 jobs for node_id, got %d", jobCount)
	}
}

func TestIngestURL_AlreadyFresh(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)

	// Pre-insert a node + fresh artifact_body.
	nodeID := "slack:CUV9EAYGY:1779251276.315399"
	ctx := context.Background()
	pool.Exec(ctx, `
		INSERT INTO graph.nodes (id, type, natural_key, url, body_revision, updated_at, machine_id)
		VALUES ($1, 'slack', $2, $3, 1, NOW(), 'test')`,
		nodeID, "CUV9EAYGY:1779251276.315399",
		"https://wego.slack.com/archives/CUV9EAYGY/p1779251276315399",
	)
	pool.Exec(ctx, `
		INSERT INTO graph.artifact_bodies (node_id, body_full, fetched_at)
		VALUES ($1, 'cached body', $2)`,
		nodeID, time.Now().Add(-5*time.Minute),
	)

	deps := Deps{
		DB:        pool,
		Logger:    zerolog.Nop(),
		MachineID: "test-machine",
		Fetchers:  fetchers.NewRegistry(fetchers.Config{}, zerolog.Nop()),
	}
	handler := NewIngestURLHandler(deps)

	body := `{"url":"https://wego.slack.com/archives/CUV9EAYGY/p1779251276315399"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var resp ingestURLResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Outcome != "already_fresh" {
		t.Errorf("outcome = %q, want already_fresh", resp.Outcome)
	}
	if len(resp.JobsEnqueued) != 0 {
		t.Errorf("expected 0 jobs for already_fresh, got %d", len(resp.JobsEnqueued))
	}
}
