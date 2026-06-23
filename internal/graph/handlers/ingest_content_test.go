package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/extractor"
	"github.com/agent-mem/agent-mem/internal/graph/fetchers"
	"github.com/agent-mem/agent-mem/internal/graph/identity"
	"github.com/agent-mem/agent-mem/internal/graph/normalizer"
)

// postJSON is a test helper that sends a POST request with a JSON body.
func postJSON(t *testing.T, handler http.Handler, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func TestIngestContent_RejectsUnknownSource(t *testing.T) {
	deps := Deps{
		Logger:    zerolog.Nop(),
		MachineID: "test",
		Fetchers:  fetchers.NewRegistry(fetchers.Config{}, zerolog.Nop()),
	}
	handler := NewIngestContentHandler(deps)

	w := postJSON(t, handler, map[string]any{
		"source":        "weird",
		"canonical_url": "https://example.com",
		"body":          "hello world",
		"metadata":      map[string]any{},
	})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]string
	json.NewDecoder(w.Body).Decode(&resp)
	if resp["error"] == "" {
		t.Fatal("expected error field in response")
	}
}

func TestIngestContent_RejectsMissingSlackFields(t *testing.T) {
	deps := Deps{
		Logger:    zerolog.Nop(),
		MachineID: "test",
		Fetchers:  fetchers.NewRegistry(fetchers.Config{}, zerolog.Nop()),
	}
	handler := NewIngestContentHandler(deps)

	// Slack source but no channel_id or ts in metadata.
	w := postJSON(t, handler, map[string]any{
		"source":        "slack",
		"canonical_url": "https://wego.slack.com/archives/C05/p1779",
		"body":          "hello",
		"metadata":      map[string]any{},
	})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing channel_id/ts, got %d: %s", w.Code, w.Body.String())
	}
}

func TestIngestContent_Slack_CreatesNode(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)

	deps := Deps{
		DB:          pool,
		Logger:      zerolog.Nop(),
		MachineID:   "test-machine",
		Fetchers:    fetchers.NewRegistry(fetchers.Config{}, zerolog.Nop()),
		Normalizers: normalizer.NewRegistry(),
		Extractor:   extractor.New(pool, zerolog.Nop()),
		Identity:    identity.NewService(pool, zerolog.Nop()),
	}
	handler := NewIngestContentHandler(deps)

	w := postJSON(t, handler, map[string]any{
		"source":        "slack",
		"canonical_url": "https://wego.slack.com/archives/C05RNSE8TBR/p1779711855864859",
		"body":          "deploy PAY-2200 is live",
		"metadata": map[string]any{
			"channel_id": "C05RNSE8TBR",
			"ts":         "1779711855.864859",
			"body_ts":    "2026-05-25T12:24:15Z",
			"author": map[string]any{
				"ref":          "slack_uid:UUK3WPNNQ",
				"display_name": "Lei Zheng",
				"is_bot":       false,
			},
			"mentions": []any{},
			"files":    []any{},
			"scope":    "slack:C05RNSE8TBR",
		},
	})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp ingestResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	expectedNodeID := "slack:C05RNSE8TBR:1779711855.864859"
	if resp.NodeID != expectedNodeID {
		t.Errorf("node_id = %q, want %q", resp.NodeID, expectedNodeID)
	}
	if resp.Outcome != "created" {
		t.Errorf("outcome = %q, want created", resp.Outcome)
	}

	// Verify node row exists in DB.
	var dbCount int
	pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM graph.nodes WHERE id = $1`, expectedNodeID).Scan(&dbCount)
	if dbCount != 1 {
		t.Errorf("expected 1 node row, got %d", dbCount)
	}
}

func TestIngestContent_Idempotent_SameBodyTS(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)

	deps := Deps{
		DB:          pool,
		Logger:      zerolog.Nop(),
		MachineID:   "test-machine",
		Fetchers:    fetchers.NewRegistry(fetchers.Config{}, zerolog.Nop()),
		Normalizers: normalizer.NewRegistry(),
		Extractor:   extractor.New(pool, zerolog.Nop()),
		Identity:    identity.NewService(pool, zerolog.Nop()),
	}
	handler := NewIngestContentHandler(deps)

	req := map[string]any{
		"source":        "slack",
		"canonical_url": "https://wego.slack.com/archives/C05RNSE8TBR/p1779711856000000",
		"body":          "same body",
		"metadata": map[string]any{
			"channel_id": "C05RNSE8TBR",
			"ts":         "1779711856.000000",
			"body_ts":    "2026-05-25T12:25:00Z",
			"author":     map[string]any{"ref": "slack_uid:UABC123", "display_name": "Test User"},
			"mentions":   []any{},
			"files":      []any{},
		},
	}

	// First POST.
	w1 := postJSON(t, handler, req)
	if w1.Code != http.StatusOK {
		t.Fatalf("first POST: expected 200, got %d: %s", w1.Code, w1.Body.String())
	}
	var r1 ingestResponse
	json.NewDecoder(w1.Body).Decode(&r1)
	if r1.Outcome != "created" {
		t.Errorf("first post outcome = %q, want created", r1.Outcome)
	}

	// Second POST with same body_ts.
	w2 := postJSON(t, handler, req)
	if w2.Code != http.StatusOK {
		t.Fatalf("second POST: expected 200, got %d: %s", w2.Code, w2.Body.String())
	}
	var r2 ingestResponse
	json.NewDecoder(w2.Body).Decode(&r2)
	if r2.Outcome != "unchanged" {
		t.Errorf("second post outcome = %q, want unchanged", r2.Outcome)
	}
}

func TestIngestContent_WithFiles_EnqueuesDescribeJobs(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)

	deps := Deps{
		DB:          pool,
		Logger:      zerolog.Nop(),
		MachineID:   "test-machine",
		Fetchers:    fetchers.NewRegistry(fetchers.Config{}, zerolog.Nop()),
		Normalizers: normalizer.NewRegistry(),
		Extractor:   extractor.New(pool, zerolog.Nop()),
		Identity:    identity.NewService(pool, zerolog.Nop()),
	}
	handler := NewIngestContentHandler(deps)

	w := postJSON(t, handler, map[string]any{
		"source":        "slack",
		"canonical_url": "https://wego.slack.com/archives/C05RNSE8TBR/p1779711900000000",
		"body":          "here is a screenshot",
		"metadata": map[string]any{
			"channel_id": "C05RNSE8TBR",
			"ts":         "1779711900.000000",
			"body_ts":    "2026-05-25T12:30:00Z",
			"author":     map[string]any{"ref": "slack_uid:UTEST001", "display_name": "Tester"},
			"mentions":   []any{},
			"files": []any{
				map[string]any{
					"id":          "F0B5TLXQLTV",
					"mimetype":    "image/png",
					"filename":    "screenshot.png",
					"size":        248312,
					"url_private": "https://files.slack.com/screenshot.png",
					"thumb_360":   "https://files.slack.com/screenshot_360.png",
				},
			},
		},
	})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp ingestResponse
	json.NewDecoder(w.Body).Decode(&resp)

	if len(resp.AttachmentsRegistered) != 1 {
		t.Errorf("expected 1 attachment registered, got %d", len(resp.AttachmentsRegistered))
	}
	if len(resp.AttachmentsRegistered) > 0 && resp.AttachmentsRegistered[0].NodeID != "slack_file:F0B5TLXQLTV" {
		t.Errorf("attachment node_id = %q, want slack_file:F0B5TLXQLTV", resp.AttachmentsRegistered[0].NodeID)
	}

	// Verify a describe_attachment job row was created.
	var jobCount int
	pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM graph.jobs WHERE type = 'describe_attachment'`,
	).Scan(&jobCount)
	if jobCount < 1 {
		t.Errorf("expected at least 1 describe_attachment job, got %d", jobCount)
	}
}
