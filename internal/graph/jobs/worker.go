package jobs

import (
	"context"
	"errors"
	"sort"

	"golang.org/x/sync/semaphore"
)

// runOne acquires the necessary semaphores, runs the handler, applies
// the resulting state transition, and ensures all locks are released.
func (d *TypeDispatcher) runOne(ctx context.Context, job *Job) {
	log := d.cfg.Logger.With().
		Str("job_type", job.Type).
		Int64("job_id", job.ID).
		Int16("attempt", job.Attempts).
		Logger()

	// Acquire semaphores in sorted order to prevent cross-deadlock.
	systems := append([]string(nil), d.entry.Systems...)
	sort.Strings(systems)
	released := make([]*semaphore.Weighted, 0, len(systems))
	releaseAll := func() {
		for i := len(released) - 1; i >= 0; i-- {
			released[i].Release(1)
		}
	}
	for _, s := range systems {
		sem, ok := d.cfg.Semaphores[s]
		if !ok || sem == nil {
			log.Warn().Str("system", s).Msg("no semaphore configured; skipping")
			continue
		}
		if err := sem.Acquire(ctx, 1); err != nil {
			releaseAll()
			// Treat as transient — ctx cancelled or pool starvation.
			_ = Retry(ctx, d.cfg.DB, job.ID, err,
				Backoff(job.Attempts, d.cfg.BackoffBase, d.cfg.BackoffCap))
			return
		}
		released = append(released, sem)
	}
	defer releaseAll()

	// Optional heartbeat goroutine for long-running types.
	var hb *Heartbeat
	if d.entry.Heartbeat {
		hb = StartHeartbeat(ctx, d.cfg.DB, job.ID, d.entry.Lease, log)
	}

	// Run the handler.
	runCtx := ctx
	if d.entry.Lease > 0 && !d.entry.Heartbeat {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, d.entry.Lease)
		defer cancel()
	}
	err := d.entry.Handler(runCtx, job.Payload)
	if hb != nil {
		hb.Stop()
	}

	// Decide state.
	if err == nil {
		if err := Complete(ctx, d.cfg.DB, job.ID); err != nil {
			log.Error().Err(err).Msg("complete failed")
		}
		return
	}
	if errors.Is(err, context.Canceled) {
		// Shutdown in flight; let the janitor reclaim.
		log.Info().Msg("interrupted; lease will expire")
		return
	}
	if job.Attempts >= job.MaxAttempts || !IsRetryable(err) {
		if e := Fail(ctx, d.cfg.DB, job.ID, err); e != nil {
			log.Error().Err(e).Msg("fail failed")
		}
		return
	}
	delay := Backoff(job.Attempts, d.cfg.BackoffBase, d.cfg.BackoffCap)
	if e := Retry(ctx, d.cfg.DB, job.ID, err, delay); e != nil {
		log.Error().Err(e).Msg("retry failed")
	}
	log.Info().Err(err).Dur("delay", delay).Msg("scheduled retry")
}
