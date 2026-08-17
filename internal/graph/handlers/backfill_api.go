package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

var reChannelID = regexp.MustCompile(`^C[A-Z0-9]+$`)

// backfillSlackRequest is the request body for POST /api/graph/backfill/slack.
type backfillSlackRequest struct {
	ChannelID string `json:"channel_id"`
	Months    int    `json:"months"`
}

// backfillSlackResponse is the response body for POST /api/graph/backfill/slack.
type backfillSlackResponse struct {
	JobID           int64  `json:"job_id"`
	Status          string `json:"status"`
	ChannelID       string `json:"channel_id"`
	OldestTS        string `json:"oldest_ts"`
	EstimatedMonths int    `json:"estimated_months"`
}

// NewBackfillSlackHandler returns an http.Handler for POST /api/graph/backfill/slack.
func NewBackfillSlackHandler(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req backfillSlackRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}

		if !reChannelID.MatchString(req.ChannelID) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid channel_id %q; must match ^C[A-Z0-9]+$", req.ChannelID))
			return
		}
		if req.Months < 1 || req.Months > 24 {
			writeError(w, http.StatusBadRequest, "months must be between 1 and 24")
			return
		}

		// Compute oldest_ts as unix seconds with fractional ".000000".
		cutoff := time.Now().UTC().Add(-time.Duration(req.Months) * 30 * 24 * time.Hour)
		oldestTS := fmt.Sprintf("%d.000000", cutoff.Unix())

		payload := backfillSlackChannelPayload{
			ChannelID: req.ChannelID,
			OldestTS:  oldestTS,
			Cursor:    "",
		}

		jobID, err := jobs.Enqueue(r.Context(), deps.DB, "backfill_slack_channel", payload, jobs.EnqueueOptions{
			Priority:     5,
			TargetRunner: "vps",
			MachineID:    deps.MachineID,
		})
		if err != nil {
			deps.Logger.Error().Err(err).Msg("backfill_slack: enqueue failed")
			writeError(w, http.StatusInternalServerError, "enqueue failed: "+err.Error())
			return
		}

		resp := backfillSlackResponse{
			JobID:           jobID,
			Status:          "queued",
			ChannelID:       req.ChannelID,
			OldestTS:        oldestTS,
			EstimatedMonths: req.Months,
		}
		writeJSON(w, http.StatusAccepted, resp)
	})
}

// backfillAttachmentsRequest is the request body for POST
// /api/graph/backfill/attachments. Body is optional; an empty body uses the
// default cap.
type backfillAttachmentsRequest struct {
	Limit int `json:"limit"`
}

// backfillAttachmentsResponse is the response body for POST
// /api/graph/backfill/attachments.
type backfillAttachmentsResponse struct {
	Status   string `json:"status"`
	Matched  int    `json:"matched"`
	Enqueued int    `json:"enqueued"`
	Limit    int    `json:"limit"`
}

// NewBackfillAttachmentsHandler returns an http.Handler for POST
// /api/graph/backfill/attachments. It re-enqueues describe_attachment for the
// poisoned attachment rows (agent-mem-16e): explicitly triggered, capped, and
// deduped. Deliberately NOT wired into worker startup.
func NewBackfillAttachmentsHandler(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req backfillAttachmentsRequest
		// Optional body: ignore decode errors (empty/absent body -> default cap).
		_ = json.NewDecoder(r.Body).Decode(&req)

		limit := req.Limit
		if limit <= 0 {
			limit = backfillFailedAttachmentsDefaultLimit
		}
		if limit > 500 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 500")
			return
		}

		matched, enqueued := BackfillFailedAttachments(r.Context(), deps.DB, deps.Logger, limit)
		writeJSON(w, http.StatusAccepted, backfillAttachmentsResponse{
			Status:   "ok",
			Matched:  matched,
			Enqueued: enqueued,
			Limit:    limit,
		})
	})
}

// backfillStaleSummariesRequest is the request body for POST
// /api/graph/backfill/stale-summaries. Body is optional; an empty body uses the
// default cap.
type backfillStaleSummariesRequest struct {
	Limit int `json:"limit"`
}

// backfillStaleSummariesResponse is the response body for POST
// /api/graph/backfill/stale-summaries.
type backfillStaleSummariesResponse struct {
	Status    string `json:"status"`
	Matched   int    `json:"matched"`
	Enqueued  int    `json:"enqueued"`
	Remaining int    `json:"remaining"`
	Limit     int    `json:"limit"`
}

// backfillStaleSummariesDefaultLimit is the bounded batch size for the
// summary-only sweep. Each row costs one thread-summary generation and one
// embedding while topic-link LLM judging is skipped, so a 100-row page remains
// operator-controlled without the former judge-call cascade.
const backfillStaleSummariesDefaultLimit = 100

// resolveStaleSummariesLimit resolves an optional request limit to the effective
// cap: a non-positive value falls back to the default, and a value over the 500
// ceiling is rejected (ok=false -> 400). Extracted so the clamp is unit-testable
// without a database pool.
func resolveStaleSummariesLimit(reqLimit int) (limit int, ok bool) {
	limit = reqLimit
	if limit <= 0 {
		limit = backfillStaleSummariesDefaultLimit
	}
	if limit > 500 {
		return 0, false
	}
	return limit, true
}

// NewBackfillStaleSummariesHandler returns an http.Handler for POST
// /api/graph/backfill/stale-summaries. It re-enqueues summarize_thread for cached
// rows whose signature is not on the current threadSummarySigVersion — the rows a
// version bump strands, because staleness is otherwise only detected lazily on a
// channel view and BackfillMissingThreadSummaries only covers threads with NO row
// (agent-mem-8q4). Like NewBackfillAttachmentsHandler this is a bulk LLM re-run:
// explicitly triggered, capped, and deduped. Deliberately NOT wired into worker
// startup or any scheduled job.
func NewBackfillStaleSummariesHandler(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req backfillStaleSummariesRequest
		// Optional body: ignore decode errors (empty/absent body -> default cap).
		_ = json.NewDecoder(r.Body).Decode(&req)

		limit, ok := resolveStaleSummariesLimit(req.Limit)
		if !ok {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 500")
			return
		}

		matched, enqueued, remaining := BackfillStaleThreadSummaries(r.Context(), deps.DB, limit)
		writeJSON(w, http.StatusAccepted, backfillStaleSummariesResponse{
			Status:    "ok",
			Matched:   matched,
			Enqueued:  enqueued,
			Remaining: remaining,
			Limit:     limit,
		})
	})
}
