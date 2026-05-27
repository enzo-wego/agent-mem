// Package jobs provides a Postgres-backed job queue for the graph processing pipeline.
// Jobs are stored in graph.jobs and claimed atomically via SKIP LOCKED.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB is the minimal interface for queue ops. Satisfied by *pgxpool.Pool and pgx.Tx.
type DB interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Job is one row claimed from graph.jobs.
type Job struct {
	ID           int64
	Type         string
	Payload      []byte
	Priority     int16
	Attempts     int16
	MaxAttempts  int16
	AvailableAt  time.Time
	LockedBy     string
	LockedAt     time.Time
	TargetRunner string
}

// EnqueueOptions configures Enqueue.
type EnqueueOptions struct {
	Priority     int16
	AvailableAt  time.Time // zero = NOW()
	MaxAttempts  int16     // 0 = default 5
	TargetRunner string    // "any" | "vps" | "local"; "" -> "any"
	MachineID    string    // origin of this enqueue (required)
}

// Enqueue adds a new job row. Payload must be JSON-encodable.
func Enqueue(ctx context.Context, db DB, jobType string, payload any, opts EnqueueOptions) (int64, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("enqueue marshal: %w", err)
	}
	return EnqueueRaw(ctx, db, jobType, raw, opts)
}

// EnqueueRaw bypasses marshalling — payload is raw JSON bytes.
func EnqueueRaw(ctx context.Context, db DB, jobType string, payload []byte, opts EnqueueOptions) (int64, error) {
	priority := opts.Priority
	if priority == 0 {
		priority = 5
	}
	maxAttempts := opts.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = 5
	}
	targetRunner := opts.TargetRunner
	if targetRunner == "" {
		targetRunner = "any"
	}

	var id int64
	var err error
	if opts.AvailableAt.IsZero() {
		err = db.QueryRow(ctx, `
			INSERT INTO graph.jobs (type, payload, priority, max_attempts, target_runner, machine_id)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING id`,
			jobType, payload, priority, maxAttempts, targetRunner, opts.MachineID,
		).Scan(&id)
	} else {
		err = db.QueryRow(ctx, `
			INSERT INTO graph.jobs (type, payload, priority, max_attempts, target_runner, machine_id, available_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING id`,
			jobType, payload, priority, maxAttempts, targetRunner, opts.MachineID, opts.AvailableAt,
		).Scan(&id)
	}
	if err != nil {
		return 0, fmt.Errorf("enqueue insert: %w", err)
	}
	return id, nil
}

const claimSQL = `
WITH next AS (
  SELECT id FROM graph.jobs
  WHERE status = 'queued'
    AND available_at <= NOW()
    AND type = $1
    AND target_runner IN ('any', $2)
  ORDER BY priority ASC, available_at ASC, id ASC
  FOR UPDATE SKIP LOCKED
  LIMIT 1
)
UPDATE graph.jobs
SET status      = 'running',
    locked_by   = $3,
    locked_at   = NOW(),
    attempts    = attempts + 1
WHERE id IN (SELECT id FROM next)
RETURNING id, type, payload, priority, attempts, max_attempts,
          available_at, locked_by, locked_at, target_runner`

// Claim atomically picks one queued job of the given type, sets status='running',
// and returns it. Returns (nil, nil) when the queue is empty.
func Claim(ctx context.Context, db DB, jobType string, workerID string, runner string) (*Job, error) {
	j := &Job{}
	err := db.QueryRow(ctx, claimSQL, jobType, runner, workerID).Scan(
		&j.ID, &j.Type, &j.Payload, &j.Priority, &j.Attempts, &j.MaxAttempts,
		&j.AvailableAt, &j.LockedBy, &j.LockedAt, &j.TargetRunner,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	return j, nil
}

// Complete marks a job as done.
func Complete(ctx context.Context, db DB, id int64) error {
	_, err := db.Exec(ctx, `
		UPDATE graph.jobs
		SET status = 'done', completed_at = NOW()
		WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("complete job %d: %w", id, err)
	}
	return nil
}

// Retry resets the job to 'queued' with available_at = NOW() + delay.
func Retry(ctx context.Context, db DB, id int64, runErr error, delay time.Duration) error {
	var errMsg string
	if runErr != nil {
		errMsg = runErr.Error()
	}
	_, err := db.Exec(ctx, `
		UPDATE graph.jobs
		SET status       = 'queued',
		    locked_by    = NULL,
		    locked_at    = NULL,
		    available_at = NOW() + $2::interval,
		    last_error   = $3
		WHERE id = $1`,
		id, fmt.Sprintf("%d microseconds", delay.Microseconds()), errMsg,
	)
	if err != nil {
		return fmt.Errorf("retry job %d: %w", id, err)
	}
	return nil
}

// Fail marks a job 'failed'. Used when max_attempts is reached or err is non-retryable.
func Fail(ctx context.Context, db DB, id int64, runErr error) error {
	var errMsg string
	if runErr != nil {
		errMsg = runErr.Error()
	}
	_, err := db.Exec(ctx, `
		UPDATE graph.jobs
		SET status     = 'failed',
		    last_error = $2
		WHERE id = $1`,
		id, errMsg,
	)
	if err != nil {
		return fmt.Errorf("fail job %d: %w", id, err)
	}
	return nil
}

// QueueDepth returns counts grouped by status for the given type (or all types
// if jobType="").
func QueueDepth(ctx context.Context, db DB, jobType string) (map[string]int, error) {
	var rows pgx.Rows
	var err error
	if jobType == "" {
		rows, err = db.Query(ctx, `
			SELECT status, COUNT(*) FROM graph.jobs GROUP BY status`)
	} else {
		rows, err = db.Query(ctx, `
			SELECT status, COUNT(*) FROM graph.jobs WHERE type = $1 GROUP BY status`, jobType)
	}
	if err != nil {
		return nil, fmt.Errorf("queue depth: %w", err)
	}
	defer rows.Close()

	result := make(map[string]int)
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, fmt.Errorf("queue depth scan: %w", err)
		}
		result[status] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue depth rows: %w", err)
	}
	return result, nil
}

// ensure *pgxpool.Pool satisfies DB at compile time.
var _ DB = (*pgxpool.Pool)(nil)
