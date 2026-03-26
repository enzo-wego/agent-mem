package database

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
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

// GetRowsForPull returns rows not from the requesting machine, ordered by ID.
func (db *DB) GetRowsForPull(ctx context.Context, table, machineID string, lastID, limit int) (pgx.Rows, error) {
	return db.Pool.Query(ctx, fmt.Sprintf(`
		SELECT * FROM %s
		WHERE sync_source != $1 AND id > $2
		ORDER BY id ASC LIMIT $3
	`, table), machineID, lastID, limit)
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

// GetLastSyncTime returns the last push or pull timestamp from a metadata table.
func (db *DB) GetLastSyncTime(ctx context.Context, key string) (*time.Time, error) {
	// Use schema_versions table for metadata storage (lightweight)
	var t time.Time
	err := db.Pool.QueryRow(ctx, `
		SELECT applied_at FROM schema_versions WHERE description = $1
	`, key).Scan(&t)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// SetLastSyncTime stores the last push or pull timestamp.
func (db *DB) SetLastSyncTime(ctx context.Context, key string) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO schema_versions (version, description, applied_at)
		VALUES ((SELECT COALESCE(MAX(version), 0) + 1 FROM schema_versions), $1, now())
		ON CONFLICT (version) DO UPDATE SET applied_at = now()
	`, key)
	return err
}
