package database

import (
	"context"
	"fmt"
	"time"
)

// StorePrompt inserts a user prompt and returns its ID and prompt number.
func (db *DB) StorePrompt(ctx context.Context, contentSessionID, project, prompt string) (int64, int, error) {
	now := time.Now()
	epoch := now.Unix()

	// Get next prompt number for this session
	var promptNumber int
	err := db.Pool.QueryRow(ctx, `
		SELECT COALESCE(MAX(prompt_number), 0) + 1
		FROM user_prompts
		WHERE content_session_id = $1
	`, contentSessionID).Scan(&promptNumber)
	if err != nil {
		return 0, 0, fmt.Errorf("get prompt number: %w", err)
	}

	var id int64
	err = db.Pool.QueryRow(ctx, `
		INSERT INTO user_prompts (content_session_id, project, prompt, prompt_number, created_at, created_at_epoch)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id
	`, contentSessionID, project, prompt, promptNumber, now, epoch).Scan(&id)
	if err != nil {
		return 0, 0, fmt.Errorf("store prompt: %w", err)
	}

	return id, promptNumber, nil
}
