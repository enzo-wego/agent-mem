package jobs_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
	"github.com/rs/zerolog"
	"golang.org/x/sync/semaphore"
)

func TestTypeDispatcher_RunsPooledJobs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool := testDB(t)

	// Enqueue 20 fetch_body jobs.
	for i := 0; i < 20; i++ {
		mustEnqueue(t, pool, "fetch_body", []byte(`{}`), 0)
	}

	var ran atomic.Int32
	var inflight atomic.Int32
	var maxInflight atomic.Int32

	reg := jobs.NewRegistry()
	reg.Register("fetch_body", jobs.Entry{
		PoolSize: 4,
		Lease:    5 * time.Second,
		Handler: func(ctx context.Context, payload []byte) error {
			cur := inflight.Add(1)
			for {
				old := maxInflight.Load()
				if cur <= old || maxInflight.CompareAndSwap(old, cur) {
					break
				}
			}
			defer inflight.Add(-1)
			time.Sleep(50 * time.Millisecond)
			ran.Add(1)
			return nil
		},
	})

	d := jobs.NewTypeDispatcher(jobs.DispatcherConfig{
		Type:         "fetch_body",
		Registry:     reg,
		DB:           pool,
		WorkerID:     "w-test",
		Runner:       "vps",
		IdleInterval: 100 * time.Millisecond,
		Semaphores:   map[string]*semaphore.Weighted{},
		BackoffBase:  1 * time.Second,
		BackoffCap:   10 * time.Second,
		Logger:       zerolog.Nop(),
	})

	go d.Run(ctx)

	// Wait for queue to drain.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if ran.Load() == 20 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()

	if ran.Load() != 20 {
		t.Fatalf("ran %d/20 jobs", ran.Load())
	}
	if maxInflight.Load() > 4 {
		t.Errorf("max inflight %d > pool size 4", maxInflight.Load())
	}
}

func TestTypeDispatcher_RetryOn5xx(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool := testDB(t)

	var calls atomic.Int32

	reg := jobs.NewRegistry()
	reg.Register("fetch_body", jobs.Entry{
		PoolSize: 1,
		Lease:    5 * time.Second,
		Handler: func(ctx context.Context, payload []byte) error {
			n := calls.Add(1)
			if n < 3 {
				return jobs.NewHTTPError(503, "down")
			}
			return nil
		},
	})

	d := jobs.NewTypeDispatcher(jobs.DispatcherConfig{
		Type:        "fetch_body",
		Registry:    reg,
		DB:          pool,
		WorkerID:    "w-1",
		Runner:      "vps",
		BackoffBase: 200 * time.Millisecond,
		BackoffCap:  2 * time.Second,
		Semaphores:  map[string]*semaphore.Weighted{},
		Logger:      zerolog.Nop(),
	})
	mustEnqueueWithMaxAttempts(t, pool, "fetch_body", []byte(`{}`), 0, 5)
	go d.Run(ctx)

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if calls.Load() >= 3 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if calls.Load() < 3 {
		t.Fatalf("expected 3 attempts, got %d", calls.Load())
	}
}

func TestTypeDispatcher_FailOn4xx(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool := testDB(t)

	reg := jobs.NewRegistry()
	reg.Register("fetch_body", jobs.Entry{
		PoolSize: 1,
		Lease:    5 * time.Second,
		Handler: func(ctx context.Context, payload []byte) error {
			return jobs.NewHTTPError(404, "missing")
		},
	})

	d := jobs.NewTypeDispatcher(jobs.DispatcherConfig{
		Type:       "fetch_body",
		Registry:   reg,
		DB:         pool,
		WorkerID:   "w-1",
		Runner:     "vps",
		Semaphores: map[string]*semaphore.Weighted{},
		Logger:     zerolog.Nop(),
	})
	id := mustEnqueue(t, pool, "fetch_body", []byte(`{}`), 0)
	go d.Run(ctx)

	waitForStatus(t, pool, id, "failed", 3*time.Second)
}
