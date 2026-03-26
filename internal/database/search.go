package database

import (
	"context"
	"fmt"
	"time"

	"github.com/pgvector/pgvector-go"
)

// SearchResult represents a unified search result from any table.
type SearchResult struct {
	ID            int       `json:"id"`
	Type          string    `json:"type"`
	Title         string    `json:"title"`
	Subtitle      string    `json:"subtitle"`
	Narrative     string    `json:"narrative"`
	Project       string    `json:"project"`
	CreatedAt     time.Time `json:"created_at"`
	TextRank      float64   `json:"text_rank"`
	SemanticScore float64   `json:"semantic_score"`
	CombinedScore float64   `json:"combined_score"`
}

// HybridSearch performs combined FTS + semantic search across observations.
func (db *DB) HybridSearch(ctx context.Context, query string, queryEmbedding []float32, project string, limit int) ([]SearchResult, error) {
	vec := pgvector.NewVector(queryEmbedding)

	rows, err := db.Pool.Query(ctx, `
		WITH fts AS (
			SELECT id, ts_rank(search_vector, websearch_to_tsquery('english', $1)) AS text_rank
			FROM observations
			WHERE project = $2 AND search_vector @@ websearch_to_tsquery('english', $1)
		),
		semantic AS (
			SELECT id, 1 - (embedding <=> $3::vector) AS semantic_score
			FROM observations
			WHERE project = $2 AND embedding IS NOT NULL
			ORDER BY embedding <=> $3::vector
			LIMIT $4 * 3
		)
		SELECT o.id, o.type, COALESCE(o.title, ''), COALESCE(o.subtitle, ''),
		       COALESCE(o.narrative, ''), o.project, o.created_at,
		       COALESCE(f.text_rank, 0) AS text_rank,
		       COALESCE(s.semantic_score, 0) AS semantic_score,
		       0.4 * COALESCE(f.text_rank, 0) + 0.6 * COALESCE(s.semantic_score, 0) AS combined_score
		FROM observations o
		LEFT JOIN fts f ON o.id = f.id
		LEFT JOIN semantic s ON o.id = s.id
		WHERE f.id IS NOT NULL OR s.id IS NOT NULL
		ORDER BY combined_score DESC
		LIMIT $4
	`, query, project, &vec, limit)
	if err != nil {
		return nil, fmt.Errorf("hybrid search: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(
			&r.ID, &r.Type, &r.Title, &r.Subtitle, &r.Narrative,
			&r.Project, &r.CreatedAt,
			&r.TextRank, &r.SemanticScore, &r.CombinedScore,
		); err != nil {
			return nil, fmt.Errorf("scan search result: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// HybridSearchSummaries performs combined FTS + semantic search across session summaries.
func (db *DB) HybridSearchSummaries(ctx context.Context, query string, queryEmbedding []float32, project string, limit int) ([]SearchResult, error) {
	vec := pgvector.NewVector(queryEmbedding)

	rows, err := db.Pool.Query(ctx, `
		WITH fts AS (
			SELECT id, ts_rank(search_vector, websearch_to_tsquery('english', $1)) AS text_rank
			FROM session_summaries
			WHERE project = $2 AND search_vector @@ websearch_to_tsquery('english', $1)
		),
		semantic AS (
			SELECT id, 1 - (embedding <=> $3::vector) AS semantic_score
			FROM session_summaries
			WHERE project = $2 AND embedding IS NOT NULL
			ORDER BY embedding <=> $3::vector
			LIMIT $4 * 3
		)
		SELECT ss.id, 'summary' AS type, COALESCE(ss.request, ''), '',
		       COALESCE(ss.completed, ''), ss.project, ss.created_at,
		       COALESCE(f.text_rank, 0) AS text_rank,
		       COALESCE(s.semantic_score, 0) AS semantic_score,
		       0.4 * COALESCE(f.text_rank, 0) + 0.6 * COALESCE(s.semantic_score, 0) AS combined_score
		FROM session_summaries ss
		LEFT JOIN fts f ON ss.id = f.id
		LEFT JOIN semantic s ON ss.id = s.id
		WHERE f.id IS NOT NULL OR s.id IS NOT NULL
		ORDER BY combined_score DESC
		LIMIT $4
	`, query, project, &vec, limit)
	if err != nil {
		return nil, fmt.Errorf("hybrid search summaries: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(
			&r.ID, &r.Type, &r.Title, &r.Subtitle, &r.Narrative,
			&r.Project, &r.CreatedAt,
			&r.TextRank, &r.SemanticScore, &r.CombinedScore,
		); err != nil {
			return nil, fmt.Errorf("scan summary result: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// SearchByFile returns observations that reference a specific file path.
func (db *DB) SearchByFile(ctx context.Context, filePath, project string, limit int) ([]SearchResult, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, type, COALESCE(title, ''), COALESCE(subtitle, ''),
		       COALESCE(narrative, ''), project, created_at,
		       0::float8 AS text_rank, 0::float8 AS semantic_score, 0::float8 AS combined_score
		FROM observations
		WHERE project = $1
		  AND (files_read @> $2::jsonb OR files_modified @> $2::jsonb)
		ORDER BY created_at_epoch DESC
		LIMIT $3
	`, project, fmt.Sprintf(`[%q]`, filePath), limit)
	if err != nil {
		return nil, fmt.Errorf("search by file: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(
			&r.ID, &r.Type, &r.Title, &r.Subtitle, &r.Narrative,
			&r.Project, &r.CreatedAt,
			&r.TextRank, &r.SemanticScore, &r.CombinedScore,
		); err != nil {
			return nil, fmt.Errorf("scan file result: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// SearchTimeline returns observations in a date range.
func (db *DB) SearchTimeline(ctx context.Context, project string, fromEpoch, toEpoch int64, limit int) ([]SearchResult, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, type, COALESCE(title, ''), COALESCE(subtitle, ''),
		       COALESCE(narrative, ''), project, created_at,
		       0::float8, 0::float8, 0::float8
		FROM observations
		WHERE project = $1 AND created_at_epoch >= $2 AND created_at_epoch <= $3
		ORDER BY created_at_epoch DESC
		LIMIT $4
	`, project, fromEpoch, toEpoch, limit)
	if err != nil {
		return nil, fmt.Errorf("search timeline: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(
			&r.ID, &r.Type, &r.Title, &r.Subtitle, &r.Narrative,
			&r.Project, &r.CreatedAt,
			&r.TextRank, &r.SemanticScore, &r.CombinedScore,
		); err != nil {
			return nil, fmt.Errorf("scan timeline result: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// GetObservationByID returns a single observation with all fields.
func (db *DB) GetObservationByID(ctx context.Context, id int) (*Observation, error) {
	var o Observation
	err := db.Pool.QueryRow(ctx, `
		SELECT id, memory_session_id, project, type, title, subtitle, narrative, text,
		       facts, concepts, files_read, files_modified, discovery_tokens,
		       created_at, created_at_epoch, embedding,
		       sync_id, sync_version, sync_source
		FROM observations WHERE id = $1
	`, id).Scan(
		&o.ID, &o.MemorySessionID, &o.Project, &o.Type, &o.Title, &o.Subtitle,
		&o.Narrative, &o.Text, &o.Facts, &o.Concepts, &o.FilesRead, &o.FilesModified,
		&o.DiscoveryTokens, &o.CreatedAt, &o.CreatedAtEpoch, &o.Embedding,
		&o.SyncID, &o.SyncVersion, &o.SyncSource,
	)
	if err != nil {
		return nil, fmt.Errorf("get observation: %w", err)
	}
	return &o, nil
}

// ListSummaries returns session summaries for a project, most recent first.
func (db *DB) ListSummaries(ctx context.Context, project string, limit int) ([]SessionSummary, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, memory_session_id, project,
		       COALESCE(request, ''), COALESCE(investigated, ''), COALESCE(learned, ''),
		       COALESCE(completed, ''), COALESCE(next_steps, ''), COALESCE(notes, ''),
		       COALESCE(files_read, '[]'::jsonb), COALESCE(files_edited, '[]'::jsonb),
		       discovery_tokens, created_at, created_at_epoch,
		       embedding, sync_id, sync_version, sync_source
		FROM session_summaries
		WHERE project = $1
		ORDER BY created_at_epoch DESC
		LIMIT $2
	`, project, limit)
	if err != nil {
		return nil, fmt.Errorf("list summaries: %w", err)
	}
	defer rows.Close()

	var summaries []SessionSummary
	for rows.Next() {
		var s SessionSummary
		if err := rows.Scan(
			&s.ID, &s.MemorySessionID, &s.Project,
			&s.Request, &s.Investigated, &s.Learned,
			&s.Completed, &s.NextSteps, &s.Notes,
			&s.FilesRead, &s.FilesEdited,
			&s.DiscoveryTokens, &s.CreatedAt, &s.CreatedAtEpoch,
			&s.Embedding, &s.SyncID, &s.SyncVersion, &s.SyncSource,
		); err != nil {
			return nil, fmt.Errorf("scan summary: %w", err)
		}
		summaries = append(summaries, s)
	}
	return summaries, rows.Err()
}

// ListPrompts returns user prompts for a project, most recent first.
func (db *DB) ListPrompts(ctx context.Context, project string, limit int) ([]UserPrompt, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, content_session_id, project, prompt, prompt_number,
		       created_at, created_at_epoch,
		       embedding, sync_id, sync_version, sync_source
		FROM user_prompts
		WHERE project = $1
		ORDER BY created_at_epoch DESC
		LIMIT $2
	`, project, limit)
	if err != nil {
		return nil, fmt.Errorf("list prompts: %w", err)
	}
	defer rows.Close()

	var prompts []UserPrompt
	for rows.Next() {
		var p UserPrompt
		if err := rows.Scan(
			&p.ID, &p.ContentSessionID, &p.Project, &p.Prompt, &p.PromptNumber,
			&p.CreatedAt, &p.CreatedAtEpoch,
			&p.Embedding, &p.SyncID, &p.SyncVersion, &p.SyncSource,
		); err != nil {
			return nil, fmt.Errorf("scan prompt: %w", err)
		}
		prompts = append(prompts, p)
	}
	return prompts, rows.Err()
}

// ListObservations returns observations filtered by project and optional type.
func (db *DB) ListObservations(ctx context.Context, project, obsType string, limit int) ([]SearchResult, error) {
	var queryStr string
	var args []any

	if obsType != "" {
		queryStr = `
			SELECT id, type, COALESCE(title, ''), COALESCE(subtitle, ''),
			       COALESCE(narrative, ''), project, created_at,
			       0::float8, 0::float8, 0::float8
			FROM observations
			WHERE project = $1 AND type = $2
			ORDER BY created_at_epoch DESC LIMIT $3`
		args = []any{project, obsType, limit}
	} else {
		queryStr = `
			SELECT id, type, COALESCE(title, ''), COALESCE(subtitle, ''),
			       COALESCE(narrative, ''), project, created_at,
			       0::float8, 0::float8, 0::float8
			FROM observations
			WHERE project = $1
			ORDER BY created_at_epoch DESC LIMIT $2`
		args = []any{project, limit}
	}

	rows, err := db.Pool.Query(ctx, queryStr, args...)
	if err != nil {
		return nil, fmt.Errorf("list observations: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(
			&r.ID, &r.Type, &r.Title, &r.Subtitle, &r.Narrative,
			&r.Project, &r.CreatedAt,
			&r.TextRank, &r.SemanticScore, &r.CombinedScore,
		); err != nil {
			return nil, fmt.Errorf("scan observation: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}
