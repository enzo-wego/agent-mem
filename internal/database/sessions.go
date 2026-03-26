package database

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"
)

func generateID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// UpsertSession creates or finds a session by content_session_id.
// Returns the session and whether it was newly created.
func (db *DB) UpsertSession(ctx context.Context, contentSessionID, project string) (*SdkSession, error) {
	memorySessionID := generateID()
	now := time.Now()
	epoch := now.Unix()

	var s SdkSession
	err := db.Pool.QueryRow(ctx, `
		INSERT INTO sdk_sessions (content_session_id, memory_session_id, project, started_at, started_at_epoch, status)
		VALUES ($1, $2, $3, $4, $5, 'active')
		ON CONFLICT (content_session_id) DO UPDATE SET project = EXCLUDED.project
		RETURNING id, content_session_id, memory_session_id, project, user_prompt,
		          started_at, started_at_epoch, completed_at, completed_at_epoch, status,
		          sync_id, sync_version, sync_source
	`, contentSessionID, memorySessionID, project, now, epoch).Scan(
		&s.ID, &s.ContentSessionID, &s.MemorySessionID, &s.Project, &s.UserPrompt,
		&s.StartedAt, &s.StartedAtEpoch, &s.CompletedAt, &s.CompletedAtEpoch, &s.Status,
		&s.SyncID, &s.SyncVersion, &s.SyncSource,
	)
	if err != nil {
		return nil, fmt.Errorf("upsert session: %w", err)
	}
	return &s, nil
}

// FindSessionByContentID looks up a session by its Claude Code session ID.
func (db *DB) FindSessionByContentID(ctx context.Context, contentSessionID string) (*SdkSession, error) {
	var s SdkSession
	err := db.Pool.QueryRow(ctx, `
		SELECT id, content_session_id, memory_session_id, project, user_prompt,
		       started_at, started_at_epoch, completed_at, completed_at_epoch, status,
		       sync_id, sync_version, sync_source
		FROM sdk_sessions
		WHERE content_session_id = $1
	`, contentSessionID).Scan(
		&s.ID, &s.ContentSessionID, &s.MemorySessionID, &s.Project, &s.UserPrompt,
		&s.StartedAt, &s.StartedAtEpoch, &s.CompletedAt, &s.CompletedAtEpoch, &s.Status,
		&s.SyncID, &s.SyncVersion, &s.SyncSource,
	)
	if err != nil {
		return nil, fmt.Errorf("find session: %w", err)
	}
	return &s, nil
}

// CompleteSession marks a session as completed.
func (db *DB) CompleteSession(ctx context.Context, contentSessionID string) error {
	now := time.Now()
	epoch := now.Unix()
	_, err := db.Pool.Exec(ctx, `
		UPDATE sdk_sessions
		SET status = 'completed', completed_at = $2, completed_at_epoch = $3
		WHERE content_session_id = $1
	`, contentSessionID, now, epoch)
	if err != nil {
		return fmt.Errorf("complete session: %w", err)
	}
	return nil
}
