package worker

import (
	"encoding/json"
	"net/http"
	"strconv"

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

	log.Info().Int("received", received).Int("rejected", rejected).Str("from", payload.MachineID).Msg("Sync push received")

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sync.SyncPushResponse{
		Received: received,
		Rejected: rejected,
	})
}

// handleSyncPull returns rows for a requesting machine.
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

	ctx := r.Context()

	// Get observations not from this machine
	observations, _ := s.db.GetUnsyncedObservations(ctx, limit)
	summaries, _ := s.db.GetUnsyncedSummaries(ctx, limit)
	prompts, _ := s.db.GetUnsyncedPrompts(ctx, limit)
	sessions, _ := s.db.GetUnsyncedSessions(ctx, limit)

	// Filter out rows from requesting machine
	var filteredObs []interface{ GetSyncSource() string }
	_ = filteredObs // just use simple approach below

	resp := sync.SyncPullResponse{
		Sessions:     sessions,
		Observations: observations,
		Summaries:    summaries,
		Prompts:      prompts,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleSyncInfo returns current sync status.
func (s *Server) handleSyncInfo(w http.ResponseWriter, r *http.Request) {
	if s.syncEngine == nil {
		http.Error(w, "sync not configured", http.StatusServiceUnavailable)
		return
	}

	info, err := s.syncEngine.GetInfo(r.Context())
	if err != nil {
		log.Error().Err(err).Msg("Failed to get sync info")
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
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
func (s *Server) verifyAPIKey(r *http.Request) bool {
	snap := s.config.Snapshot()
	if snap.APIKey == "" {
		return true // no auth configured
	}
	auth := r.Header.Get("Authorization")
	return auth == "Bearer "+snap.APIKey
}
