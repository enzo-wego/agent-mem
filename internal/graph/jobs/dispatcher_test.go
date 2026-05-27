package jobs_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
	"github.com/rs/zerolog"
)

func TestDispatcher_RunsPooledJobs(t *testing.T) {
	pool := openTestDB(t)
	truncateJobsTable(t, pool)
	ctx := context.Background()

	const total = 20
	const poolSize = 4

	for i := 0; i < total; i++ {
		if _, err := jobs.EnqueueRaw(ctx, pool, "pooled_type", []byte(`{}`), defaultOpts("machine1")); err != nil {
			t.Fatalf("EnqueueRaw: %v", err)
		}
	}

	var concurrent int64
	var maxConcurrent int64
	var ran int64

	sems := jobs.NewSemaphores(jobs.DefaultRate())
	d := jobs.NewDispatcher(pool, sems, "worker1", "any", zerolog.Nop())
	d.IdleInterval = 50 * time.Millisecond
	d.Register("pooled_type", jobs.HandlerInfo{
		Handler: func(_ context.Context, _ []byte) error {
			c := atomic.AddInt64(&concurrent, 1)
			// Track peak.
			for {
				old := atomic.LoadInt64(&maxConcurrent)
				if c <= old || atomic.CompareAndSwapInt64(&maxConcurrent, old, c) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			atomic.AddInt64(&concurrent, -1)
			atomic.AddInt64(&ran, 1)
			return nil
		},
		PoolSize: poolSize,
		Timeout:  5 * time.Second,
	})

	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	d.Run(runCtx)

	// Wait until all jobs processed or timeout.
	deadline := time.Now().Add(9 * time.Second)
	for time.Now().Before(deadline) {
		depths, err := jobs.QueueDepth(ctx, pool, "pooled_type")
		if err != nil {
			t.Fatalf("QueueDepth: %v", err)
		}
		if depths["done"] == total {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	cancel()
	d.Wait()

	if atomic.LoadInt64(&ran) != total {
		t.Errorf("ran = %d, want %d", ran, total)
	}
	if mc := atomic.LoadInt64(&maxConcurrent); mc > poolSize {
		t.Errorf("max concurrent = %d, exceeded pool size %d", mc, poolSize)
	}
}

func TestDispatcher_RetryOn5xx(t *testing.T) {
	pool := openTestDB(t)
	truncateJobsTable(t, pool)
	ctx := context.Background()

	if _, err := jobs.EnqueueRaw(ctx, pool, "retry5xx_type", []byte(`{}`),
		jobs.EnqueueOptions{MachineID: "machine1", MaxAttempts: 5}); err != nil {
		t.Fatalf("EnqueueRaw: %v", err)
	}

	var calls int64

	sems := jobs.NewSemaphores(jobs.DefaultRate())
	d := jobs.NewDispatcher(pool, sems, "worker1", "any", zerolog.Nop())
	d.IdleInterval = 20 * time.Millisecond
	d.BackoffBase = 100 * time.Millisecond
	d.BackoffCap = 500 * time.Millisecond
	d.Register("retry5xx_type", jobs.HandlerInfo{
		Handler: func(_ context.Context, _ []byte) error {
			n := atomic.AddInt64(&calls, 1)
			if n < 3 {
				return jobs.NewHTTPError(503, "unavailable")
			}
			return nil
		},
		PoolSize: 1,
		Timeout:  5 * time.Second,
	})

	runCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	d.Run(runCtx)

	// Wait for done.
	deadline := time.Now().Add(14 * time.Second)
	for time.Now().Before(deadline) {
		depths, _ := jobs.QueueDepth(ctx, pool, "retry5xx_type")
		if depths["done"] == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	d.Wait()

	if atomic.LoadInt64(&calls) != 3 {
		t.Errorf("calls = %d, want 3", atomic.LoadInt64(&calls))
	}
}

func TestDispatcher_FailOn4xx(t *testing.T) {
	pool := openTestDB(t)
	truncateJobsTable(t, pool)
	ctx := context.Background()

	if _, err := jobs.EnqueueRaw(ctx, pool, "fail4xx_type", []byte(`{}`),
		jobs.EnqueueOptions{MachineID: "machine1", MaxAttempts: 5}); err != nil {
		t.Fatalf("EnqueueRaw: %v", err)
	}

	sems := jobs.NewSemaphores(jobs.DefaultRate())
	d := jobs.NewDispatcher(pool, sems, "worker1", "any", zerolog.Nop())
	d.IdleInterval = 20 * time.Millisecond
	d.Register("fail4xx_type", jobs.HandlerInfo{
		Handler: func(_ context.Context, _ []byte) error {
			return jobs.NewHTTPError(404, "not found")
		},
		PoolSize: 1,
		Timeout:  5 * time.Second,
	})

	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	d.Run(runCtx)

	deadline := time.Now().Add(9 * time.Second)
	for time.Now().Before(deadline) {
		depths, _ := jobs.QueueDepth(ctx, pool, "fail4xx_type")
		if depths["failed"] == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	d.Wait()

	depths, err := jobs.QueueDepth(ctx, pool, "fail4xx_type")
	if err != nil {
		t.Fatalf("QueueDepth: %v", err)
	}
	if depths["failed"] != 1 {
		t.Errorf("failed = %d, want 1; depths=%v", depths["failed"], depths)
	}
}

func TestDispatcher_HonoursTimeout(t *testing.T) {
	pool := openTestDB(t)
	truncateJobsTable(t, pool)
	ctx := context.Background()

	if _, err := jobs.EnqueueRaw(ctx, pool, "timeout_type", []byte(`{}`),
		jobs.EnqueueOptions{MachineID: "machine1", MaxAttempts: 2}); err != nil {
		t.Fatalf("EnqueueRaw: %v", err)
	}

	var ctxErr atomic.Value

	sems := jobs.NewSemaphores(jobs.DefaultRate())
	d := jobs.NewDispatcher(pool, sems, "worker1", "any", zerolog.Nop())
	d.IdleInterval = 20 * time.Millisecond
	d.BackoffBase = 100 * time.Millisecond
	d.BackoffCap = 500 * time.Millisecond
	d.Register("timeout_type", jobs.HandlerInfo{
		Handler: func(handlerCtx context.Context, _ []byte) error {
			select {
			case <-handlerCtx.Done():
				ctxErr.Store(handlerCtx.Err())
				return fmt.Errorf("handler timed out: %w", handlerCtx.Err())
			case <-time.After(30 * time.Second):
				return nil
			}
		},
		PoolSize: 1,
		Timeout:  200 * time.Millisecond, // short timeout
	})

	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	d.Run(runCtx)

	// The job should be retried (ctx.Err() -> DeadlineExceeded -> retryable).
	// Wait for it to be retried at least once (attempts>1) or failed due to max_attempts.
	deadline := time.Now().Add(9 * time.Second)
	for time.Now().Before(deadline) {
		var attempts int16
		err := pool.QueryRow(ctx,
			`SELECT attempts FROM graph.jobs WHERE type = 'timeout_type' LIMIT 1`,
		).Scan(&attempts)
		if err == nil && attempts >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	d.Wait()

	// The handler ctx.Err() should have been context.DeadlineExceeded.
	stored := ctxErr.Load()
	if stored == nil {
		t.Fatal("handler context error never set")
	}
	if stored.(error) != context.DeadlineExceeded {
		t.Errorf("ctx.Err() = %v, want DeadlineExceeded", stored)
	}
}
