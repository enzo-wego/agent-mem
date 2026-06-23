package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// chiRequest wraps a request with chi URL params set.
func chiRequest(method, path string, body string, paramName, paramValue string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(paramName, paramValue)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	return req
}

func TestJobsList_BasicQuery(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)

	ctx := context.Background()
	// Enqueue 3 jobs.
	for i := 0; i < 3; i++ {
		_, err := jobs.Enqueue(ctx, pool, "fetch_body", map[string]string{
			"node_id": fmt.Sprintf("slack:C01:%d.000000", i),
		}, jobs.EnqueueOptions{
			Priority:  5,
			MachineID: "test",
		})
		if err != nil {
			t.Fatalf("enqueue job %d: %v", i, err)
		}
	}

	deps := Deps{
		DB:        pool,
		Logger:    zerolog.Nop(),
		MachineID: "test",
	}
	handler := NewJobsListHandler(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/graph/jobs", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp jobsListResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(resp.Jobs) != 3 {
		t.Errorf("expected 3 jobs, got %d", len(resp.Jobs))
	}
	if resp.QueueDepth["queued"] < 3 {
		t.Errorf("queue_depth.queued = %d, want >= 3", resp.QueueDepth["queued"])
	}
}

func TestJobsList_FilterByStatus(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)

	ctx := context.Background()
	// Enqueue 2 queued jobs.
	for i := 0; i < 2; i++ {
		jobs.Enqueue(ctx, pool, "fetch_body", map[string]string{"node_id": fmt.Sprintf("slack:C02:%d.000000", i)},
			jobs.EnqueueOptions{Priority: 5, MachineID: "test"})
	}
	// Manually insert 1 running job.
	pool.Exec(ctx, `
		INSERT INTO graph.jobs (type, payload, priority, status, max_attempts, target_runner, machine_id, locked_by, locked_at)
		VALUES ('fetch_body', '{"node_id":"slack:C02:99.000000"}', 5, 'running', 5, 'any', 'test', 'worker-1', NOW())
	`)

	deps := Deps{
		DB:        pool,
		Logger:    zerolog.Nop(),
		MachineID: "test",
	}
	handler := NewJobsListHandler(deps)

	req := httptest.NewRequest(http.MethodGet, "/api/graph/jobs?status=queued", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp jobsListResponse
	json.NewDecoder(w.Body).Decode(&resp)

	for _, j := range resp.Jobs {
		if j.Status != "queued" {
			t.Errorf("got job with status %q in queued-only filter", j.Status)
		}
	}
	if len(resp.Jobs) != 2 {
		t.Errorf("expected 2 queued jobs, got %d", len(resp.Jobs))
	}
}

func TestJobsAdmin_Retry(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)

	ctx := context.Background()
	// Insert a failed job.
	var jobID int64
	pool.QueryRow(ctx, `
		INSERT INTO graph.jobs (type, payload, priority, status, max_attempts, target_runner, machine_id, last_error)
		VALUES ('fetch_body', '{"node_id":"slack:C03:1.000000"}', 5, 'failed', 5, 'any', 'test', 'timeout')
		RETURNING id
	`).Scan(&jobID)

	if jobID == 0 {
		t.Fatal("failed to insert test job")
	}

	deps := Deps{
		DB:        pool,
		Logger:    zerolog.Nop(),
		MachineID: "test",
	}
	handler := NewJobsRetryHandler(deps)

	req := chiRequest(http.MethodPost, fmt.Sprintf("/api/graph/jobs/%d/retry", jobID), "", "id", fmt.Sprintf("%d", jobID))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify status flipped to queued.
	var status string
	pool.QueryRow(ctx, `SELECT status FROM graph.jobs WHERE id = $1`, jobID).Scan(&status)
	if status != "queued" {
		t.Errorf("status = %q after retry, want queued", status)
	}
}

func TestJobsAdmin_Delete(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)

	ctx := context.Background()
	// Insert a queued job.
	var jobID int64
	pool.QueryRow(ctx, `
		INSERT INTO graph.jobs (type, payload, priority, status, max_attempts, target_runner, machine_id)
		VALUES ('fetch_body', '{"node_id":"slack:C04:2.000000"}', 5, 'queued', 5, 'any', 'test')
		RETURNING id
	`).Scan(&jobID)

	if jobID == 0 {
		t.Fatal("failed to insert test job")
	}

	deps := Deps{
		DB:        pool,
		Logger:    zerolog.Nop(),
		MachineID: "test",
	}
	handler := NewJobsDeleteHandler(deps)

	req := chiRequest(http.MethodDelete, fmt.Sprintf("/api/graph/jobs/%d", jobID), "", "id", fmt.Sprintf("%d", jobID))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify status flipped to failed.
	var status, lastError string
	pool.QueryRow(ctx, `SELECT status, last_error FROM graph.jobs WHERE id = $1`, jobID).Scan(&status, &lastError)
	if status != "failed" {
		t.Errorf("status = %q after delete, want failed", status)
	}
	if lastError != "manually deleted" {
		t.Errorf("last_error = %q, want 'manually deleted'", lastError)
	}
}
