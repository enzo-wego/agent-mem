package jobs

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

// nullLeaseGrace bounds abandoned claims from versions that did not set a lease.
const nullLeaseGrace = 30 * time.Minute

// Done jobs older than doneRetention are deleted once per pruneInterval, in
// pruneBatch-row deletes. fetch_body and describe_attachment rows are kept:
// their handlers read each node's newest job, done or failed.
const (
	doneRetention = 30 * 24 * time.Hour
	pruneInterval = time.Hour
	pruneBatch    = 50000
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
	var lastPrune time.Time
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
			if time.Since(lastPrune) >= pruneInterval {
				lastPrune = time.Now()
				if n, err := j.prune(ctx); err != nil {
					log.Error().Err(err).Msg("janitor prune failed")
				} else if n > 0 {
					log.Info().Int("deleted", n).Msg("pruned old done jobs")
				}
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
			  AND (lease_until < NOW()
			       OR (lease_until IS NULL AND locked_at < NOW() - ($2::bigint * INTERVAL '1 second')))
			ORDER BY COALESCE(lease_until, locked_at) ASC
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
	`, j.cfg.BatchSize, int(nullLeaseGrace/time.Second))
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// prune deletes done jobs past doneRetention and returns the number deleted.
func (j *Janitor) prune(ctx context.Context) (int, error) {
	total := 0
	for {
		tag, err := j.cfg.DB.Exec(ctx, `
			DELETE FROM graph.jobs WHERE id IN (
				SELECT id FROM graph.jobs
				WHERE status = 'done'
				  AND type NOT IN ('fetch_body', 'describe_attachment')
				  AND completed_at < NOW() - ($1::bigint * INTERVAL '1 second')
				LIMIT $2)`, int64(doneRetention/time.Second), pruneBatch)
		if err != nil {
			return total, err
		}
		n := int(tag.RowsAffected())
		total += n
		if n < pruneBatch || ctx.Err() != nil {
			return total, nil
		}
	}
}
