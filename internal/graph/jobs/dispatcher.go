package jobs

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"golang.org/x/sync/semaphore"
)

// DispatcherConfig is the constructor input for TypeDispatcher.
type DispatcherConfig struct {
	Type         string
	Registry     *Registry
	DB           *pgxpool.Pool
	WorkerID     string                       // unique string per process: host+pid+uuid
	Runner       string                       // "vps" or "local"
	IdleInterval time.Duration                // poll cadence when queue empty (default 5s)
	Semaphores   map[string]*semaphore.Weighted // per-system rate limiters
	BackoffBase  time.Duration                // default 30s
	BackoffCap   time.Duration                // default 1h
	Logger       zerolog.Logger

	// Paused reports whether job execution is suspended. Checked before every
	// claim, so flipping it takes effect within one idle interval and needs no
	// restart. Jobs keep being *enqueued* while paused — they simply are not
	// claimed, so the queue backs up harmlessly and drains when unpaused.
	// This exists to survive a spent LLM budget without dropping ingest: the
	// alternative (stopping the worker) makes the HTTP API unavailable, and
	// inbound webhooks are then lost rather than queued. nil = never paused.
	Paused func() bool
}

// TypeDispatcher claims and runs jobs of one specific type.
type TypeDispatcher struct {
	cfg   DispatcherConfig
	pool  *semaphore.Weighted // pool slots = registry.PoolSize for this type
	entry Entry
}

// NewTypeDispatcher returns a dispatcher ready to Run.
func NewTypeDispatcher(cfg DispatcherConfig) *TypeDispatcher {
	if cfg.IdleInterval == 0 {
		cfg.IdleInterval = 5 * time.Second
	}
	if cfg.BackoffBase == 0 {
		cfg.BackoffBase = 30 * time.Second
	}
	if cfg.BackoffCap == 0 {
		cfg.BackoffCap = 1 * time.Hour
	}
	entry, _ := cfg.Registry.Get(cfg.Type)
	poolSize := entry.PoolSize
	if poolSize <= 0 {
		poolSize = 4
	}
	return &TypeDispatcher{
		cfg:   cfg,
		pool:  semaphore.NewWeighted(int64(poolSize)),
		entry: entry,
	}
}

// Run loops until ctx is cancelled. Chain-after-success: try claiming again
// immediately after launching a worker. Slow-poll when the queue is empty.
func (d *TypeDispatcher) Run(ctx context.Context) {
	log := d.cfg.Logger.With().Str("dispatcher", d.cfg.Type).Logger()
	log.Info().Int("pool", d.entry.PoolSize).Msg("dispatcher starting")
	for ctx.Err() == nil {
		// Acquire a pool slot, blocking until one is free or ctx is done.
		if err := d.pool.Acquire(ctx, 1); err != nil {
			return
		}
		// Paused: release the slot and idle without claiming. Deliberately after
		// Acquire so the check costs nothing extra, and before Claim so a paused
		// worker never takes a lease it won't honour.
		if d.cfg.Paused != nil && d.cfg.Paused() {
			d.pool.Release(1)
			d.sleep(ctx, d.cfg.IdleInterval)
			continue
		}
		lease := d.entry.Lease
		if lease == 0 {
			lease = 60 * time.Second
		}
		job, err := Claim(ctx, d.cfg.DB, d.cfg.Type, lease, d.cfg.WorkerID, d.cfg.Runner)
		if err != nil {
			d.pool.Release(1)
			if !errors.Is(err, context.Canceled) {
				log.Error().Err(err).Msg("claim failed")
			}
			d.sleep(ctx, 1*time.Second)
			continue
		}
		if job == nil {
			d.pool.Release(1)
			d.sleep(ctx, d.cfg.IdleInterval)
			continue
		}
		go func(j *Job) {
			defer d.pool.Release(1)
			d.runOne(ctx, j)
		}(job)
	}
	log.Info().Msg("dispatcher stopping")
}

// sleep returns early if ctx is cancelled.
func (d *TypeDispatcher) sleep(ctx context.Context, dur time.Duration) {
	t := time.NewTimer(dur)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
