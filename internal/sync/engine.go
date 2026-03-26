package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
}

// SyncPushResponse is the response from the cloud after a push.
type SyncPushResponse struct {
	Received int `json:"received"`
	Rejected int `json:"rejected"`
}

// SyncPullResponse is the data received from cloud during pull.
type SyncPullResponse struct {
	Sessions     []database.SdkSession          `json:"sessions,omitempty"`
	Observations []database.SyncableObservation `json:"observations,omitempty"`
	Summaries    []database.SyncableSummary     `json:"summaries,omitempty"`
	Prompts      []database.SyncablePrompt      `json:"prompts,omitempty"`
}

// SyncInfo holds current sync status for the info endpoint.
type SyncInfo struct {
	Mode         string             `json:"mode"`
	MachineID    string             `json:"machine_id"`
	SyncEnabled  bool               `json:"sync_enabled"`
	SyncInterval string             `json:"sync_interval"`
	Stats        []database.SyncStats `json:"stats"`
	LastPush     *time.Time         `json:"last_push,omitempty"`
	LastPull     *time.Time         `json:"last_pull,omitempty"`
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
		}
	}
}

func (e *Engine) push(ctx context.Context) error {
	sessions, _ := e.db.GetUnsyncedSessions(ctx, batchSize)
	observations, _ := e.db.GetUnsyncedObservations(ctx, batchSize)
	summaries, _ := e.db.GetUnsyncedSummaries(ctx, batchSize)
	prompts, _ := e.db.GetUnsyncedPrompts(ctx, batchSize)

	total := len(sessions) + len(observations) + len(summaries) + len(prompts)
	if total == 0 {
		e.db.SetLastSyncTime(ctx, "last_push")
		return nil
	}

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

	// Mark synced
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
	url := fmt.Sprintf("%s/api/sync/pull?machine_id=%s&limit=%d", e.config.SyncURL, e.config.MachineID, batchSize)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create pull request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.config.APIKey)

	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("pull: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("pull returned %d: %s", resp.StatusCode, string(body))
	}

	var pullResp SyncPullResponse
	if err := json.NewDecoder(resp.Body).Decode(&pullResp); err != nil {
		return fmt.Errorf("decode pull response: %w", err)
	}

	imported := 0
	for i := range pullResp.Sessions {
		if err := e.db.ImportSession(ctx, &pullResp.Sessions[i]); err == nil {
			imported++
		}
	}
	for i := range pullResp.Observations {
		if err := e.db.ImportObservation(ctx, &pullResp.Observations[i]); err == nil {
			imported++
		}
	}
	for i := range pullResp.Summaries {
		if err := e.db.ImportSummary(ctx, &pullResp.Summaries[i]); err == nil {
			imported++
		}
	}
	for i := range pullResp.Prompts {
		if err := e.db.ImportPrompt(ctx, &pullResp.Prompts[i]); err == nil {
			imported++
		}
	}

	e.db.SetLastSyncTime(ctx, "last_pull")
	if imported > 0 {
		log.Info().Int("imported", imported).Msg("Sync pull complete")
	}
	return nil
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
