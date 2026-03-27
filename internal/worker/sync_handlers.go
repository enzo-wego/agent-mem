package worker

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/agent-mem/agent-mem/internal/sync"
)

// handleSyncPush receives data pushed from another machine.
func (s *Server) handleSyncPush(w http.ResponseWriter, r *http.Request) {
	var payload sync.SyncPushPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	received, rejected := 0, 0
	ctx := r.Context()

	for i := range payload.Sessions {
		if err := s.db.ImportSession(ctx, &payload.Sessions[i]); err != nil {
			rejected++
		} else {
			received++
		}
	}
	for i := range payload.Observations {
		if err := s.db.ImportObservation(ctx, &payload.Observations[i]); err != nil {
			rejected++
		} else {
			received++
		}
	}
	for i := range payload.Summaries {
		if err := s.db.ImportSummary(ctx, &payload.Summaries[i]); err != nil {
			rejected++
		} else {
			received++
		}
	}
	for i := range payload.Prompts {
		if err := s.db.ImportPrompt(ctx, &payload.Prompts[i]); err != nil {
			rejected++
		} else {
			received++
		}
	}

	// Record per-client push time for cloud dashboard
	if payload.MachineID != "" {
		s.db.SetLastSyncTime(ctx, "client_push:"+payload.MachineID)
	}

	log.Info().Int("received", received).Int("rejected", rejected).Str("from", payload.MachineID).Msg("Sync push received")

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sync.SyncPushResponse{
		Received: received,
		Rejected: rejected,
	})
}

// handleSyncPull returns rows for a requesting machine using cursor-based pagination.
func (s *Server) handleSyncPull(w http.ResponseWriter, r *http.Request) {
	machineID := r.URL.Query().Get("machine_id")
	limit := 100
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = n
	}

	if machineID == "" {
		http.Error(w, "missing machine_id", http.StatusBadRequest)
		return
	}

	// Per-table cursors: cloud-side IDs from previous pull
	obsAfter, _ := strconv.Atoi(r.URL.Query().Get("obs_after"))
	sumAfter, _ := strconv.Atoi(r.URL.Query().Get("sum_after"))
	promptAfter, _ := strconv.Atoi(r.URL.Query().Get("prompt_after"))
	sessAfter, _ := strconv.Atoi(r.URL.Query().Get("sess_after"))

	ctx := r.Context()

	observations, _ := s.db.GetObservationsForPull(ctx, machineID, obsAfter, limit)
	summaries, _ := s.db.GetSummariesForPull(ctx, machineID, sumAfter, limit)
	prompts, _ := s.db.GetPromptsForPull(ctx, machineID, promptAfter, limit)
	sessions, _ := s.db.GetSessionsForPull(ctx, machineID, sessAfter, limit)

	// Compute cursors: max ID per table
	cursors := sync.PullCursors{}
	if len(observations) > 0 {
		cursors.Observations = observations[len(observations)-1].ID
	}
	if len(summaries) > 0 {
		cursors.Summaries = summaries[len(summaries)-1].ID
	}
	if len(prompts) > 0 {
		cursors.Prompts = prompts[len(prompts)-1].ID
	}
	if len(sessions) > 0 {
		cursors.Sessions = sessions[len(sessions)-1].ID
	}

	resp := sync.SyncPullResponse{
		Sessions:     sessions,
		Observations: observations,
		Summaries:    summaries,
		Prompts:      prompts,
		Cursors:      cursors,
	}

	// Record per-client pull time for cloud dashboard
	s.db.SetLastSyncTime(ctx, "client_pull:"+machineID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleSyncInfo returns current sync status.
// Works in both local mode (with sync engine) and cloud mode (receive-only).
func (s *Server) handleSyncInfo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// If sync engine is running (local mode), use it
	if s.syncEngine != nil {
		info, err := s.syncEngine.GetInfo(ctx)
		if err != nil {
			log.Error().Err(err).Msg("Failed to get sync info")
			http.Error(w, "error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(info)
		return
	}

	// Cloud mode: show server totals and per-client sync times
	snap := s.config.Snapshot()
	stats, err := s.db.GetSyncStats(ctx)
	if err != nil {
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	// Cloud only shows totals, unsynced is not meaningful
	for i := range stats {
		stats[i].Unsynced = 0
	}

	info := sync.SyncInfo{
		Mode:      "cloud",
		MachineID: snap.MachineID,
		Stats:     stats,
	}

	// Per-client timestamps
	clientTimes, err := s.db.GetClientSyncTimes(ctx)
	if err == nil {
		for _, ct := range clientTimes {
			info.Clients = append(info.Clients, sync.ClientInfo{
				MachineID: ct.MachineID,
				LastPush:  ct.LastPush,
				LastPull:  ct.LastPull,
			})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

// handleSyncCloudStats proxies a stats request to the cloud sync URL using
// the server's configured API key, so the dashboard doesn't need the key.
func (s *Server) handleSyncCloudStats(w http.ResponseWriter, r *http.Request) {
	snap := s.config.Snapshot()
	if snap.SyncURL == "" {
		http.Error(w, `{"error":"sync not configured"}`, http.StatusServiceUnavailable)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, snap.SyncURL+"/api/stats", nil)
	if err != nil {
		http.Error(w, `{"error":"failed to create request"}`, http.StatusInternalServerError)
		return
	}
	if snap.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+snap.APIKey)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, `{"error":"cloud unreachable"}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// apiKeyMiddleware is a chi middleware that rejects requests when an API key
// is configured and the request does not carry a matching Bearer token.
func (s *Server) apiKeyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.verifyAPIKey(r) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// verifyAPIKey checks the Authorization header against the configured API key.
// Only enforced in cloud mode (api_key set, no sync_url). Local instances that
// have api_key + sync_url are sync clients and don't require auth on their own API.
func (s *Server) verifyAPIKey(r *http.Request) bool {
	snap := s.config.Snapshot()
	if snap.APIKey == "" || snap.SyncURL != "" {
		return true // no auth needed: either no key or local (sync client) mode
	}
	auth := r.Header.Get("Authorization")
	return auth == "Bearer "+snap.APIKey
}
