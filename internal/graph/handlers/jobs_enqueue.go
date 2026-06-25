package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// enqueuableTypes is the allowlist of job types the admin enqueue endpoint may
// trigger. Kept narrow (maintenance/refresh jobs) so the API-key boundary can't
// be used to inject arbitrary work.
var enqueuableTypes = map[string]bool{
	"backfill_created_at":    true,
	"refresh_slack_channels": true,
	"refresh_slack_users":    true,
	"refresh_slack_groups":   true,
	"import_bamboohr":        true, // payload: {csv_path} or {csv_bytes}
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
		id, err := jobs.EnqueueRaw(r.Context(), deps.DB, req.Type, payload,
			jobs.EnqueueOptions{MachineID: deps.MachineID})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": id, "type": req.Type})
	})
}
