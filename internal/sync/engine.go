package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/agent-mem/agent-mem/internal/config"
	"github.com/agent-mem/agent-mem/internal/database"
)

const batchSize = 100

// SyncPushPayload is the data sent from local to cloud.
type SyncPushPayload struct {
	MachineID    string                         `json:"machine_id"`
	Sessions     []database.SdkSession          `json:"sessions,omitempty"`
	Observations []database.SyncableObservation `json:"observations,omitempty"`
	Summaries    []database.SyncableSummary     `json:"summaries,omitempty"`
	Prompts      []database.SyncablePrompt      `json:"prompts,omitempty"`

	// Graph tables (FK-ordered: people → nodes → edges, then the rest)
	GraphPeople             []database.SyncableGraphPerson             `json:"graph_people,omitempty"`
	GraphNodes              []database.SyncableGraphNode               `json:"graph_nodes,omitempty"`
	GraphEdges              []database.SyncableGraphEdge               `json:"graph_edges,omitempty"`
	GraphArtifactIndex      []database.SyncableGraphArtifactIndex      `json:"graph_artifact_index,omitempty"`
	GraphArtifactBodies     []database.SyncableGraphArtifactBody       `json:"graph_artifact_bodies,omitempty"`
	GraphSlackGroups        []database.SyncableGraphSlackGroup         `json:"graph_slack_groups,omitempty"`
	GraphEntities           []database.SyncableGraphEntity             `json:"graph_entities,omitempty"`
	GraphJobs               []database.SyncableGraphJob                `json:"graph_jobs,omitempty"`
	GraphUserAffinityConfig []database.SyncableGraphUserAffinityConfig `json:"graph_user_affinity_config,omitempty"`
}

// SyncPushResponse is the response from the cloud after a push.
type SyncPushResponse struct {
	Received int `json:"received"`
	Rejected int `json:"rejected"`
}

// PullCursors holds per-table cloud-side IDs for cursor-based pull pagination.
type PullCursors struct {
	Observations int `json:"observations"`
	Summaries    int `json:"summaries"`
	Prompts      int `json:"prompts"`
	Sessions     int `json:"sessions"`

	// Graph table cursors (ID-based for tables with int PKs; offset-based for text-PK tables)
	GraphPeople             int `json:"graph_people"`
	GraphNodes              int `json:"graph_nodes"`
	GraphEdges              int `json:"graph_edges"`
	GraphArtifactIndex      int `json:"graph_artifact_index"`
	GraphArtifactBodies     int `json:"graph_artifact_bodies"`
	GraphSlackGroups        int `json:"graph_slack_groups"`
	GraphEntities           int `json:"graph_entities"`
	GraphJobs               int `json:"graph_jobs"`
	GraphUserAffinityConfig int `json:"graph_user_affinity_config"`
}

// SyncPullResponse is the data received from cloud during pull.
type SyncPullResponse struct {
	Sessions     []database.SdkSession          `json:"sessions,omitempty"`
	Observations []database.SyncableObservation `json:"observations,omitempty"`
	Summaries    []database.SyncableSummary     `json:"summaries,omitempty"`
	Prompts      []database.SyncablePrompt      `json:"prompts,omitempty"`
	Cursors      PullCursors                    `json:"cursors"`

	// Graph tables
	GraphPeople             []database.SyncableGraphPerson             `json:"graph_people,omitempty"`
	GraphNodes              []database.SyncableGraphNode               `json:"graph_nodes,omitempty"`
	GraphEdges              []database.SyncableGraphEdge               `json:"graph_edges,omitempty"`
	GraphArtifactIndex      []database.SyncableGraphArtifactIndex      `json:"graph_artifact_index,omitempty"`
	GraphArtifactBodies     []database.SyncableGraphArtifactBody       `json:"graph_artifact_bodies,omitempty"`
	GraphSlackGroups        []database.SyncableGraphSlackGroup         `json:"graph_slack_groups,omitempty"`
	GraphEntities           []database.SyncableGraphEntity             `json:"graph_entities,omitempty"`
	GraphJobs               []database.SyncableGraphJob                `json:"graph_jobs,omitempty"`
	GraphUserAffinityConfig []database.SyncableGraphUserAffinityConfig `json:"graph_user_affinity_config,omitempty"`
}

// ClientInfo holds per-client sync timestamps (cloud mode).
type ClientInfo struct {
	MachineID string     `json:"machine_id"`
	LastPush  *time.Time `json:"last_push,omitempty"`
	LastPull  *time.Time `json:"last_pull,omitempty"`
}

// SyncInfo holds current sync status for the info endpoint.
type SyncInfo struct {
	Mode         string               `json:"mode"`
	MachineID    string               `json:"machine_id"`
	SyncEnabled  bool                 `json:"sync_enabled"`
	SyncInterval string               `json:"sync_interval,omitempty"`
	Stats        []database.SyncStats `json:"stats"`
	LastPush     *time.Time           `json:"last_push,omitempty"`
	LastPull     *time.Time           `json:"last_pull,omitempty"`
	Clients      []ClientInfo         `json:"clients,omitempty"`
}

// Engine manages push/pull sync between local and cloud.
type Engine struct {
	db     *database.DB
	config *config.Config
	client *http.Client
	ticker *time.Ticker
}

// NewEngine creates a new sync engine.
func NewEngine(db *database.DB, cfg *config.Config) *Engine {
	return &Engine{
		db:     db,
		config: cfg,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// Start runs the sync loop (blocking).
func (e *Engine) Start(ctx context.Context) {
	interval, err := time.ParseDuration(e.config.SyncInterval)
	if err != nil {
		interval = 60 * time.Second
	}
	e.ticker = time.NewTicker(interval)
	defer e.ticker.Stop()

	log.Info().Dur("interval", interval).Msg("Sync engine started")

	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("Sync engine stopped")
			return
		case <-e.ticker.C:
			if err := e.push(ctx); err != nil {
				log.Error().Err(err).Msg("Sync push failed")
			}
			if err := e.pull(ctx); err != nil {
				log.Error().Err(err).Msg("Sync pull failed")
			}
			if err := e.pullDerived(ctx); err != nil {
				log.Error().Err(err).Msg("Sync pull_derived failed")
			}
		}
	}
}

const (
	batchSizeDefault       = 100
	batchSizeArtifactLarge = 50 // artifact_bodies and artifact_index have large payloads
)

func (e *Engine) push(ctx context.Context) error {
	sessions, _ := e.db.GetUnsyncedSessions(ctx, batchSize)
	observations, _ := e.db.GetUnsyncedObservations(ctx, batchSize)
	summaries, _ := e.db.GetUnsyncedSummaries(ctx, batchSize)
	prompts, _ := e.db.GetUnsyncedPrompts(ctx, batchSize)

	// Graph tables (FK-ordered)
	graphPeople, _ := e.db.GetUnsyncedGraphPeople(ctx, batchSizeDefault)
	graphNodes, _ := e.db.GetUnsyncedGraphNodes(ctx, batchSizeDefault)
	graphEdges, _ := e.db.GetUnsyncedGraphEdges(ctx, batchSizeDefault)
	graphArtifactIndex, _ := e.db.GetUnsyncedGraphArtifactIndex(ctx, batchSizeArtifactLarge)
	graphArtifactBodies, _ := e.db.GetUnsyncedGraphArtifactBodies(ctx, batchSizeArtifactLarge)
	graphSlackGroups, _ := e.db.GetUnsyncedGraphSlackGroups(ctx, batchSizeDefault)
	graphEntities, _ := e.db.GetUnsyncedGraphEntities(ctx, batchSizeDefault)
	graphJobs, _ := e.db.GetUnsyncedGraphJobs(ctx, batchSizeDefault)
	graphUserAffinity, _ := e.db.GetUnsyncedGraphUserAffinityConfig(ctx, batchSizeDefault)

	total := len(sessions) + len(observations) + len(summaries) + len(prompts) +
		len(graphPeople) + len(graphNodes) + len(graphEdges) +
		len(graphArtifactIndex) + len(graphArtifactBodies) +
		len(graphSlackGroups) + len(graphEntities) + len(graphJobs) + len(graphUserAffinity)

	// Always push (even empty) so cloud tracks client heartbeat
	payload := SyncPushPayload{
		MachineID:               e.config.MachineID,
		Sessions:                sessions,
		Observations:            observations,
		Summaries:               summaries,
		Prompts:                 prompts,
		GraphPeople:             graphPeople,
		GraphNodes:              graphNodes,
		GraphEdges:              graphEdges,
		GraphArtifactIndex:      graphArtifactIndex,
		GraphArtifactBodies:     graphArtifactBodies,
		GraphSlackGroups:        graphSlackGroups,
		GraphEntities:           graphEntities,
		GraphJobs:               graphJobs,
		GraphUserAffinityConfig: graphUserAffinity,
	}

	resp, err := e.postJSON(ctx, e.config.SyncURL+"/api/sync/push", payload)
	if err != nil {
		return fmt.Errorf("push: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("push returned %d: %s", resp.StatusCode, string(body))
	}

	var pushResp SyncPushResponse
	json.NewDecoder(resp.Body).Decode(&pushResp)

	// Mark synced — original tables
	syncVer := int(time.Now().Unix())
	if len(sessions) > 0 {
		e.db.MarkSynced(ctx, "sdk_sessions", syncIDs(sessions), syncVer)
	}
	if len(observations) > 0 {
		e.db.MarkSynced(ctx, "observations", syncObsIDs(observations), syncVer)
	}
	if len(summaries) > 0 {
		e.db.MarkSynced(ctx, "session_summaries", syncSumIDs(summaries), syncVer)
	}
	if len(prompts) > 0 {
		e.db.MarkSynced(ctx, "user_prompts", syncPromptIDs(prompts), syncVer)
	}

	// Mark synced — graph tables
	syncVer64 := int64(syncVer)
	if len(graphPeople) > 0 {
		e.db.MarkSyncedGraphBySyncID(ctx, "graph", "people", graphSyncIDs(graphPeople, func(p database.SyncableGraphPerson) string { return p.SyncID }), syncVer64)
	}
	if len(graphNodes) > 0 {
		e.db.MarkSyncedGraphBySyncID(ctx, "graph", "nodes", graphSyncIDs(graphNodes, func(n database.SyncableGraphNode) string { return n.SyncID }), syncVer64)
	}
	if len(graphEdges) > 0 {
		e.db.MarkSyncedGraphBySyncID(ctx, "graph", "edges", graphSyncIDs(graphEdges, func(e database.SyncableGraphEdge) string { return e.SyncID }), syncVer64)
	}
	if len(graphArtifactIndex) > 0 {
		e.db.MarkSyncedGraphBySyncID(ctx, "graph", "artifact_index", graphSyncIDs(graphArtifactIndex, func(ai database.SyncableGraphArtifactIndex) string { return ai.SyncID }), syncVer64)
	}
	if len(graphArtifactBodies) > 0 {
		e.db.MarkSyncedGraphBySyncID(ctx, "graph", "artifact_bodies", graphSyncIDs(graphArtifactBodies, func(ab database.SyncableGraphArtifactBody) string { return ab.SyncID }), syncVer64)
	}
	if len(graphSlackGroups) > 0 {
		e.db.MarkSyncedGraphBySyncID(ctx, "graph", "slack_groups", graphSyncIDs(graphSlackGroups, func(sg database.SyncableGraphSlackGroup) string { return sg.SyncID }), syncVer64)
	}
	if len(graphEntities) > 0 {
		e.db.MarkSyncedGraphBySyncID(ctx, "graph", "entities", graphSyncIDs(graphEntities, func(e database.SyncableGraphEntity) string { return e.SyncID }), syncVer64)
	}
	if len(graphJobs) > 0 {
		e.db.MarkSyncedGraphBySyncID(ctx, "graph", "jobs", graphSyncIDs(graphJobs, func(j database.SyncableGraphJob) string { return j.SyncID }), syncVer64)
	}
	if len(graphUserAffinity) > 0 {
		e.db.MarkSyncedGraphBySyncID(ctx, "graph", "user_affinity_config", graphSyncIDs(graphUserAffinity, func(c database.SyncableGraphUserAffinityConfig) string { return c.SyncID }), syncVer64)
	}

	e.db.SetLastSyncTime(ctx, "last_push")
	log.Info().Int("total", total).Int("received", pushResp.Received).Msg("Sync push complete")
	return nil
}

func (e *Engine) pull(ctx context.Context) error {
	totalImported := 0

	for {
		// Load per-table cursors from settings
		obsCursor := e.getPullCursor(ctx, "observations")
		sumCursor := e.getPullCursor(ctx, "summaries")
		promptCursor := e.getPullCursor(ctx, "prompts")
		sessCursor := e.getPullCursor(ctx, "sessions")

		// Graph cursors
		gPeopleCursor := e.getPullCursor(ctx, "graph.people")
		gNodesCursor := e.getPullCursor(ctx, "graph.nodes")
		gEdgesCursor := e.getPullCursor(ctx, "graph.edges")
		gArtIdxCursor := e.getPullCursor(ctx, "graph.artifact_index")
		gArtBodyCursor := e.getPullCursor(ctx, "graph.artifact_bodies")
		gSlackGrpCursor := e.getPullCursor(ctx, "graph.slack_groups")
		gEntitiesCursor := e.getPullCursor(ctx, "graph.entities")
		gJobsCursor := e.getPullCursor(ctx, "graph.jobs")
		gAffinityCursor := e.getPullCursor(ctx, "graph.user_affinity_config")

		pullURL := fmt.Sprintf(
			"%s/api/sync/pull?machine_id=%s&limit=%d"+
				"&obs_after=%d&sum_after=%d&prompt_after=%d&sess_after=%d"+
				"&g_people_after=%d&g_nodes_after=%d&g_edges_after=%d"+
				"&g_artidx_after=%d&g_artbody_after=%d&g_slackgrp_after=%d"+
				"&g_entities_after=%d&g_jobs_after=%d&g_affinity_after=%d",
			e.config.SyncURL, e.config.MachineID, batchSize,
			obsCursor, sumCursor, promptCursor, sessCursor,
			gPeopleCursor, gNodesCursor, gEdgesCursor,
			gArtIdxCursor, gArtBodyCursor, gSlackGrpCursor,
			gEntitiesCursor, gJobsCursor, gAffinityCursor,
		)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, pullURL, nil)
		if err != nil {
			return fmt.Errorf("create pull request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+e.config.APIKey)

		resp, err := e.client.Do(req)
		if err != nil {
			return fmt.Errorf("pull: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return fmt.Errorf("pull returned %d: %s", resp.StatusCode, string(body))
		}

		var pullResp SyncPullResponse
		if err := json.NewDecoder(resp.Body).Decode(&pullResp); err != nil {
			resp.Body.Close()
			return fmt.Errorf("decode pull response: %w", err)
		}
		resp.Body.Close()

		batchTotal := len(pullResp.Sessions) + len(pullResp.Observations) +
			len(pullResp.Summaries) + len(pullResp.Prompts) +
			len(pullResp.GraphPeople) + len(pullResp.GraphNodes) + len(pullResp.GraphEdges) +
			len(pullResp.GraphArtifactIndex) + len(pullResp.GraphArtifactBodies) +
			len(pullResp.GraphSlackGroups) + len(pullResp.GraphEntities) +
			len(pullResp.GraphJobs) + len(pullResp.GraphUserAffinityConfig)
		if batchTotal == 0 {
			break // fully caught up
		}

		// Import original tables
		for i := range pullResp.Sessions {
			if err := e.db.ImportSession(ctx, &pullResp.Sessions[i]); err == nil {
				totalImported++
			}
		}
		for i := range pullResp.Observations {
			if err := e.db.ImportObservation(ctx, &pullResp.Observations[i]); err == nil {
				totalImported++
			}
		}
		for i := range pullResp.Summaries {
			if err := e.db.ImportSummary(ctx, &pullResp.Summaries[i]); err == nil {
				totalImported++
			}
		}
		for i := range pullResp.Prompts {
			if err := e.db.ImportPrompt(ctx, &pullResp.Prompts[i]); err == nil {
				totalImported++
			}
		}

		// Import graph tables (FK-ordered)
		for i := range pullResp.GraphPeople {
			if err := e.db.ImportGraphPerson(ctx, &pullResp.GraphPeople[i]); err == nil {
				totalImported++
			}
		}
		for i := range pullResp.GraphNodes {
			if err := e.db.ImportGraphNode(ctx, &pullResp.GraphNodes[i]); err == nil {
				totalImported++
			}
		}
		for i := range pullResp.GraphEdges {
			if err := e.db.ImportGraphEdge(ctx, &pullResp.GraphEdges[i]); err == nil {
				totalImported++
			}
		}
		for i := range pullResp.GraphArtifactIndex {
			if err := e.db.ImportGraphArtifactIndex(ctx, &pullResp.GraphArtifactIndex[i]); err == nil {
				totalImported++
			}
		}
		for i := range pullResp.GraphArtifactBodies {
			if err := e.db.ImportGraphArtifactBody(ctx, &pullResp.GraphArtifactBodies[i]); err == nil {
				totalImported++
			}
		}
		for i := range pullResp.GraphSlackGroups {
			if err := e.db.ImportGraphSlackGroup(ctx, &pullResp.GraphSlackGroups[i]); err == nil {
				totalImported++
			}
		}
		for i := range pullResp.GraphEntities {
			if err := e.db.ImportGraphEntity(ctx, &pullResp.GraphEntities[i]); err == nil {
				totalImported++
			}
		}
		for i := range pullResp.GraphJobs {
			if err := e.db.ImportGraphJob(ctx, &pullResp.GraphJobs[i]); err == nil {
				totalImported++
			}
		}
		for i := range pullResp.GraphUserAffinityConfig {
			if err := e.db.ImportGraphUserAffinityConfig(ctx, &pullResp.GraphUserAffinityConfig[i]); err == nil {
				totalImported++
			}
		}

		// Update cursors from response — original tables
		if pullResp.Cursors.Observations > 0 {
			e.setPullCursor(ctx, "observations", pullResp.Cursors.Observations)
		}
		if pullResp.Cursors.Summaries > 0 {
			e.setPullCursor(ctx, "summaries", pullResp.Cursors.Summaries)
		}
		if pullResp.Cursors.Prompts > 0 {
			e.setPullCursor(ctx, "prompts", pullResp.Cursors.Prompts)
		}
		if pullResp.Cursors.Sessions > 0 {
			e.setPullCursor(ctx, "sessions", pullResp.Cursors.Sessions)
		}

		// Update cursors — graph tables
		if pullResp.Cursors.GraphPeople > 0 {
			e.setPullCursor(ctx, "graph.people", pullResp.Cursors.GraphPeople)
		}
		if pullResp.Cursors.GraphNodes > 0 {
			e.setPullCursor(ctx, "graph.nodes", pullResp.Cursors.GraphNodes)
		}
		if pullResp.Cursors.GraphEdges > 0 {
			e.setPullCursor(ctx, "graph.edges", pullResp.Cursors.GraphEdges)
		}
		if pullResp.Cursors.GraphArtifactIndex > 0 {
			e.setPullCursor(ctx, "graph.artifact_index", pullResp.Cursors.GraphArtifactIndex)
		}
		if pullResp.Cursors.GraphArtifactBodies > 0 {
			e.setPullCursor(ctx, "graph.artifact_bodies", pullResp.Cursors.GraphArtifactBodies)
		}
		if pullResp.Cursors.GraphSlackGroups > 0 {
			e.setPullCursor(ctx, "graph.slack_groups", pullResp.Cursors.GraphSlackGroups)
		}
		if pullResp.Cursors.GraphEntities > 0 {
			e.setPullCursor(ctx, "graph.entities", pullResp.Cursors.GraphEntities)
		}
		if pullResp.Cursors.GraphJobs > 0 {
			e.setPullCursor(ctx, "graph.jobs", pullResp.Cursors.GraphJobs)
		}
		if pullResp.Cursors.GraphUserAffinityConfig > 0 {
			e.setPullCursor(ctx, "graph.user_affinity_config", pullResp.Cursors.GraphUserAffinityConfig)
		}
	}

	e.db.SetLastSyncTime(ctx, "last_pull")
	if totalImported > 0 {
		log.Info().Int("imported", totalImported).Msg("Sync pull complete")
	}
	return nil
}

func (e *Engine) getPullCursor(ctx context.Context, table string) int {
	var v string
	err := e.db.Pool.QueryRow(ctx, `SELECT value FROM settings WHERE key = $1`, "pull_cursor:"+table).Scan(&v)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(v)
	return n
}

func (e *Engine) setPullCursor(ctx context.Context, table string, id int) {
	e.db.Pool.Exec(ctx, `
		INSERT INTO settings (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value
	`, "pull_cursor:"+table, strconv.Itoa(id))
}

func (e *Engine) postJSON(ctx context.Context, url string, payload any) (*http.Response, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.config.APIKey)

	return e.client.Do(req)
}

// GetInfo returns current sync status.
func (e *Engine) GetInfo(ctx context.Context) (*SyncInfo, error) {
	stats, err := e.db.GetSyncStats(ctx)
	if err != nil {
		return nil, err
	}

	info := &SyncInfo{
		Mode:         "local",
		MachineID:    e.config.MachineID,
		SyncEnabled:  e.config.SyncEnabled,
		SyncInterval: e.config.SyncInterval,
		Stats:        stats,
	}

	if t, err := e.db.GetLastSyncTime(ctx, "last_push"); err == nil {
		info.LastPush = t
	}
	if t, err := e.db.GetLastSyncTime(ctx, "last_pull"); err == nil {
		info.LastPull = t
	}

	return info, nil
}

// --- helpers ---

// graphSyncIDs extracts sync_id strings from any graph slice using a selector func.
func graphSyncIDs[T any](items []T, sel func(T) string) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, sel(item))
	}
	return ids
}

func syncIDs(sessions []database.SdkSession) []string {
	ids := make([]string, 0, len(sessions))
	for _, s := range sessions {
		if s.SyncID != nil {
			ids = append(ids, *s.SyncID)
		}
	}
	return ids
}

func syncObsIDs(obs []database.SyncableObservation) []string {
	ids := make([]string, 0, len(obs))
	for _, o := range obs {
		if o.SyncID != nil {
			ids = append(ids, *o.SyncID)
		}
	}
	return ids
}

func syncSumIDs(sums []database.SyncableSummary) []string {
	ids := make([]string, 0, len(sums))
	for _, s := range sums {
		if s.SyncID != nil {
			ids = append(ids, *s.SyncID)
		}
	}
	return ids
}

func syncPromptIDs(prompts []database.SyncablePrompt) []string {
	ids := make([]string, 0, len(prompts))
	for _, p := range prompts {
		if p.SyncID != nil {
			ids = append(ids, *p.SyncID)
		}
	}
	return ids
}
