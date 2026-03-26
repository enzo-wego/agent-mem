package database

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ObservationRow is a lightweight observation for context injection.
type ObservationRow struct {
	ID             int
	Type           string
	Title          string
	Subtitle       string
	Narrative      string
	Facts          []string
	FilesRead      []string
	FilesModified  []string
	PromptNumber   int
	CreatedAt      time.Time
	CreatedAtEpoch int64
}

// SummaryRow is a lightweight summary for context injection.
type SummaryRow struct {
	ID              int
	MemorySessionID string
	Request         string
	Investigated    string
	Learned         string
	Completed       string
	NextSteps       string
	CreatedAt       time.Time
	CreatedAtEpoch  int64
}

// GetRecentObservations returns the most recent observations for a project.
func (db *DB) GetRecentObservations(ctx context.Context, project string, limit int) ([]ObservationRow, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, type, COALESCE(title, ''), COALESCE(subtitle, ''), COALESCE(narrative, ''),
		       facts, files_read, files_modified,
		       COALESCE((SELECT prompt_number FROM user_prompts WHERE content_session_id = (
		           SELECT content_session_id FROM sdk_sessions WHERE memory_session_id = o.memory_session_id LIMIT 1
		       ) ORDER BY prompt_number DESC LIMIT 1), 0),
		       created_at, created_at_epoch
		FROM observations o
		WHERE project = $1
		ORDER BY created_at_epoch DESC
		LIMIT $2
	`, project, limit)
	if err != nil {
		return nil, fmt.Errorf("get recent observations: %w", err)
	}
	defer rows.Close()

	return scanObservationRows(rows)
}

// GetRecentSummaries returns the most recent session summaries for a project.
func (db *DB) GetRecentSummaries(ctx context.Context, project string, limit int) ([]SummaryRow, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT id, memory_session_id,
		       COALESCE(request, ''), COALESCE(investigated, ''),
		       COALESCE(learned, ''), COALESCE(completed, ''), COALESCE(next_steps, ''),
		       created_at, created_at_epoch
		FROM session_summaries
		WHERE project = $1
		ORDER BY created_at_epoch DESC
		LIMIT $2
	`, project, limit)
	if err != nil {
		return nil, fmt.Errorf("get recent summaries: %w", err)
	}
	defer rows.Close()

	var summaries []SummaryRow
	for rows.Next() {
		var s SummaryRow
		if err := rows.Scan(
			&s.ID, &s.MemorySessionID,
			&s.Request, &s.Investigated, &s.Learned, &s.Completed, &s.NextSteps,
			&s.CreatedAt, &s.CreatedAtEpoch,
		); err != nil {
			return nil, fmt.Errorf("scan summary: %w", err)
		}
		summaries = append(summaries, s)
	}
	return summaries, rows.Err()
}

// ListProjects returns all distinct projects with their observation counts, ordered by count desc.
func (db *DB) ListProjects(ctx context.Context) ([]ProjectInfo, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT project, COUNT(*) as obs_count
		FROM observations
		WHERE project != ''
		GROUP BY project
		ORDER BY obs_count DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	var projects []ProjectInfo
	for rows.Next() {
		var p ProjectInfo
		if err := rows.Scan(&p.Name, &p.ObservationCount); err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

// ProjectInfo holds project name and observation count.
type ProjectInfo struct {
	Name             string `json:"name"`
	ObservationCount int    `json:"observation_count"`
}

// Stats holds aggregate counts for the header.
type Stats struct {
	Observations int `json:"observations"`
	Summaries    int `json:"summaries"`
	Prompts      int `json:"prompts"`
}

// GetStats returns aggregate counts, optionally filtered by project.
func (db *DB) GetStats(ctx context.Context, project string) (*Stats, error) {
	var s Stats
	if project != "" {
		db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM observations WHERE project = $1`, project).Scan(&s.Observations)
		db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM session_summaries WHERE project = $1`, project).Scan(&s.Summaries)
		db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_prompts WHERE project = $1`, project).Scan(&s.Prompts)
	} else {
		db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM observations`).Scan(&s.Observations)
		db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM session_summaries`).Scan(&s.Summaries)
		db.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM user_prompts`).Scan(&s.Prompts)
	}
	return &s, nil
}

func scanObservationRows(rows pgx.Rows) ([]ObservationRow, error) {
	var observations []ObservationRow
	for rows.Next() {
		var o ObservationRow
		var factsJSON, filesReadJSON, filesModifiedJSON []byte
		if err := rows.Scan(
			&o.ID, &o.Type, &o.Title, &o.Subtitle, &o.Narrative,
			&factsJSON, &filesReadJSON, &filesModifiedJSON,
			&o.PromptNumber, &o.CreatedAt, &o.CreatedAtEpoch,
		); err != nil {
			return nil, fmt.Errorf("scan observation: %w", err)
		}
		_ = json.Unmarshal(factsJSON, &o.Facts)
		_ = json.Unmarshal(filesReadJSON, &o.FilesRead)
		_ = json.Unmarshal(filesModifiedJSON, &o.FilesModified)
		observations = append(observations, o)
	}
	return observations, rows.Err()
}

