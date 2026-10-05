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

func TestE2E_StuckJobRecoveredAndCompleted(t *testing.T) {
	pool := testDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	id := mustEnqueueWithMaxAttempts(t, pool, "fetch_body", []byte(`{}`), 0, 3)
	claimed, err := jobs.Claim(ctx, pool, "fetch_body", time.Minute, "abandoned-worker", "vps")
	must(t, err)
	if claimed == nil || claimed.ID != id {
		t.Fatalf("abandoned claim = %v, want job %d", claimed, id)
	}
	_, err = pool.Exec(ctx, `UPDATE graph.jobs SET lease_until=NOW()-INTERVAL '1 second' WHERE id=$1`, id)
	must(t, err)

	// No dispatcher or heartbeat exists yet: only the janitor can recover this claim.
	janitor := jobs.NewJanitor(jobs.JanitorConfig{
		DB: pool, ScanInterval: 20 * time.Millisecond, Logger: zerolog.Nop(),
	})
	janitorDone := make(chan struct{})
	go func() {
		defer close(janitorDone)
		janitor.Run(ctx)
	}()
	defer func() { cancel(); <-janitorDone }()
	waitForStatus(t, pool, id, "queued", 2*time.Second)

	var lockedBy *string
	var lockedAt, leaseUntil *time.Time
	var lastError string
	var attempts int
	must(t, pool.QueryRow(ctx, `SELECT locked_by,locked_at,lease_until,last_error,attempts FROM graph.jobs WHERE id=$1`, id).
		Scan(&lockedBy, &lockedAt, &leaseUntil, &lastError, &attempts))
	if lockedBy != nil || lockedAt != nil || leaseUntil != nil || lastError != "janitor: lease expired" || attempts != 1 {
		t.Fatalf("reclaim state: locked_by=%v locked_at=%v lease_until=%v last_error=%q attempts=%d",
			lockedBy, lockedAt, leaseUntil, lastError, attempts)
	}

	var calls atomic.Int32
	reg := jobs.NewRegistry()
	reg.Register("fetch_body", jobs.Entry{
		PoolSize: 1,
		Lease:    time.Minute,
		Handler:  func(context.Context, []byte) error { calls.Add(1); return nil },
	})
	mgr := jobs.NewManager(jobs.ManagerConfig{
		Registry: reg, DB: pool, WorkerID: "recovery-worker", Runner: "vps",
		Semaphores:   map[string]*semaphore.Weighted{},
		IdleInterval: 20 * time.Millisecond, Logger: zerolog.Nop(),
	})
	mgr.Run(ctx)
	defer func() { cancel(); mgr.Wait() }()
	waitForStatus(t, pool, id, "done", 5*time.Second)
	must(t, pool.QueryRow(ctx, `SELECT attempts FROM graph.jobs WHERE id=$1`, id).Scan(&attempts))
	if calls.Load() != 1 || attempts != 2 {
		t.Fatalf("recovered job calls=%d attempts=%d, want 1 and 2", calls.Load(), attempts)
	}
}
