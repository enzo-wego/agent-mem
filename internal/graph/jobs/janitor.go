package jobs

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

// JanitorConfig configures the lease-expiry reclaimer.
type JanitorConfig struct {
	DB           *pgxpool.Pool
	ScanInterval time.Duration // default 30s
	BatchSize    int           // default 100
	Logger       zerolog.Logger
}

// Janitor periodically resets running jobs whose lease has expired.
type Janitor struct {
	cfg JanitorConfig
}

// NewJanitor creates a Janitor with defaults applied.
func NewJanitor(cfg JanitorConfig) *Janitor {
	if cfg.ScanInterval == 0 {
		cfg.ScanInterval = 30 * time.Second
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = 100
	}
	return &Janitor{cfg: cfg}
}

// Run loops until ctx is cancelled.
func (j *Janitor) Run(ctx context.Context) {
	log := j.cfg.Logger.With().Str("component", "janitor").Logger()
	log.Info().Dur("interval", j.cfg.ScanInterval).Msg("janitor starting")
	t := time.NewTicker(j.cfg.ScanInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("janitor stopping")
			return
		case <-t.C:
			n, err := j.scan(ctx)
			if err != nil {
				log.Error().Err(err).Msg("janitor scan failed")
				continue
			}
			if n > 0 {
				log.Warn().Int("reclaimed", n).Msg("requeued expired leases")
			}
		}
	}
}

// scan returns the number of rows reclaimed.
func (j *Janitor) scan(ctx context.Context) (int, error) {
	tag, err := j.cfg.DB.Exec(ctx, `
		WITH expired AS (
			SELECT id FROM graph.jobs
			WHERE status = 'running'
			  AND lease_until IS NOT NULL
			  AND lease_until < NOW()
			ORDER BY lease_until ASC
			FOR UPDATE SKIP LOCKED
			LIMIT $1
		)
		UPDATE graph.jobs jb
		SET status      = 'queued',
		    locked_by   = NULL,
		    locked_at   = NULL,
		    lease_until = NULL,
		    last_error  = COALESCE(last_error, '') ||
		                  CASE WHEN last_error IS NOT NULL AND last_error <> '' THEN '; ' ELSE '' END ||
		                  'janitor: lease expired'
		FROM expired e
		WHERE jb.id = e.id
	`, j.cfg.BatchSize)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
