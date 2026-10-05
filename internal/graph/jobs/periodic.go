package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

// ErrPeriodicPending means a queued or running row already owns this recurrence.
var ErrPeriodicPending = errors.New("already queued or running")

// IsPeriodic reports whether jobType has a fixed-interval singleton schedule.
func IsPeriodic(jobType string) bool {
	return periodicInterval(jobType) != 0
}

func periodicInterval(jobType string) time.Duration {
	switch jobType {
	case "notify_watch_channels", "detect_hot_topics":
		return 5 * time.Minute
	case "derive_person_roles":
		return 24 * time.Hour
	case "refresh_jira_board":
		return 6 * time.Hour
	default:
		return 0
	}
}

// RunPeriodicJobsTicker owns recurrence independently of job execution results.
// There is no immediate tick: startup cleanup can finish before the first poll.
// ponytail: one ticker owns cadence for fixed-interval singletons; per-key schedules if a type needs more
func RunPeriodicJobsTicker(ctx context.Context, db *pgxpool.Pool, machineID, runner string, log zerolog.Logger) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			PeriodicTick(ctx, db, machineID, runner, now, log)
		}
	}
}

// PeriodicTick checks each fixed schedule once. A failed type is logged without
// blocking subsequent types. The explicit clock also supports deterministic
// integration tests of concurrent ticker and manual enqueue/retry requests.
func PeriodicTick(ctx context.Context, db *pgxpool.Pool, machineID, runner string, now time.Time, log zerolog.Logger) {
	for _, jobType := range [...]string{
		"notify_watch_channels",
		"detect_hot_topics",
		"derive_person_roles",
		"refresh_jira_board",
	} {
		if err := enqueuePeriodicDue(ctx, db, jobType, machineID, runner, now); err != nil {
			log.Error().Err(err).Str("job_type", jobType).Msg("periodic enqueue failed")
		}
	}
}

func enqueuePeriodicDue(ctx context.Context, db *pgxpool.Pool, jobType, machineID, runner string, now time.Time) error {
	return periodicTransaction(ctx, db, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockPeriodic(ctx, tx, jobType); err != nil {
			return err
		}
		pending, err := periodicPending(ctx, tx, jobType, 0)
		if err != nil || pending {
			return err
		}

		key := "graph.periodic." + jobType + ".last_enqueued_at"
		var value string
		err = tx.QueryRow(ctx, `SELECT value FROM settings WHERE key=$1`, key).Scan(&value)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read periodic schedule: %w", err)
		}
		if err == nil {
			last, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				return fmt.Errorf("parse periodic schedule %s: %w", key, err)
			}
			if now.Before(last.Add(periodicInterval(jobType))) {
				return nil
			}
		}

		if _, err := EnqueueRaw(ctx, tx, jobType, []byte(`{}`), periodicOptions(jobType, machineID, runner, now)); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO settings (key, value) VALUES ($1, $2)
			ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value`,
			key, now.UTC().Format(time.RFC3339Nano))
		if err != nil {
			return fmt.Errorf("write periodic schedule: %w", err)
		}
		return nil
	})
}

// EnqueuePeriodicNow manually enqueues one allowlisted periodic type without
// changing its schedule. Tickers and manual requests use the same database lock.
func EnqueuePeriodicNow(ctx context.Context, db *pgxpool.Pool, jobType, machineID, runner string) (int64, error) {
	if !IsPeriodic(jobType) {
		return 0, fmt.Errorf("not a periodic job type: %s", jobType)
	}
	var id int64
	err := periodicTransaction(ctx, db, func(ctx context.Context, tx pgx.Tx) error {
		if err := lockPeriodic(ctx, tx, jobType); err != nil {
			return err
		}
		pending, err := periodicPending(ctx, tx, jobType, 0)
		if err != nil {
			return err
		}
		if pending {
			return ErrPeriodicPending
		}
		id, err = EnqueueRaw(ctx, tx, jobType, []byte(`{}`), periodicOptions(jobType, machineID, runner, time.Now()))
		return err
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// RetryPeriodicNow preserves the failed-only administrative retry semantics for
// every job type. Periodic rows additionally reject pending siblings under the
// ticker's advisory lock, excluding their own ID. It never changes the schedule.
func RetryPeriodicNow(ctx context.Context, db *pgxpool.Pool, id int64) error {
	return periodicTransaction(ctx, db, func(ctx context.Context, tx pgx.Tx) error {
		var jobType string
		err := tx.QueryRow(ctx, `SELECT type FROM graph.jobs WHERE id=$1`, id).Scan(&jobType)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read retry job %d: %w", id, err)
		}
		if IsPeriodic(jobType) {
			if err := lockPeriodic(ctx, tx, jobType); err != nil {
				return err
			}
			pending, err := periodicPending(ctx, tx, jobType, id)
			if err != nil {
				return err
			}
			if pending {
				return ErrPeriodicPending
			}
		}
		_, err = tx.Exec(ctx, `
			UPDATE graph.jobs
			SET status='queued', available_at=NOW(), attempts=0, last_error=NULL
			WHERE id=$1 AND status='failed'`, id)
		if err != nil {
			return fmt.Errorf("retry job %d: %w", id, err)
		}
		return nil
	})
}

func periodicOptions(jobType, machineID, runner string, now time.Time) EnqueueOptions {
	if jobType == "derive_person_roles" || runner == "" || runner == "none" {
		runner = "any"
	}
	return EnqueueOptions{Priority: 5, AvailableAt: now, TargetRunner: runner, MachineID: machineID}
}

// The timeout starts before pool.Begin acquires a connection, not after it.
// pgxpool's transaction releases its connection on either Commit or Rollback.
func periodicTransaction(ctx context.Context, db *pgxpool.Pool, operation func(context.Context, pgx.Tx) error) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin periodic transaction: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout='5s'`); err != nil {
		return fmt.Errorf("set periodic statement timeout: %w", err)
	}
	if err := operation(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit periodic transaction: %w", err)
	}
	return nil
}

func lockPeriodic(ctx context.Context, tx pgx.Tx, jobType string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('periodic:' || $1))`, jobType)
	if err != nil {
		return fmt.Errorf("lock periodic job %s: %w", jobType, err)
	}
	return nil
}

func periodicPending(ctx context.Context, tx pgx.Tx, jobType string, excludeID int64) (bool, error) {
	var pending bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM graph.jobs
			WHERE type=$1 AND status IN ('queued','running') AND id<>$2
		)`, jobType, excludeID).Scan(&pending)
	if err != nil {
		return false, fmt.Errorf("check periodic pending: %w", err)
	}
	return pending, nil
}
