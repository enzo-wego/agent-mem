package jobs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
	"github.com/rs/zerolog"
)

func TestRunOne_HeartbeatMaxRuntimeMatchesLeaseTimeout(t *testing.T) {
	for _, heartbeat := range []bool{false, true} {
		name := "lease"
		if heartbeat {
			name = "heartbeat"
		}
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				name        string
				maxAttempts int16
				wantStatus  string
			}{
				{name: "retry", maxAttempts: 3, wantStatus: "queued"},
				{name: "exhausted", maxAttempts: 1, wantStatus: "failed"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					pool := testDB(t)
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()

					started := make(chan time.Time, 1)
					returned := make(chan error, 1)
					lease, maxRuntime := 200*time.Millisecond, time.Millisecond
					if heartbeat {
						lease, maxRuntime = 3*time.Second, 200*time.Millisecond
					}
					reg := jobs.NewRegistry()
					reg.Register("fetch_body", jobs.Entry{
						PoolSize:   1,
						Lease:      lease,
						Heartbeat:  heartbeat,
						MaxRuntime: maxRuntime,
						Handler: func(ctx context.Context, payload []byte) error {
							started <- time.Now()
							<-ctx.Done()
							returned <- ctx.Err()
							return ctx.Err()
						},
					})
					d := jobs.NewTypeDispatcher(jobs.DispatcherConfig{
						Type:         "fetch_body",
						Registry:     reg,
						DB:           pool,
						WorkerID:     "w-runtime-test",
						Runner:       "vps",
						IdleInterval: 10 * time.Millisecond,
						BackoffBase:  time.Minute,
						BackoffCap:   time.Minute,
						Logger:       zerolog.Nop(),
					})
					id := mustEnqueueWithMaxAttempts(t, pool, "fetch_body", []byte(`{}`), 0, tc.maxAttempts)
					dispatcherDone := make(chan struct{})
					go func() {
						defer close(dispatcherDone)
						d.Run(ctx)
					}()
					t.Cleanup(func() {
						cancel()
						select {
						case <-dispatcherDone:
						case <-time.After(time.Second):
							t.Error("dispatcher did not stop")
						}
					})

					var began time.Time
					select {
					case began = <-started:
					case <-time.After(time.Second):
						t.Fatal("handler did not start")
					}
					deadline := began.Add(time.Second)
					select {
					case err := <-returned:
						if !errors.Is(err, context.DeadlineExceeded) {
							t.Fatalf("handler returned %v, want deadline exceeded", err)
						}
					case <-time.After(time.Until(deadline)):
						t.Fatal("blocking handler exceeded one-second timeout ceiling")
					}
					if elapsed := time.Since(began); elapsed < 100*time.Millisecond {
						t.Fatalf("handler timed out after %v, want the 200ms runtime rather than MaxRuntime for non-heartbeat jobs", elapsed)
					}
					waitForStatus(t, pool, id, tc.wantStatus, time.Until(deadline))
					if elapsed := time.Since(began); elapsed >= time.Second {
						t.Fatalf("timeout state transition took %v, want less than one second", elapsed)
					}

					var lastError string
					var attempts int16
					var lockedBy *string
					var leaseUntil *time.Time
					must(t, pool.QueryRow(ctx, `SELECT last_error, attempts, locked_by, lease_until FROM graph.jobs WHERE id=$1`, id).
						Scan(&lastError, &attempts, &lockedBy, &leaseUntil))
					if lastError != context.DeadlineExceeded.Error() || attempts != 1 || lockedBy != nil || leaseUntil != nil {
						t.Fatalf("timeout state: error=%q attempts=%d locked_by=%v lease_until=%v", lastError, attempts, lockedBy, leaseUntil)
					}
				})
			}
		})
	}
}

func TestRunOne_HeartbeatWithoutMaxRuntimeKeepsParentContext(t *testing.T) {
	pool := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan bool, 1)
	returned := make(chan error, 1)
	reg := jobs.NewRegistry()
	reg.Register("fetch_body", jobs.Entry{
		PoolSize:  1,
		Lease:     time.Second,
		Heartbeat: true,
		Handler: func(ctx context.Context, payload []byte) error {
			_, hasDeadline := ctx.Deadline()
			started <- hasDeadline
			<-ctx.Done()
			returned <- ctx.Err()
			return ctx.Err()
		},
	})
	d := jobs.NewTypeDispatcher(jobs.DispatcherConfig{
		Type:     "fetch_body",
		Registry: reg,
		DB:       pool,
		WorkerID: "w-runtime-test",
		Runner:   "vps",
		Logger:   zerolog.Nop(),
	})
	mustEnqueue(t, pool, "fetch_body", []byte(`{}`), 0)
	dispatcherDone := make(chan struct{})
	go func() {
		defer close(dispatcherDone)
		d.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-dispatcherDone:
		case <-time.After(time.Second):
			t.Error("dispatcher did not stop")
		}
	})
	select {
	case hasDeadline := <-started:
		if hasDeadline {
			t.Fatal("zero MaxRuntime added a handler deadline")
		}
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	cancel()
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("handler returned %v, want parent cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("handler ignored parent cancellation")
	}
}
