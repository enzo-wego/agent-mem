package jobs

import (
	"context"
	"fmt"
	"time"
)

// Complete marks a job as 'done' with completed_at = NOW().
func Complete(ctx context.Context, db DB, id int64) error {
	_, err := db.Exec(ctx, `
		UPDATE graph.jobs
		SET status       = 'done',
		    completed_at = NOW(),
		    locked_by    = NULL,
		    locked_at    = NULL,
		    lease_until  = NULL
		WHERE id = $1
	`, id)
	if err != nil {
		return fmt.Errorf("complete %d: %w", id, err)
	}
	return nil
}

// Retry resets the job to 'queued' with available_at = NOW() + delay.
// Caller computes delay via Backoff(attempts).
func Retry(ctx context.Context, db DB, id int64, runErr error, delay time.Duration) error {
	msg := ""
	if runErr != nil {
		msg = runErr.Error()
	}
	_, err := db.Exec(ctx, `
		UPDATE graph.jobs
		SET status       = 'queued',
		    locked_by    = NULL,
		    locked_at    = NULL,
		    lease_until  = NULL,
		    available_at = NOW() + ($2 || ' seconds')::interval,
		    last_error   = $3
		WHERE id = $1
	`, id, fmt.Sprintf("%d", int(delay/time.Second)), msg)
	if err != nil {
		return fmt.Errorf("retry %d: %w", id, err)
	}
	return nil
}

// RetryRefund requeues a job and undoes the attempt increment performed by
// Claim. Infrastructure failures are not caused by the job and therefore must
// not consume its retry budget.
func RetryRefund(ctx context.Context, db DB, id int64, runErr error, delay time.Duration) error {
	msg := ""
	if runErr != nil {
		msg = runErr.Error()
	}
	_, err := db.Exec(ctx, `
		UPDATE graph.jobs
		SET status       = 'queued',
		    locked_by    = NULL,
		    locked_at    = NULL,
		    lease_until  = NULL,
		    available_at = NOW() + ($2 || ' seconds')::interval,
		    last_error   = $3,
		    attempts     = GREATEST(attempts - 1, 0)
		WHERE id = $1
	`, id, fmt.Sprintf("%d", int(delay/time.Second)), msg)
	if err != nil {
		return fmt.Errorf("retry refund %d: %w", id, err)
	}
	return nil
}

// Fail marks the job 'failed'. Used when max_attempts is exceeded or the
// error is non-retryable.
func Fail(ctx context.Context, db DB, id int64, runErr error) error {
	msg := ""
	if runErr != nil {
		msg = runErr.Error()
	}
	_, err := db.Exec(ctx, `
		UPDATE graph.jobs
		SET status      = 'failed',
		    last_error  = $2,
		    locked_by   = NULL,
		    locked_at   = NULL,
		    lease_until = NULL
		WHERE id = $1
	`, id, msg)
	if err != nil {
		return fmt.Errorf("fail %d: %w", id, err)
	}
	return nil
}
