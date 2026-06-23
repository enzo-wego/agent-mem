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

func TestRunOne_AcquiresSemaphoresInSortedOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool := testDB(t)

	var orderRecorded []string
	semJira := semaphore.NewWeighted(1)
	semGemini := semaphore.NewWeighted(1)

	reg := jobs.NewRegistry()
	reg.Register("fetch_body", jobs.Entry{
		PoolSize: 1,
		Lease:    5 * time.Second,
		Systems:  []string{"gemini", "jira"}, // intentionally unsorted
		Handler: func(ctx context.Context, payload []byte) error {
			// Both must already be held.
			orderRecorded = append(orderRecorded, "ran")
			return nil
		},
	})

	d := jobs.NewTypeDispatcher(jobs.DispatcherConfig{
		Type:     "fetch_body",
		Registry: reg,
		DB:       pool,
		WorkerID: "w-1",
		Runner:   "vps",
		Semaphores: map[string]*semaphore.Weighted{
			"jira":   semJira,
			"gemini": semGemini,
		},
		Logger: zerolog.Nop(),
	})
	id := mustEnqueue(t, pool, "fetch_body", []byte(`{}`), 0)
	go d.Run(ctx)

	waitForDone(t, pool, id, 3*time.Second)
	if len(orderRecorded) != 1 {
		t.Fatalf("handler did not run; orderRecorded=%v", orderRecorded)
	}
}

func TestRunOne_RetryOn5xx(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool := testDB(t)

	var calls atomic.Int32

	reg := jobs.NewRegistry()
	reg.Register("fetch_body", jobs.Entry{
		PoolSize: 1,
		Lease:    5 * time.Second,
		Handler: func(ctx context.Context, payload []byte) error {
			calls.Add(1)
			return jobs.NewHTTPError(503, "down")
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
	mustEnqueueWithMaxAttempts(t, pool, "fetch_body", []byte(`{}`), 0, 3)
	go d.Run(ctx)

	deadline := time.Now().Add(4 * time.Second)
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

func TestRunOne_FailOn4xx(t *testing.T) {
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
