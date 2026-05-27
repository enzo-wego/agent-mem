package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// jobsListResponse is the response body for GET /api/graph/jobs.
type jobsListResponse struct {
	QueueDepth       map[string]int `json:"queue_depth"`
	OldestQueuedAgeS float64        `json:"oldest_queued_age_s"`
	Jobs             []jobRowView   `json:"jobs"`
}

// jobRowView is one row in the jobs listing.
type jobRowView struct {
	ID          int64           `json:"id"`
	Type        string          `json:"type"`
	Priority    int16           `json:"priority"`
	Payload     json.RawMessage `json:"payload"`
	AvailableAt string          `json:"available_at"`
	Attempts    int16           `json:"attempts"`
	Status      string          `json:"status"`
	LastError   string          `json:"last_error"`
}

// NewJobsListHandler returns an http.Handler for GET /api/graph/jobs.
func NewJobsListHandler(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		statusFilter := r.URL.Query().Get("status")
		typeFilter := r.URL.Query().Get("type")
		limit := 50
		if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 {
			limit = l
		}

		// Queue depth across all types.
		depth, err := jobs.QueueDepth(ctx, deps.DB, "")
		if err != nil {
			deps.Logger.Error().Err(err).Msg("jobs_list: QueueDepth failed")
			writeError(w, http.StatusInternalServerError, "queue depth query failed")
			return
		}

		// Ensure all known statuses appear in the map (even if zero).
		for _, s := range []string{"queued", "running", "failed", "done"} {
			if _, ok := depth[s]; !ok {
				depth[s] = 0
			}
		}

		// Oldest queued job age.
		var oldestAvailableAt *time.Time
		_ = deps.DB.QueryRow(ctx,
			`SELECT MIN(available_at) FROM graph.jobs WHERE status = 'queued'`,
		).Scan(&oldestAvailableAt)
		var oldestAgeS float64
		if oldestAvailableAt != nil {
			age := time.Since(*oldestAvailableAt).Seconds()
			if age > 0 {
				oldestAgeS = age
			}
		}

		// Build listing.
		rows, err := listJobs(ctx, deps, statusFilter, typeFilter, limit)
		if err != nil {
			deps.Logger.Error().Err(err).Msg("jobs_list: query failed")
			writeError(w, http.StatusInternalServerError, "jobs query failed")
			return
		}
		if rows == nil {
			rows = []jobRowView{}
		}

		resp := jobsListResponse{
			QueueDepth:       depth,
			OldestQueuedAgeS: oldestAgeS,
			Jobs:             rows,
		}
		writeJSON(w, http.StatusOK, resp)
	})
}

// NewJobsDeleteHandler returns an http.Handler for DELETE /api/graph/jobs/{id}.
func NewJobsDeleteHandler(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseJobID(w, r)
		if !ok {
			return
		}

		ctx := r.Context()
		_, err := deps.DB.Exec(ctx,
			`UPDATE graph.jobs SET status = 'failed', last_error = 'manually deleted' WHERE id = $1`,
			id,
		)
		if err != nil {
			deps.Logger.Error().Err(err).Int64("job_id", id).Msg("jobs_delete: update failed")
			writeError(w, http.StatusInternalServerError, "delete job failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "outcome": "deleted"})
	})
}

// NewJobsRetryHandler returns an http.Handler for POST /api/graph/jobs/{id}/retry.
func NewJobsRetryHandler(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseJobID(w, r)
		if !ok {
			return
		}

		ctx := r.Context()
		_, err := deps.DB.Exec(ctx,
			`UPDATE graph.jobs
			 SET status = 'queued', available_at = NOW(), attempts = 0, last_error = NULL
			 WHERE id = $1 AND status = 'failed'`,
			id,
		)
		if err != nil {
			deps.Logger.Error().Err(err).Int64("job_id", id).Msg("jobs_retry: update failed")
			writeError(w, http.StatusInternalServerError, "retry job failed")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "outcome": "requeued"})
	})
}

// parseJobID extracts and validates the {id} chi URL param.
func parseJobID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid job id")
		return 0, false
	}
	return id, true
}

// listJobs queries graph.jobs with optional status and type filters.
func listJobs(ctx context.Context, deps Deps, statusFilter, typeFilter string, limit int) ([]jobRowView, error) {
	var (
		pgRows interface {
			Next() bool
			Scan(dest ...any) error
			Close()
			Err() error
		}
		err error
	)

	switch {
	case statusFilter != "" && typeFilter != "":
		pgRows, err = deps.DB.Query(ctx, `
			SELECT id, type, priority, payload, available_at, attempts, status, COALESCE(last_error,'')
			FROM graph.jobs
			WHERE status = $1 AND type = $2
			ORDER BY priority ASC, available_at ASC, id ASC
			LIMIT $3`,
			statusFilter, typeFilter, limit)
	case statusFilter != "":
		pgRows, err = deps.DB.Query(ctx, `
			SELECT id, type, priority, payload, available_at, attempts, status, COALESCE(last_error,'')
			FROM graph.jobs
			WHERE status = $1
			ORDER BY priority ASC, available_at ASC, id ASC
			LIMIT $2`,
			statusFilter, limit)
	case typeFilter != "":
		pgRows, err = deps.DB.Query(ctx, `
			SELECT id, type, priority, payload, available_at, attempts, status, COALESCE(last_error,'')
			FROM graph.jobs
			WHERE type = $1
			ORDER BY priority ASC, available_at ASC, id ASC
			LIMIT $2`,
			typeFilter, limit)
	default:
		pgRows, err = deps.DB.Query(ctx, `
			SELECT id, type, priority, payload, available_at, attempts, status, COALESCE(last_error,'')
			FROM graph.jobs
			ORDER BY priority ASC, available_at ASC, id ASC
			LIMIT $1`,
			limit)
	}
	if err != nil {
		return nil, err
	}
	defer pgRows.Close()

	var result []jobRowView
	for pgRows.Next() {
		var row jobRowView
		var availableAt time.Time
		if scanErr := pgRows.Scan(
			&row.ID, &row.Type, &row.Priority, &row.Payload,
			&availableAt, &row.Attempts, &row.Status, &row.LastError,
		); scanErr != nil {
			return nil, scanErr
		}
		row.AvailableAt = availableAt.UTC().Format(time.RFC3339)
		result = append(result, row)
	}
	return result, pgRows.Err()
}
