package jobs_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
	"github.com/rs/zerolog"
	"golang.org/x/sync/semaphore"
)

func TestE2E_StuckJobRecoveredAndCompleted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool := testDB(t)

	var calls atomic.Int32
	reg := jobs.NewRegistry()
	reg.Register("fetch_body", jobs.Entry{
		PoolSize: 1,
		Lease:    1 * time.Second, // short lease so we can simulate death
		Handler: func(ctx context.Context, _ []byte) error {
			n := calls.Add(1)
			if n == 1 {
				// Simulate worker death — sleep past lease, never return cleanly.
				time.Sleep(3 * time.Second)
				return errors.New("simulated death")
			}
			return nil
		},
	})

	mgr := jobs.NewManager(jobs.ManagerConfig{
		Registry:            reg,
		DB:                  pool,
		WorkerID:            "w-test",
		Runner:              "vps",
		Semaphores:          map[string]*semaphore.Weighted{},
		IdleInterval:        100 * time.Millisecond,
		BackoffBase:         200 * time.Millisecond,
		BackoffCap:          1 * time.Second,
		JanitorScanInterval: 200 * time.Millisecond,
		Logger:              zerolog.Nop(),
	})
	id := mustEnqueueWithMaxAttempts(t, pool, "fetch_body", []byte(`{}`), 0, 3)
	go mgr.Run(ctx)

	// Wait for the second call to land (recovery via janitor).
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if calls.Load() >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if calls.Load() < 2 {
		t.Fatalf("janitor did not recover stuck job; calls=%d", calls.Load())
	}

	// Wait for status=done.
	waitForStatus(t, pool, id, "done", 5*time.Second)
}
