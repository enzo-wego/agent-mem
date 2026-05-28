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
