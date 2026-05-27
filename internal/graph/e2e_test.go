package graph_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/extractor"
	"github.com/agent-mem/agent-mem/internal/graph/fetchers"
	"github.com/agent-mem/agent-mem/internal/graph/handlers"
	"github.com/agent-mem/agent-mem/internal/graph/identity"
	"github.com/agent-mem/agent-mem/internal/graph/normalizer"
)

// openE2EPool connects to DATABASE_URL or skips the test.
func openE2EPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping e2e test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("DB ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// truncateE2ETables cleans up graph tables used by the e2e test.
func truncateE2ETables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	tables := []string{
		"graph.artifact_bodies",
		"graph.artifact_index",
		"graph.edges",
		"graph.jobs",
		"graph.nodes",
		"graph.identity_map",
		"graph.people",
		"graph.entities",
	}
	for _, tbl := range tables {
		if _, err := pool.Exec(ctx, "DELETE FROM "+tbl); err != nil {
			t.Fatalf("truncate %s: %v", tbl, err)
		}
	}
}

// buildE2ERouter wires the ingest endpoints against a real DB pool, no Gemini.
func buildE2ERouter(pool *pgxpool.Pool) http.Handler {
	log := zerolog.Nop()
	deps := handlers.Deps{
		DB:          pool,
		Logger:      log,
		MachineID:   "e2e-machine",
		Fetchers:    fetchers.NewRegistry(fetchers.Config{}, log),
		Normalizers: normalizer.NewRegistry(),
		Extractor:   extractor.New(pool, log),
		Identity:    identity.NewService(pool, log),
		Gemini:      nil, // no Gemini in e2e; describe_attachment jobs will be skipped
	}

	r := chi.NewRouter()
	handlers.Mount(r, deps)
	return r
}

// postIngestContent sends a POST to /api/graph/ingest/content and returns the decoded response.
func postIngestContent(t *testing.T, router http.Handler, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/graph/ingest/content", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	var resp map[string]any
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return w.Code, resp
}

// TestE2E_IngestContent_TRYThread is an end-to-end test against a real Postgres DB.
// It exercises the full synchronous ingest path: node upsert, artifact_bodies upsert,
// edge reconciliation from the extractor, and the outcome tiebreaker.
//
// Uses the Lei TRY thread message from extractor/testdata/try_currency_lei.txt.
func TestE2E_IngestContent_TRYThread(t *testing.T) {
	pool := openE2EPool(t)
	truncateE2ETables(t, pool)

	router := buildE2ERouter(pool)
	ctx := context.Background()

	const (
		channelID = "C08S954G2LX"
		ts        = "1779710863.216389"
		bodyTS1   = "2026-05-25T09:01:03Z"
		bodyTS2   = "2026-05-25T09:02:00Z" // newer — triggers update
	)

	nodeID := "slack:" + channelID + ":" + ts

	// The body from the test fixture.
	body := `Hey team, quick heads up — the TRY (Turkish Lira) payments through checkout are failing intermittently since this morning. I saw about 12 errors in the last hour. Looks like the issue might be related to PAY-2128.

Check the thread here: https://wego.slack.com/archives/C08S954G2LX/p1779710863216389

I'll dig into it more. Anyone from the tabby team have context?`

	req := map[string]any{
		"source":        "slack",
		"canonical_url": "https://wego.slack.com/archives/" + channelID + "/p1779710863216389",
		"body":          body,
		"metadata": map[string]any{
			"channel_id": channelID,
			"ts":         ts,
			"body_ts":    bodyTS1,
			"author": map[string]any{
				"ref":          "slack_uid:UUK3WPNNQ",
				"display_name": "Lei Zheng",
				"is_bot":       false,
			},
			"mentions": []any{},
			"files":    []any{},
			"scope":    "slack:" + channelID,
		},
	}

	// --- First POST: should create the node ---
	code, resp := postIngestContent(t, router, req)
	if code != http.StatusOK {
		t.Fatalf("POST 1: expected 200, got %d: %v", code, resp)
	}
	if resp["node_id"] != nodeID {
		t.Errorf("POST 1 node_id = %v, want %q", resp["node_id"], nodeID)
	}
	if resp["outcome"] != "created" {
		t.Errorf("POST 1 outcome = %v, want created", resp["outcome"])
	}

	// Verify graph.nodes row exists.
	var dbNodeID string
	if err := pool.QueryRow(ctx, `SELECT id FROM graph.nodes WHERE id = $1`, nodeID).Scan(&dbNodeID); err != nil {
		t.Fatalf("graph.nodes row not found: %v", err)
	}

	// Verify graph.artifact_bodies row exists.
	var bodyFull string
	if err := pool.QueryRow(ctx, `SELECT body_full FROM graph.artifact_bodies WHERE node_id = $1`, nodeID).Scan(&bodyFull); err != nil {
		t.Fatalf("graph.artifact_bodies row not found: %v", err)
	}
	if bodyFull == "" {
		t.Error("artifact_bodies.body_full should not be empty")
	}

	// Verify extracted edges include jira:PAY-2128 reference.
	var jiraEdgeCount int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM graph.edges
		WHERE from_node_id = $1 AND to_node_id LIKE 'jira:%'`, nodeID,
	).Scan(&jiraEdgeCount); err != nil {
		t.Fatalf("query jira edges: %v", err)
	}
	if jiraEdgeCount == 0 {
		t.Error("expected at least one jira edge extracted from body referencing PAY-2128")
	}

	// Verify extracted edges include the slack thread URL reference.
	var slackEdgeCount int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM graph.edges
		WHERE from_node_id = $1 AND to_node_id LIKE 'slack:%'`, nodeID,
	).Scan(&slackEdgeCount); err != nil {
		t.Fatalf("query slack edges: %v", err)
	}
	if slackEdgeCount == 0 {
		t.Error("expected at least one slack edge extracted from the wego.slack.com URL in the body")
	}

	// Check edges_created field.
	if ec, ok := resp["edges_created"].(float64); !ok || ec == 0 {
		t.Errorf("expected edges_created > 0, got %v", resp["edges_created"])
	}

	// --- Second POST with newer body_ts: should return "updated" ---
	reqUpdated := copyMap(req)
	reqUpdated["metadata"].(map[string]any)["body_ts"] = bodyTS2
	reqUpdated["body"] = body + "\n\nUpdate: confirmed PAY-2128 is the root cause."

	code2, resp2 := postIngestContent(t, router, reqUpdated)
	if code2 != http.StatusOK {
		t.Fatalf("POST 2: expected 200, got %d: %v", code2, resp2)
	}
	if resp2["outcome"] != "updated" {
		t.Errorf("POST 2 outcome = %v, want updated", resp2["outcome"])
	}

	// --- Third POST with same body_ts as POST 2: should return "unchanged" ---
	code3, resp3 := postIngestContent(t, router, reqUpdated)
	if code3 != http.StatusOK {
		t.Fatalf("POST 3: expected 200, got %d: %v", code3, resp3)
	}
	if resp3["outcome"] != "unchanged" {
		t.Errorf("POST 3 outcome = %v, want unchanged", resp3["outcome"])
	}
}

// TestE2E_IngestContent_Jira verifies Jira source ingestion.
func TestE2E_IngestContent_Jira(t *testing.T) {
	pool := openE2EPool(t)
	truncateE2ETables(t, pool)

	router := buildE2ERouter(pool)
	ctx := context.Background()

	bodyTS := time.Now().UTC().Format(time.RFC3339)

	code, resp := postIngestContent(t, router, map[string]any{
		"source":        "jira",
		"canonical_url": "https://wegomushi.atlassian.net/browse/PAY-2128",
		"body":          "TRY payments intermittent failures — checkout pipeline",
		"metadata": map[string]any{
			"key":         "PAY-2128",
			"project_key": "PAY",
			"body_ts":     bodyTS,
			"author": map[string]any{
				"ref":          "jira_uid:5f1234abc",
				"display_name": "Dev User",
			},
			"mentions": []any{},
			"files":    []any{},
		},
	})

	if code != http.StatusOK {
		t.Fatalf("Jira ingest: expected 200, got %d: %v", code, resp)
	}
	if resp["outcome"] == nil {
		t.Fatal("expected outcome field")
	}

	expectedNodeID := "jira:PAY-2128"
	if resp["node_id"] != expectedNodeID {
		t.Errorf("node_id = %v, want %q", resp["node_id"], expectedNodeID)
	}

	// Verify row in DB.
	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM graph.nodes WHERE id = $1`, expectedNodeID).Scan(&count); err != nil {
		t.Fatalf("count node: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 node row, got %d", count)
	}
}

// copyMap shallow-copies a map for test mutation.
func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
