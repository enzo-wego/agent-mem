package jobs

import (
	"context"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"golang.org/x/sync/semaphore"
)

// Handler is the signature every job-type handler implements.
type Handler func(ctx context.Context, payload []byte) error

// HandlerInfo bundles a handler with its required semaphore systems.
type HandlerInfo struct {
	Handler  Handler
	Systems  []string      // semaphore names to acquire before running
	PoolSize int           // max concurrent goroutines (default 4)
	Timeout  time.Duration // per-call timeout (default 60s); 0 = no timeout
}

// Dispatcher owns one goroutine per registered type.
type Dispatcher struct {
	DB           *pgxpool.Pool
	Sems         *Semaphores
	WorkerID     string
	Runner       string        // "vps" or "local"
	IdleInterval time.Duration // poll cadence when queue empty; default 5s
	BackoffBase  time.Duration // default 30s
	BackoffCap   time.Duration // default 1h
	Logger       zerolog.Logger

	handlers map[string]HandlerInfo
	wg       sync.WaitGroup
}

// NewDispatcher creates a Dispatcher with defaults.
func NewDispatcher(db *pgxpool.Pool, sems *Semaphores, workerID, runner string, log zerolog.Logger) *Dispatcher {
	return &Dispatcher{
		DB:           db,
		Sems:         sems,
		WorkerID:     workerID,
		Runner:       runner,
		IdleInterval: 5 * time.Second,
		BackoffBase:  30 * time.Second,
		BackoffCap:   time.Hour,
		Logger:       log,
		handlers:     make(map[string]HandlerInfo),
	}
}

// Register binds a handler to a type. Must be called before Run.
func (d *Dispatcher) Register(jobType string, info HandlerInfo) {
	if info.PoolSize <= 0 {
		info.PoolSize = 4
	}
	if info.Timeout == 0 {
		info.Timeout = 60 * time.Second
	}
	d.handlers[jobType] = info
}

// Run starts one goroutine per registered type. Returns when ctx is cancelled.
func (d *Dispatcher) Run(ctx context.Context) {
	for jobType, info := range d.handlers {
		d.wg.Add(1)
		go d.runLoop(ctx, jobType, info)
	}
}

// Wait blocks until all per-type loops have exited.
func (d *Dispatcher) Wait() {
	d.wg.Wait()
}

// runLoop is the per-type polling loop.
func (d *Dispatcher) runLoop(ctx context.Context, jobType string, info HandlerInfo) {
	defer d.wg.Done()

	pool := semaphore.NewWeighted(int64(info.PoolSize))
	log := d.Logger.With().Str("job_type", jobType).Logger()

	for {
		select {
		case <-ctx.Done():
			// Drain: wait for all in-flight workers to finish.
			_ = pool.Acquire(ctx, int64(info.PoolSize))
			return
		default:
		}

		// Acquire a pool slot.
		if err := pool.Acquire(ctx, 1); err != nil {
			// ctx cancelled.
			return
		}

		job, err := Claim(ctx, d.DB, jobType, d.WorkerID, d.Runner)
		if err != nil {
			pool.Release(1)
			log.Error().Err(err).Msg("claim error")
			select {
			case <-ctx.Done():
				return
			case <-time.After(d.IdleInterval):
			}
			continue
		}

		if job == nil {
			pool.Release(1)
			// Queue empty — wait before polling again.
			select {
			case <-ctx.Done():
				return
			case <-time.After(d.IdleInterval):
			}
			continue
		}

		// Spawn worker goroutine.
		go d.runJob(ctx, job, info, pool, log)
	}
}

// runJob executes a single job and applies the appropriate state transition.
func (d *Dispatcher) runJob(ctx context.Context, job *Job, info HandlerInfo, pool *semaphore.Weighted, log zerolog.Logger) {
	defer pool.Release(1)

	log = log.With().Int64("job_id", job.ID).Logger()

	// Acquire system semaphores.
	release, err := d.Sems.AcquireMany(ctx, info.Systems)
	if err != nil {
		log.Error().Err(err).Msg("semaphore acquire failed")
		if retryErr := Retry(context.Background(), d.DB, job.ID, err,
			Backoff(job.Attempts, d.BackoffBase, d.BackoffCap)); retryErr != nil {
			log.Error().Err(retryErr).Msg("retry failed")
		}
		return
	}
	defer release()

	// Apply per-call timeout.
	runCtx := ctx
	if info.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, info.Timeout)
		defer cancel()
	}

	handlerErr := info.Handler(runCtx, job.Payload)

	if handlerErr == nil {
		if err := Complete(context.Background(), d.DB, job.ID); err != nil {
			log.Error().Err(err).Msg("complete failed")
		}
		return
	}

	// Decide retry vs fail.
	retryable := IsRetryable(handlerErr)
	attemptsExhausted := job.Attempts >= job.MaxAttempts

	if retryable && !attemptsExhausted {
		delay := Backoff(job.Attempts, d.BackoffBase, d.BackoffCap)
		log.Warn().Err(handlerErr).Dur("delay", delay).Int16("attempts", job.Attempts).Msg("retrying job")
		if err := Retry(context.Background(), d.DB, job.ID, handlerErr, delay); err != nil {
			log.Error().Err(err).Msg("retry update failed")
		}
		return
	}

	log.Error().Err(handlerErr).Int16("attempts", job.Attempts).Msg("failing job")
	if err := Fail(context.Background(), d.DB, job.ID, handlerErr); err != nil {
		log.Error().Err(err).Msg("fail update failed")
	}
}

// Backoff returns delay = base * 2^(attempts-1), capped at cap, with ±20% jitter.
func Backoff(attempts int16, base, cap time.Duration) time.Duration {
	if attempts <= 0 {
		attempts = 1
	}
	shift := int(attempts) - 1
	if shift > 62 {
		shift = 62
	}
	delay := base * (1 << uint(shift))
	if delay <= 0 || delay > cap {
		delay = cap
	}
	// ±20% jitter: multiply by a factor in [0.8, 1.2].
	jitter := 0.8 + rand.Float64()*0.4
	delay = time.Duration(float64(delay) * jitter)
	if delay > cap {
		delay = cap
	}
	return delay
}
