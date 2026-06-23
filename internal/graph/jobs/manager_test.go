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

func TestManager_StartsAllDispatchers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool := testDB(t)

	var fetchRan, identRan atomic.Int32

	reg := jobs.NewRegistry()
	reg.Register("fetch_body", jobs.Entry{
		PoolSize: 2,
		Lease:    5 * time.Second,
		Handler: func(ctx context.Context, _ []byte) error {
			fetchRan.Add(1)
			return nil
		},
	})
	reg.Register("resolve_identity", jobs.Entry{
		PoolSize: 2,
		Lease:    5 * time.Second,
		Handler: func(ctx context.Context, _ []byte) error {
			identRan.Add(1)
			return nil
		},
	})

	mgr := jobs.NewManager(jobs.ManagerConfig{
		Registry:            reg,
		DB:                  pool,
		WorkerID:            "w-test",
		Runner:              "vps",
		Semaphores:          map[string]*semaphore.Weighted{},
		IdleInterval:        200 * time.Millisecond,
		JanitorScanInterval: 200 * time.Millisecond,
		Logger:              zerolog.Nop(),
	})

	for i := 0; i < 5; i++ {
		mustEnqueue(t, pool, "fetch_body", []byte(`{}`), 0)
		mustEnqueue(t, pool, "resolve_identity", []byte(`{}`), 0)
	}

	go mgr.Run(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fetchRan.Load() == 5 && identRan.Load() == 5 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	mgr.Wait()

	if fetchRan.Load() != 5 || identRan.Load() != 5 {
		t.Fatalf("fetch=%d ident=%d (want 5/5)", fetchRan.Load(), identRan.Load())
	}
}
