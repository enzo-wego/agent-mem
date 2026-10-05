package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// reEpicBriefKey is the Jira key shape of an epic the brief job accepts.
var reEpicBriefKey = regexp.MustCompile(`^[A-Z][A-Z0-9]+-[0-9]+$`)

// enqueuableTypes is the allowlist of job types the admin enqueue endpoint may
// trigger. Kept narrow (maintenance/refresh jobs) so the API-key boundary can't
// be used to inject arbitrary work.
var enqueuableTypes = map[string]bool{
	"backfill_created_at":      true,
	"backfill_artifact_tsv":    true, // payload: {batch} (default 500, 1-5000)
	"refresh_jira_board":       true,
	"refresh_jira_updates":     true,
	"refresh_slack_channels":   true,
	"refresh_slack_members":    true, // payload: {"force":true} for immediate/manual queue retries
	"refresh_slack_users":      true,
	"refresh_slack_bots":       true,
	"refresh_slack_groups":     true,
	"derive_person_roles":      true,
	"import_bamboohr":          true, // payload: {csv_path} or {csv_bytes}
	"merge_identities_by_name": true,
	"refresh_epic_brief":       true, // dry run with an epic_key only
}

type jobsEnqueueRequest struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// NewJobsEnqueueHandler returns an http.Handler for POST /api/graph/jobs/enqueue.
// It lets a trusted (API-key bearing) caller trigger a maintenance job without
// direct DB access — e.g. backfill_created_at or refresh_slack_channels.
func NewJobsEnqueueHandler(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jobsEnqueueRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
			return
		}
		if !enqueuableTypes[req.Type] {
			http.Error(w, `{"error":"job type not allowed"}`, http.StatusBadRequest)
			return
		}
		payload := []byte(req.Payload)
		if len(payload) == 0 {
			payload = []byte("{}")
		}
		if req.Type == "refresh_epic_brief" {
			// A real build spends LLM calls and writes briefs: the canary
			// path is dry-run-only; the scheduled path enqueues real runs.
			var p refreshEpicBriefPayload
			if err := json.Unmarshal(payload, &p); err != nil || !p.DryRun || !reEpicBriefKey.MatchString(strings.ToUpper(strings.TrimSpace(p.EpicKey))) {
				http.Error(w, `{"error":"refresh_epic_brief is only enqueuable as a dry run with an epic_key"}`, http.StatusBadRequest)
				return
			}
		}
		var id int64
		var err error
		if jobs.IsPeriodic(req.Type) {
			id, err = jobs.EnqueuePeriodicNow(r.Context(), deps.DB, req.Type, deps.MachineID, deps.Runner)
		} else {
			id, err = jobs.EnqueueRaw(r.Context(), deps.DB, req.Type, payload,
				jobs.EnqueueOptions{MachineID: deps.MachineID})
		}
		if errors.Is(err, jobs.ErrPeriodicPending) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		if err != nil {
			if jobs.IsPeriodic(req.Type) {
				writeError(w, http.StatusInternalServerError, "periodic enqueue outcome is unknown; check the jobs list before retrying")
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": id, "type": req.Type})
	})
}
