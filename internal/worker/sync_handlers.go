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

	// Graph tables (FK-ordered: people → nodes → edges, then the rest)
	for i := range payload.GraphPeople {
		if err := s.db.ImportGraphPerson(ctx, &payload.GraphPeople[i]); err != nil {
			rejected++
		} else {
			received++
		}
	}
	for i := range payload.GraphNodes {
		if err := s.db.ImportGraphNode(ctx, &payload.GraphNodes[i]); err != nil {
			rejected++
		} else {
			received++
		}
	}
	for i := range payload.GraphEdges {
		if err := s.db.ImportGraphEdge(ctx, &payload.GraphEdges[i]); err != nil {
			rejected++
		} else {
			received++
		}
	}
	for i := range payload.GraphArtifactIndex {
		if err := s.db.ImportGraphArtifactIndex(ctx, &payload.GraphArtifactIndex[i]); err != nil {
			rejected++
		} else {
			received++
		}
	}
	for i := range payload.GraphArtifactBodies {
		if err := s.db.ImportGraphArtifactBody(ctx, &payload.GraphArtifactBodies[i]); err != nil {
			rejected++
		} else {
			received++
		}
	}
	for i := range payload.GraphSlackGroups {
		if err := s.db.ImportGraphSlackGroup(ctx, &payload.GraphSlackGroups[i]); err != nil {
			rejected++
		} else {
			received++
		}
	}
	for i := range payload.GraphEntities {
		if err := s.db.ImportGraphEntity(ctx, &payload.GraphEntities[i]); err != nil {
			rejected++
		} else {
			received++
		}
	}
	for i := range payload.GraphJobs {
		if err := s.db.ImportGraphJob(ctx, &payload.GraphJobs[i]); err != nil {
			rejected++
		} else {
			received++
		}
	}
	for i := range payload.GraphUserAffinityConfig {
		if err := s.db.ImportGraphUserAffinityConfig(ctx, &payload.GraphUserAffinityConfig[i]); err != nil {
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

	// Graph cursors: int keyset for people/edges/jobs (monotonic BIGSERIAL id).
	// The other six paginate on (timestamp, pk), transported as "<RFC3339Nano>|<pk>"
	// in one query param and decoded here (empty/unparseable => from the beginning).
	gPeopleAfter, _ := strconv.Atoi(r.URL.Query().Get("g_people_after"))
	gNodesTS, gNodesPK := sync.DecodeCursor(r.URL.Query().Get("g_nodes_after"))
	gEdgesAfter, _ := strconv.Atoi(r.URL.Query().Get("g_edges_after"))
	gArtIdxTS, gArtIdxPK := sync.DecodeCursor(r.URL.Query().Get("g_artidx_after"))
	gArtBodyTS, gArtBodyPK := sync.DecodeCursor(r.URL.Query().Get("g_artbody_after"))
	gSlackGrpTS, gSlackGrpPK := sync.DecodeCursor(r.URL.Query().Get("g_slackgrp_after"))
	gEntitiesTS, gEntitiesPK := sync.DecodeCursor(r.URL.Query().Get("g_entities_after"))
	gJobsAfter, _ := strconv.Atoi(r.URL.Query().Get("g_jobs_after"))
	gAffinityTS, gAffinityPKStr := sync.DecodeCursor(r.URL.Query().Get("g_affinity_after"))
	gAffinityAfter, _ := strconv.Atoi(gAffinityPKStr) // eeid; 0 (empty/unparseable) starts from the beginning

	ctx := r.Context()

	observations, _ := s.db.GetObservationsForPull(ctx, machineID, obsAfter, limit)
	summaries, _ := s.db.GetSummariesForPull(ctx, machineID, sumAfter, limit)
	prompts, _ := s.db.GetPromptsForPull(ctx, machineID, promptAfter, limit)
	sessions, _ := s.db.GetSessionsForPull(ctx, machineID, sessAfter, limit)

	// Graph tables
	graphPeople, _ := s.db.GetGraphPeopleForPull(ctx, machineID, gPeopleAfter, limit)
	graphNodes, _ := s.db.GetGraphNodesForPull(ctx, machineID, gNodesTS, gNodesPK, limit)
	graphEdges, _ := s.db.GetGraphEdgesForPull(ctx, machineID, gEdgesAfter, limit)
	graphArtIdx, _ := s.db.GetGraphArtifactIndexForPull(ctx, machineID, gArtIdxTS, gArtIdxPK, limit/2)
	graphArtBody, _ := s.db.GetGraphArtifactBodiesForPull(ctx, machineID, gArtBodyTS, gArtBodyPK, limit/2)
	graphSlackGrp, _ := s.db.GetGraphSlackGroupsForPull(ctx, machineID, gSlackGrpTS, gSlackGrpPK, limit)
	graphEntities, _ := s.db.GetGraphEntitiesForPull(ctx, machineID, gEntitiesTS, gEntitiesPK, limit)
	graphJobs, _ := s.db.GetGraphJobsForPull(ctx, machineID, gJobsAfter, limit)
	graphAffinity, _ := s.db.GetGraphUserAffinityConfigForPull(ctx, machineID, gAffinityTS, gAffinityAfter, limit)

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

	// Graph cursors: the last returned row's key per table (keyset, never offset).
	if len(graphPeople) > 0 {
		cursors.GraphPeople = int(graphPeople[len(graphPeople)-1].ID)
	}
	if len(graphEdges) > 0 {
		cursors.GraphEdges = int(graphEdges[len(graphEdges)-1].ID)
	}
	if len(graphJobs) > 0 {
		cursors.GraphJobs = int(graphJobs[len(graphJobs)-1].ID)
	}
	// Keyset cursors: the last returned row's (timestamp, pk), encoded as one
	// string. ORDER BY guarantees the last row holds the max pair in the batch.
	if len(graphNodes) > 0 {
		last := graphNodes[len(graphNodes)-1]
		cursors.GraphNodes = sync.EncodeCursor(last.UpdatedAt, last.ID)
	}
	if len(graphArtIdx) > 0 {
		last := graphArtIdx[len(graphArtIdx)-1]
		cursors.GraphArtifactIndex = sync.EncodeCursor(last.RefreshedAt, last.NodeID)
	}
	if len(graphArtBody) > 0 {
		last := graphArtBody[len(graphArtBody)-1]
		cursors.GraphArtifactBodies = sync.EncodeCursor(last.FetchedAt, last.NodeID)
	}
	if len(graphSlackGrp) > 0 {
		last := graphSlackGrp[len(graphSlackGrp)-1]
		cursors.GraphSlackGroups = sync.EncodeCursor(last.RefreshedAt, last.ID)
	}
	if len(graphEntities) > 0 {
		last := graphEntities[len(graphEntities)-1]
		cursors.GraphEntities = sync.EncodeCursor(last.FirstSeenAt, last.ID)
	}
	if len(graphAffinity) > 0 {
		last := graphAffinity[len(graphAffinity)-1]
		cursors.GraphUserAffinityConfig = sync.EncodeCursor(last.UpdatedAt, strconv.Itoa(last.EEID))
	}

	resp := sync.SyncPullResponse{
		Sessions:                sessions,
		Observations:            observations,
		Summaries:               summaries,
		Prompts:                 prompts,
		GraphPeople:             graphPeople,
		GraphNodes:              graphNodes,
		GraphEdges:              graphEdges,
		GraphArtifactIndex:      graphArtIdx,
		GraphArtifactBodies:     graphArtBody,
		GraphSlackGroups:        graphSlackGrp,
		GraphEntities:           graphEntities,
		GraphJobs:               graphJobs,
		GraphUserAffinityConfig: graphAffinity,
		Cursors:                 cursors,
	}

	// Record per-client pull time for cloud dashboard
	s.db.SetLastSyncTime(ctx, "client_pull:"+machineID)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleSyncPullDerived serves derived graph tables (thread_summaries,
// slack_users, slack_channels) by timestamp cursor. Registered inside the
// authenticated route group alongside /api/sync/pull.
func (s *Server) handleSyncPullDerived(w http.ResponseWriter, r *http.Request) {
	parseTS := func(param string) time.Time {
		t, err := time.Parse(time.RFC3339Nano, r.URL.Query().Get(param))
		if err != nil {
			return time.Time{}
		}
		return t
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 5000 {
		limit = 5000
	}

	ctx := r.Context()
	threadSummaries, _ := s.db.GetThreadSummariesSince(ctx, parseTS("ts_after"), limit)
	slackUsers, _ := s.db.GetSlackUsersSince(ctx, parseTS("su_after"), limit)
	slackChannels, _ := s.db.GetSlackChannelsSince(ctx, parseTS("sc_after"), limit)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sync.DerivedPullResponse{
		ThreadSummaries: threadSummaries,
		SlackUsers:      slackUsers,
		SlackChannels:   slackChannels,
	})
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
