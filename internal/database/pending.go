package database

import (
	"context"
	"fmt"
	"time"
)

// QueuePendingMessage inserts a new pending message for async processing.
func (db *DB) QueuePendingMessage(ctx context.Context, contentSessionID, messageType string, payload []byte) (int64, error) {
	var id int64
	err := db.Pool.QueryRow(ctx, `
		INSERT INTO pending_messages (content_session_id, message_type, payload, status)
		VALUES ($1, $2, $3, 'pending')
		RETURNING id
	`, contentSessionID, messageType, payload).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("queue pending message: %w", err)
	}
	return id, nil
}

// ClaimPendingMessage atomically claims the oldest pending message for processing.
// Returns nil if no messages are pending.
func (db *DB) ClaimPendingMessage(ctx context.Context) (*PendingMessage, error) {
	var msg PendingMessage
	err := db.Pool.QueryRow(ctx, `
		UPDATE pending_messages
		SET status = 'processing', attempts = attempts + 1
		WHERE id = (
			SELECT id FROM pending_messages
			WHERE status = 'pending'
			ORDER BY created_at ASC
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, content_session_id, message_type, payload, status, attempts, created_at, processed_at, error
	`).Scan(
		&msg.ID, &msg.ContentSessionID, &msg.MessageType, &msg.Payload,
		&msg.Status, &msg.Attempts, &msg.CreatedAt, &msg.ProcessedAt, &msg.Error,
	)
	if err != nil {
		if err.Error() == "no rows in result set" {
			return nil, nil
		}
		return nil, fmt.Errorf("claim pending message: %w", err)
	}
	return &msg, nil
}

// MarkMessageProcessed marks a pending message as completed.
func (db *DB) MarkMessageProcessed(ctx context.Context, id int) error {
	now := time.Now()
	_, err := db.Pool.Exec(ctx, `
		UPDATE pending_messages
		SET status = 'completed', processed_at = $2
		WHERE id = $1
	`, id, now)
	if err != nil {
		return fmt.Errorf("mark processed: %w", err)
	}
	return nil
}

// RequeuePendingMessage returns a claimed message to 'pending' so it is retried,
// recording why it bounced.
//
// This exists because MarkMessageFailed is terminal — ClaimPendingMessage only
// ever selects status='pending', so a failed row is never looked at again. Using
// it for a transient fault (the LLM gateway down, a 503 quota window, a network
// blip) silently discards the observation. Callers must therefore distinguish
// "will never work" from "try later"; see llmgateway.IsRetryable.
//
// Requeueing here deliberately preserves attempts: message-specific failures
// consume the budget incremented atomically by ClaimPendingMessage.
func (db *DB) RequeuePendingMessage(ctx context.Context, id int, errMsg string) error {
	_, err := db.Pool.Exec(ctx, `
		UPDATE pending_messages
		SET status = 'pending', error = $2
		WHERE id = $1
	`, id, errMsg)
	if err != nil {
		return fmt.Errorf("requeue pending message: %w", err)
	}
	return nil
}

// RequeueRetryablePendingMessage returns a claimed message to 'pending' and
// refunds the claim attempt. Infrastructure failures are not caused by the
// message and therefore must not consume its retry budget.
func (db *DB) RequeueRetryablePendingMessage(ctx context.Context, id int, errMsg string) error {
	_, err := db.Pool.Exec(ctx, `
		UPDATE pending_messages
		SET status = 'pending', error = $2, attempts = GREATEST(attempts - 1, 0)
		WHERE id = $1
	`, id, errMsg)
	if err != nil {
		return fmt.Errorf("requeue retryable pending message: %w", err)
	}
	return nil
}

// MarkMessageFailed marks a pending message as failed after its retry budget is
// spent. This is terminal: nothing re-claims a failed row.
func (db *DB) MarkMessageFailed(ctx context.Context, id int, errMsg string) error {
	now := time.Now()
	_, err := db.Pool.Exec(ctx, `
		UPDATE pending_messages
		SET status = 'failed', processed_at = $2, error = $3
		WHERE id = $1
	`, id, now, errMsg)
	if err != nil {
		return fmt.Errorf("mark failed: %w", err)
	}
	return nil
}

// PendingMessageCount returns the number of pending messages.
func (db *DB) PendingMessageCount(ctx context.Context) (int, error) {
	var count int
	err := db.Pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM pending_messages WHERE status = 'pending'
	`).Scan(&count)
	return count, err
}
