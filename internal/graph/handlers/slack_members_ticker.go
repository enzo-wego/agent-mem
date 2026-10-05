package handlers

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

func slackMembersTick(ctx context.Context, db *pgxpool.Pool, machineID, runner string, now time.Time) error {
	due, err := slackMembersDue(ctx, db, now)
	if err != nil {
		return err
	}
	if !due {
		return nil
	}
	if runner == "" {
		runner = "any"
	}
	_, err = db.Exec(ctx, `INSERT INTO graph.jobs(type,payload,priority,machine_id,target_runner)
  SELECT 'refresh_slack_members','{}',5,$1,$2
  WHERE NOT EXISTS(SELECT 1 FROM graph.jobs WHERE type='refresh_slack_members' AND status IN('queued','running'))`, machineID, runner)
	if err != nil {
		return fmt.Errorf("enqueue refresh_slack_members: %w", err)
	}
	return nil
}

// RunSlackMembersTicker polls persisted last-attempt state once a minute.
// Failures do not create retry chains; manual force jobs retain queue backoff.
func RunSlackMembersTicker(ctx context.Context, db *pgxpool.Pool, machineID, runner string, log zerolog.Logger) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := slackMembersTick(ctx, db, machineID, runner, now); err != nil && ctx.Err() == nil {
				log.Error().Err(err).Msg("slack members ticker: tick failed")
			}
		}
	}
}
