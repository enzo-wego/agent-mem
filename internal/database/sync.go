package database

import (
	"context"
	"fmt"
	"time"

	"github.com/pgvector/pgvector-go"
)

// SyncableObservation is an observation with embedding data for sync transport.
type SyncableObservation struct {
	Observation
	EmbeddingData []float32 `json:"embedding_data,omitempty"`
}

// SyncableSummary is a summary with embedding data for sync transport.
type SyncableSummary struct {
	SessionSummary
	EmbeddingData []float32 `json:"embedding_data,omitempty"`
}

// SyncablePrompt is a prompt with embedding data for sync transport.
type SyncablePrompt struct {
	UserPrompt
	EmbeddingData []float32 `json:"embedding_data,omitempty"`
}

// GetUnsyncedSessions returns sessions that haven't been synced yet.
func (db *DB) GetUnsyncedSessions(ctx context.Context, limit int) ([]SdkSession, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, content_session_id, memory_session_id, project, user_prompt,
		       started_at, started_at_epoch, completed_at, completed_at_epoch, status,
		       sync_id, sync_version, sync_source
		FROM sdk_sessions
		WHERE sync_id IS NOT NULL AND sync_version = 0
		ORDER BY id ASC LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("get unsynced sessions: %w", err)
	}
	defer rows.Close()

	var sessions []SdkSession
	for rows.Next() {
		var s SdkSession
		if err := rows.Scan(
			&s.ID, &s.ContentSessionID, &s.MemorySessionID, &s.Project, &s.UserPrompt,
			&s.StartedAt, &s.StartedAtEpoch, &s.CompletedAt, &s.CompletedAtEpoch, &s.Status,
			&s.SyncID, &s.SyncVersion, &s.SyncSource,
		); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

// GetUnsyncedObservations returns observations that haven't been synced yet.
func (db *DB) GetUnsyncedObservations(ctx context.Context, limit int) ([]SyncableObservation, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, memory_session_id, project, type, title, subtitle, narrative, text,
		       facts, concepts, files_read, files_modified, discovery_tokens,
		       created_at, created_at_epoch, embedding,
		       sync_id, sync_version, sync_source
		FROM observations
		WHERE sync_id IS NOT NULL AND sync_version = 0
		ORDER BY id ASC LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("get unsynced observations: %w", err)
	}
	defer rows.Close()

	var observations []SyncableObservation
	for rows.Next() {
		var o SyncableObservation
		if err := rows.Scan(
			&o.ID, &o.MemorySessionID, &o.Project, &o.Type, &o.Title, &o.Subtitle,
			&o.Narrative, &o.Text, &o.Facts, &o.Concepts, &o.FilesRead, &o.FilesModified,
			&o.DiscoveryTokens, &o.CreatedAt, &o.CreatedAtEpoch, &o.Embedding,
			&o.SyncID, &o.SyncVersion, &o.SyncSource,
		); err != nil {
			return nil, fmt.Errorf("scan observation: %w", err)
		}
		if o.Embedding != nil {
			o.EmbeddingData = o.Embedding.Slice()
		}
		observations = append(observations, o)
	}
	return observations, rows.Err()
}

// GetUnsyncedSummaries returns summaries that haven't been synced yet.
func (db *DB) GetUnsyncedSummaries(ctx context.Context, limit int) ([]SyncableSummary, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, memory_session_id, project, request, investigated, learned, completed, next_steps,
		       notes, files_read, files_edited, discovery_tokens,
		       created_at, created_at_epoch, embedding,
		       sync_id, sync_version, sync_source
		FROM session_summaries
		WHERE sync_id IS NOT NULL AND sync_version = 0
		ORDER BY id ASC LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("get unsynced summaries: %w", err)
	}
	defer rows.Close()

	var summaries []SyncableSummary
	for rows.Next() {
		var s SyncableSummary
		if err := rows.Scan(
			&s.ID, &s.MemorySessionID, &s.Project, &s.Request, &s.Investigated,
			&s.Learned, &s.Completed, &s.NextSteps, &s.Notes, &s.FilesRead, &s.FilesEdited,
			&s.DiscoveryTokens, &s.CreatedAt, &s.CreatedAtEpoch, &s.Embedding,
			&s.SyncID, &s.SyncVersion, &s.SyncSource,
		); err != nil {
			return nil, fmt.Errorf("scan summary: %w", err)
		}
		if s.Embedding != nil {
			s.EmbeddingData = s.Embedding.Slice()
		}
		summaries = append(summaries, s)
	}
	return summaries, rows.Err()
}

// GetUnsyncedPrompts returns prompts that haven't been synced yet.
func (db *DB) GetUnsyncedPrompts(ctx context.Context, limit int) ([]SyncablePrompt, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, content_session_id, project, prompt, prompt_number,
		       created_at, created_at_epoch, embedding,
		       sync_id, sync_version, sync_source
		FROM user_prompts
		WHERE sync_id IS NOT NULL AND sync_version = 0
		ORDER BY id ASC LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("get unsynced prompts: %w", err)
	}
	defer rows.Close()

	var prompts []SyncablePrompt
	for rows.Next() {
		var p SyncablePrompt
		if err := rows.Scan(
			&p.ID, &p.ContentSessionID, &p.Project, &p.Prompt, &p.PromptNumber,
			&p.CreatedAt, &p.CreatedAtEpoch, &p.Embedding,
			&p.SyncID, &p.SyncVersion, &p.SyncSource,
		); err != nil {
			return nil, fmt.Errorf("scan prompt: %w", err)
		}
		if p.Embedding != nil {
			p.EmbeddingData = p.Embedding.Slice()
		}
		prompts = append(prompts, p)
	}
	return prompts, rows.Err()
}

// MarkSynced updates the sync_version for rows that were successfully pushed.
func (db *DB) MarkSynced(ctx context.Context, table string, syncIDs []string, version int) error {
	if len(syncIDs) == 0 {
		return nil
	}
	_, err := db.Pool.Exec(ctx, fmt.Sprintf(`
		UPDATE %s SET sync_version = $1 WHERE sync_id = ANY($2)
	`, table), version, syncIDs)
	if err != nil {
		return fmt.Errorf("mark synced %s: %w", table, err)
	}
	return nil
}

// ImportObservation imports an observation from sync (ON CONFLICT DO NOTHING).
func (db *DB) ImportObservation(ctx context.Context, o *SyncableObservation) error {
	var embeddingVec *pgvector.Vector
	if len(o.EmbeddingData) > 0 {
		v := pgvector.NewVector(o.EmbeddingData)
		embeddingVec = &v
	}

	_, err := db.Pool.Exec(ctx, `
		INSERT INTO observations (
			memory_session_id, project, type, title, subtitle, narrative, text,
			facts, concepts, files_read, files_modified, discovery_tokens,
			created_at, created_at_epoch, embedding,
			sync_id, sync_version, sync_source
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
		ON CONFLICT (sync_id) DO NOTHING
	`,
		o.MemorySessionID, o.Project, o.Type, o.Title, o.Subtitle, o.Narrative, o.Text,
		o.Facts, o.Concepts, o.FilesRead, o.FilesModified, o.DiscoveryTokens,
		o.CreatedAt, o.CreatedAtEpoch, embeddingVec,
		o.SyncID, o.SyncVersion, o.SyncSource,
	)
	return err
}

// ImportSession imports a session from sync.
func (db *DB) ImportSession(ctx context.Context, s *SdkSession) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO sdk_sessions (
			content_session_id, memory_session_id, project, user_prompt,
			started_at, started_at_epoch, completed_at, completed_at_epoch, status,
			sync_id, sync_version, sync_source
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (sync_id) DO NOTHING
	`,
		s.ContentSessionID, s.MemorySessionID, s.Project, s.UserPrompt,
		s.StartedAt, s.StartedAtEpoch, s.CompletedAt, s.CompletedAtEpoch, s.Status,
		s.SyncID, s.SyncVersion, s.SyncSource,
	)
	return err
}

// ImportSummary imports a session summary from sync.
func (db *DB) ImportSummary(ctx context.Context, s *SyncableSummary) error {
	var embeddingVec *pgvector.Vector
	if len(s.EmbeddingData) > 0 {
		v := pgvector.NewVector(s.EmbeddingData)
		embeddingVec = &v
	}

	_, err := db.Pool.Exec(ctx, `
		INSERT INTO session_summaries (
			memory_session_id, project, request, investigated, learned, completed, next_steps,
			notes, files_read, files_edited, discovery_tokens,
			created_at, created_at_epoch, embedding,
			sync_id, sync_version, sync_source
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
		ON CONFLICT (sync_id) DO NOTHING
	`,
		s.MemorySessionID, s.Project, s.Request, s.Investigated, s.Learned, s.Completed, s.NextSteps,
		s.Notes, s.FilesRead, s.FilesEdited, s.DiscoveryTokens,
		s.CreatedAt, s.CreatedAtEpoch, embeddingVec,
		s.SyncID, s.SyncVersion, s.SyncSource,
	)
	return err
}

// ImportPrompt imports a user prompt from sync.
func (db *DB) ImportPrompt(ctx context.Context, p *SyncablePrompt) error {
	var embeddingVec *pgvector.Vector
	if len(p.EmbeddingData) > 0 {
		v := pgvector.NewVector(p.EmbeddingData)
		embeddingVec = &v
	}

	_, err := db.Pool.Exec(ctx, `
		INSERT INTO user_prompts (
			content_session_id, project, prompt, prompt_number,
			created_at, created_at_epoch, embedding,
			sync_id, sync_version, sync_source
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (sync_id) DO NOTHING
	`,
		p.ContentSessionID, p.Project, p.Prompt, p.PromptNumber,
		p.CreatedAt, p.CreatedAtEpoch, embeddingVec,
		p.SyncID, p.SyncVersion, p.SyncSource,
	)
	return err
}

// GetObservationsForPull returns observations not originating from the requesting machine,
// paginated by cloud-side ID for cursor-based sync.
func (db *DB) GetObservationsForPull(ctx context.Context, excludeSource string, afterID, limit int) ([]SyncableObservation, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, memory_session_id, project, type, title, subtitle, narrative, text,
		       facts, concepts, files_read, files_modified, discovery_tokens,
		       created_at, created_at_epoch, embedding,
		       sync_id, sync_version, sync_source
		FROM observations
		WHERE sync_source IS DISTINCT FROM $1 AND id > $2
		ORDER BY id ASC LIMIT $3
	`, excludeSource, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("get observations for pull: %w", err)
	}
	defer rows.Close()

	var observations []SyncableObservation
	for rows.Next() {
		var o SyncableObservation
		if err := rows.Scan(
			&o.ID, &o.MemorySessionID, &o.Project, &o.Type, &o.Title, &o.Subtitle,
			&o.Narrative, &o.Text, &o.Facts, &o.Concepts, &o.FilesRead, &o.FilesModified,
			&o.DiscoveryTokens, &o.CreatedAt, &o.CreatedAtEpoch, &o.Embedding,
			&o.SyncID, &o.SyncVersion, &o.SyncSource,
		); err != nil {
			return nil, fmt.Errorf("scan observation for pull: %w", err)
		}
		if o.Embedding != nil {
			o.EmbeddingData = o.Embedding.Slice()
		}
		observations = append(observations, o)
	}
	return observations, rows.Err()
}

// GetSummariesForPull returns summaries not originating from the requesting machine.
func (db *DB) GetSummariesForPull(ctx context.Context, excludeSource string, afterID, limit int) ([]SyncableSummary, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, memory_session_id, project, request, investigated, learned, completed, next_steps,
		       notes, files_read, files_edited, discovery_tokens,
		       created_at, created_at_epoch, embedding,
		       sync_id, sync_version, sync_source
		FROM session_summaries
		WHERE sync_source IS DISTINCT FROM $1 AND id > $2
		ORDER BY id ASC LIMIT $3
	`, excludeSource, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("get summaries for pull: %w", err)
	}
	defer rows.Close()

	var summaries []SyncableSummary
	for rows.Next() {
		var s SyncableSummary
		if err := rows.Scan(
			&s.ID, &s.MemorySessionID, &s.Project, &s.Request, &s.Investigated,
			&s.Learned, &s.Completed, &s.NextSteps, &s.Notes, &s.FilesRead, &s.FilesEdited,
			&s.DiscoveryTokens, &s.CreatedAt, &s.CreatedAtEpoch, &s.Embedding,
			&s.SyncID, &s.SyncVersion, &s.SyncSource,
		); err != nil {
			return nil, fmt.Errorf("scan summary for pull: %w", err)
		}
		if s.Embedding != nil {
			s.EmbeddingData = s.Embedding.Slice()
		}
		summaries = append(summaries, s)
	}
	return summaries, rows.Err()
}

// GetPromptsForPull returns prompts not originating from the requesting machine.
func (db *DB) GetPromptsForPull(ctx context.Context, excludeSource string, afterID, limit int) ([]SyncablePrompt, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, content_session_id, project, prompt, prompt_number,
		       created_at, created_at_epoch, embedding,
		       sync_id, sync_version, sync_source
		FROM user_prompts
		WHERE sync_source IS DISTINCT FROM $1 AND id > $2
		ORDER BY id ASC LIMIT $3
	`, excludeSource, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("get prompts for pull: %w", err)
	}
	defer rows.Close()

	var prompts []SyncablePrompt
	for rows.Next() {
		var p SyncablePrompt
		if err := rows.Scan(
			&p.ID, &p.ContentSessionID, &p.Project, &p.Prompt, &p.PromptNumber,
			&p.CreatedAt, &p.CreatedAtEpoch, &p.Embedding,
			&p.SyncID, &p.SyncVersion, &p.SyncSource,
		); err != nil {
			return nil, fmt.Errorf("scan prompt for pull: %w", err)
		}
		if p.Embedding != nil {
			p.EmbeddingData = p.Embedding.Slice()
		}
		prompts = append(prompts, p)
	}
	return prompts, rows.Err()
}

// GetSessionsForPull returns sessions not originating from the requesting machine.
func (db *DB) GetSessionsForPull(ctx context.Context, excludeSource string, afterID, limit int) ([]SdkSession, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, content_session_id, memory_session_id, project, user_prompt,
		       started_at, started_at_epoch, completed_at, completed_at_epoch, status,
		       sync_id, sync_version, sync_source
		FROM sdk_sessions
		WHERE sync_source IS DISTINCT FROM $1 AND id > $2
		ORDER BY id ASC LIMIT $3
	`, excludeSource, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("get sessions for pull: %w", err)
	}
	defer rows.Close()

	var sessions []SdkSession
	for rows.Next() {
		var s SdkSession
		if err := rows.Scan(
			&s.ID, &s.ContentSessionID, &s.MemorySessionID, &s.Project, &s.UserPrompt,
			&s.StartedAt, &s.StartedAtEpoch, &s.CompletedAt, &s.CompletedAtEpoch, &s.Status,
			&s.SyncID, &s.SyncVersion, &s.SyncSource,
		); err != nil {
			return nil, fmt.Errorf("scan session for pull: %w", err)
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

// SyncStats holds sync status counts.
type SyncStats struct {
	Table    string `json:"table"`
	Total    int    `json:"total"`
	Unsynced int    `json:"unsynced"`
}

// GetSyncStats returns sync statistics for all tables.
func (db *DB) GetSyncStats(ctx context.Context) ([]SyncStats, error) {
	tables := []string{"observations", "session_summaries", "user_prompts", "sdk_sessions"}
	var stats []SyncStats
	for _, t := range tables {
		var total, unsynced int
		db.Pool.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s`, t)).Scan(&total)
		db.Pool.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE sync_id IS NOT NULL AND sync_version = 0`, t)).Scan(&unsynced)
		stats = append(stats, SyncStats{Table: t, Total: total, Unsynced: unsynced})
	}
	return stats, nil
}

// --- Sync timestamp tracking ---

// GetLastSyncTime returns the last push or pull timestamp.
func (db *DB) GetLastSyncTime(ctx context.Context, key string) (*time.Time, error) {
	var v string
	err := db.Pool.QueryRow(ctx, `SELECT value FROM settings WHERE key = $1`, key).Scan(&v)
	if err != nil {
		return nil, err
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// SetLastSyncTime stores the current time as the last push or pull timestamp.
func (db *DB) SetLastSyncTime(ctx context.Context, key string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO settings (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value
	`, key, now)
	return err
}

// ClientSyncTime holds per-client sync timestamps.
type ClientSyncTime struct {
	MachineID string
	LastPush  *time.Time
	LastPull  *time.Time
}

// GetClientSyncTimes returns per-client push/pull timestamps from settings.
func (db *DB) GetClientSyncTimes(ctx context.Context) ([]ClientSyncTime, error) {
	rows, err := db.Pool.Query(ctx, `SELECT key, value FROM settings WHERE key LIKE 'client_push:%' OR key LIKE 'client_pull:%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	clients := make(map[string]*ClientSyncTime)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			continue
		}

		var prefix, machineID string
		if len(k) > 12 && k[:12] == "client_push:" {
			prefix = "push"
			machineID = k[12:]
		} else if len(k) > 12 && k[:12] == "client_pull:" {
			prefix = "pull"
			machineID = k[12:]
		} else {
			continue
		}

		c, ok := clients[machineID]
		if !ok {
			c = &ClientSyncTime{MachineID: machineID}
			clients[machineID] = c
		}
		if prefix == "push" {
			c.LastPush = &t
		} else {
			c.LastPull = &t
		}
	}

	result := make([]ClientSyncTime, 0, len(clients))
	for _, c := range clients {
		result = append(result, *c)
	}
	return result, nil
}
