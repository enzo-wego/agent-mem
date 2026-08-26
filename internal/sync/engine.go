package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
	GraphUserAffinityConfig []database.SyncableGraphUserAffinityConfig `json:"graph_user_affinity_config,omitempty"`
}

// SyncPushResponse is the response from the cloud after a push.
type SyncPushResponse struct {
	Received int `json:"received"`
	Rejected int `json:"rejected"`
}

// PullCursors holds per-table cloud-side cursors for pull pagination.
type PullCursors struct {
	Observations int `json:"observations"`
	Summaries    int `json:"summaries"`
	Prompts      int `json:"prompts"`
	Sessions     int `json:"sessions"`

	// Graph table cursors. people/edges key on a monotonic BIGSERIAL id and
	// carry the last row's numeric key. The other six key on (timestamp, pk) and
	// carry that pair encoded as "<RFC3339Nano>|<pk>" (see EncodeCursor). That
	// includes user_affinity_config: its eeid is not monotonic with write time,
	// so it too rides a composite string cursor with the eeid rendered as text.
	GraphPeople             int    `json:"graph_people"`
	GraphNodes              string `json:"graph_nodes"`
	GraphEdges              int    `json:"graph_edges"`
	GraphArtifactIndex      string `json:"graph_artifact_index"`
	GraphArtifactBodies     string `json:"graph_artifact_bodies"`
	GraphSlackGroups        string `json:"graph_slack_groups"`
	GraphEntities           string `json:"graph_entities"`
	GraphUserAffinityConfig string `json:"graph_user_affinity_config"`
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

func (e *Engine) push(ctx context.Context) error {
	// Push is flat-memory only. The hub owns graph memory and this side is a read
	// replica of it (docs/ai/round-local-graph-replica.md, 2026-08-26): sending
	// graph rows back made the hub execute this machine's job queue.
	sessions, _ := e.db.GetUnsyncedSessions(ctx, batchSize)
	observations, _ := e.db.GetUnsyncedObservations(ctx, batchSize)
	summaries, _ := e.db.GetUnsyncedSummaries(ctx, batchSize)
	prompts, _ := e.db.GetUnsyncedPrompts(ctx, batchSize)

	total := len(sessions) + len(observations) + len(summaries) + len(prompts)

	// Always push (even empty) so cloud tracks client heartbeat
	payload := SyncPushPayload{
		MachineID:    e.config.MachineID,
		Sessions:     sessions,
		Observations: observations,
		Summaries:    summaries,
		Prompts:      prompts,
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

	e.db.SetLastSyncTime(ctx, "last_push")
	log.Info().Int("total", total).Int("received", pushResp.Received).Msg("Sync push complete")
	return nil
}

func (e *Engine) pull(ctx context.Context) error {
	totalImported := 0
	importFailed := 0

	// record tallies an import result. On failure we log and count but never stop:
	// the cursor must still advance past a failed row. Blocking on failure would
	// deadlock the pull loop — an edge whose node has not yet arrived fails its FK,
	// and if the cursor could not move past it the same batch would be re-requested
	// forever inside one cycle (see the `for { ... if batchTotal == 0 { break } }`
	// loop below). Log the failure and let a later walk pick the row up once its
	// parent row exists.
	record := func(err error, table, key string) {
		if err != nil {
			importFailed++
			log.Warn().Err(err).Str("table", table).Str("key", key).Msg("Sync import failed")
			return
		}
		totalImported++
	}

	for {
		// Graph cursors
		gPeopleCursor := e.getPullCursor(ctx, "graph.people")
		gNodesCursor := e.getPullCursorStr(ctx, "graph.nodes")
		gEdgesCursor := e.getPullCursor(ctx, "graph.edges")
		gArtIdxCursor := e.getPullCursorStr(ctx, "graph.artifact_index")
		gArtBodyCursor := e.getPullCursorStr(ctx, "graph.artifact_bodies")
		gSlackGrpCursor := e.getPullCursorStr(ctx, "graph.slack_groups")
		gEntitiesCursor := e.getPullCursorStr(ctx, "graph.entities")
		gAffinityCursor := e.getPullCursorStr(ctx, "graph.user_affinity_config")

		pullURL := fmt.Sprintf(
			"%s/api/sync/pull?machine_id=%s&limit=%d"+
				"&g_people_after=%d&g_nodes_after=%s&g_edges_after=%d"+
				"&g_artidx_after=%s&g_artbody_after=%s&g_slackgrp_after=%s"+
				"&g_entities_after=%s&g_affinity_after=%s",
			e.config.SyncURL, e.config.MachineID, batchSize,
			gPeopleCursor, url.QueryEscape(gNodesCursor), gEdgesCursor,
			url.QueryEscape(gArtIdxCursor), url.QueryEscape(gArtBodyCursor), url.QueryEscape(gSlackGrpCursor),
			url.QueryEscape(gEntitiesCursor), url.QueryEscape(gAffinityCursor),
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

		batchTotal := len(pullResp.GraphPeople) + len(pullResp.GraphNodes) + len(pullResp.GraphEdges) +
			len(pullResp.GraphArtifactIndex) + len(pullResp.GraphArtifactBodies) +
			len(pullResp.GraphSlackGroups) + len(pullResp.GraphEntities) +
			len(pullResp.GraphUserAffinityConfig)
		if batchTotal == 0 {
			break // fully caught up
		}

		// Import graph tables in FK order (people -> nodes -> edges, then the
		// rest). record() logs+counts failures and always advances.
		for i := range pullResp.GraphPeople {
			record(e.db.ImportGraphPerson(ctx, &pullResp.GraphPeople[i]), "graph.people", strconv.FormatInt(pullResp.GraphPeople[i].ID, 10))
		}
		for i := range pullResp.GraphNodes {
			record(e.db.ImportGraphNode(ctx, &pullResp.GraphNodes[i]), "graph.nodes", pullResp.GraphNodes[i].ID)
		}
		for i := range pullResp.GraphEdges {
			record(e.db.ImportGraphEdge(ctx, &pullResp.GraphEdges[i]), "graph.edges", strconv.FormatInt(pullResp.GraphEdges[i].ID, 10))
		}
		for i := range pullResp.GraphArtifactIndex {
			record(e.db.ImportGraphArtifactIndex(ctx, &pullResp.GraphArtifactIndex[i]), "graph.artifact_index", pullResp.GraphArtifactIndex[i].NodeID)
		}
		for i := range pullResp.GraphArtifactBodies {
			record(e.db.ImportGraphArtifactBody(ctx, &pullResp.GraphArtifactBodies[i]), "graph.artifact_bodies", pullResp.GraphArtifactBodies[i].NodeID)
		}
		for i := range pullResp.GraphSlackGroups {
			record(e.db.ImportGraphSlackGroup(ctx, &pullResp.GraphSlackGroups[i]), "graph.slack_groups", pullResp.GraphSlackGroups[i].ID)
		}
		for i := range pullResp.GraphEntities {
			record(e.db.ImportGraphEntity(ctx, &pullResp.GraphEntities[i]), "graph.entities", pullResp.GraphEntities[i].ID)
		}
		for i := range pullResp.GraphUserAffinityConfig {
			record(e.db.ImportGraphUserAffinityConfig(ctx, &pullResp.GraphUserAffinityConfig[i]), "graph.user_affinity_config", strconv.Itoa(pullResp.GraphUserAffinityConfig[i].EEID))
		}
		// Update cursors — graph tables
		if pullResp.Cursors.GraphPeople > 0 {
			e.setPullCursor(ctx, "graph.people", pullResp.Cursors.GraphPeople)
		}
		if pullResp.Cursors.GraphNodes != "" {
			e.setPullCursorStr(ctx, "graph.nodes", pullResp.Cursors.GraphNodes)
		}
		if pullResp.Cursors.GraphEdges > 0 {
			e.setPullCursor(ctx, "graph.edges", pullResp.Cursors.GraphEdges)
		}
		if pullResp.Cursors.GraphArtifactIndex != "" {
			e.setPullCursorStr(ctx, "graph.artifact_index", pullResp.Cursors.GraphArtifactIndex)
		}
		if pullResp.Cursors.GraphArtifactBodies != "" {
			e.setPullCursorStr(ctx, "graph.artifact_bodies", pullResp.Cursors.GraphArtifactBodies)
		}
		if pullResp.Cursors.GraphSlackGroups != "" {
			e.setPullCursorStr(ctx, "graph.slack_groups", pullResp.Cursors.GraphSlackGroups)
		}
		if pullResp.Cursors.GraphEntities != "" {
			e.setPullCursorStr(ctx, "graph.entities", pullResp.Cursors.GraphEntities)
		}
		if pullResp.Cursors.GraphUserAffinityConfig != "" {
			e.setPullCursorStr(ctx, "graph.user_affinity_config", pullResp.Cursors.GraphUserAffinityConfig)
		}
	}

	e.db.SetLastSyncTime(ctx, "last_pull")
	if totalImported > 0 || importFailed > 0 {
		log.Info().Int("imported", totalImported).Int("failed", importFailed).Msg("Sync pull complete")
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

// getPullCursorStr / setPullCursorStr are the TEXT-key variants used by the
// keyset cursors on TEXT-PK graph tables (nodes, artifact_index/bodies,
// slack_groups, entities). The settings.value column is already TEXT.
func (e *Engine) getPullCursorStr(ctx context.Context, table string) string {
	var v string
	err := e.db.Pool.QueryRow(ctx, `SELECT value FROM settings WHERE key = $1`, "pull_cursor:"+table).Scan(&v)
	if err != nil {
		return ""
	}
	return v
}

func (e *Engine) setPullCursorStr(ctx context.Context, table, key string) {
	e.db.Pool.Exec(ctx, `
		INSERT INTO settings (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value
	`, "pull_cursor:"+table, key)
}

// EncodeCursor packs a pull cursor's (timestamp, primary-key) pair into the
// single string carried in PullCursors, formatted "<RFC3339Nano>|<pk>". The six
// keyset-paginated graph tables order by (timestamp, pk), so the cursor carries
// both halves inside one string value — no PullCursors shape change.
func EncodeCursor(ts time.Time, pk string) string {
	return ts.UTC().Format(time.RFC3339Nano) + "|" + pk
}

// DecodeCursor unpacks a cursor produced by EncodeCursor. An empty string OR any
// string that does not parse as "<RFC3339Nano>|<pk>" means "from the beginning":
// it returns the zero time and an empty pk, and the query's (ts, pk) > ($2, $3)
// then matches every row.
//
// This fail-open is DELIBERATE — do NOT "fix" it to surface an error. The pull
// cursors parked before this change hold bare natural ids like "wego_order:WF-…"
// that cannot parse as a pair, so the first pull after deploy walks each table
// from the start and self-heals into one full walk.
//
// Re-delivering a row whose timestamp advanced past a cursor we already passed is
// expected and harmless: every Import* uses ON CONFLICT DO NOTHING, so a row that
// already exists locally is skipped, not overwritten. Do not turn that into
// DO UPDATE here — content overwrite is a separate decision (agent-mem-zqt).
func DecodeCursor(s string) (time.Time, string) {
	// RFC3339Nano never contains '|', so the first '|' separates the timestamp
	// from the pk; splitting there leaves any '|' inside a natural-key pk intact.
	sep := strings.IndexByte(s, '|')
	if sep < 0 {
		return time.Time{}, ""
	}
	ts, err := time.Parse(time.RFC3339Nano, s[:sep])
	if err != nil {
		return time.Time{}, ""
	}
	return ts, s[sep+1:]
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
