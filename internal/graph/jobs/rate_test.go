package jobs_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

func TestAcquireMany_SortedOrder(t *testing.T) {
	sems := jobs.NewSemaphores(jobs.DefaultRate())
	ctx := context.Background()

	// Acquire ["gemini","jira"] in one goroutine and ["jira","gemini"] in another.
	// If locking is sorted, there should be no deadlock.
	var wg sync.WaitGroup
	errs := make(chan error, 2)

	for _, order := range [][]string{{"gemini", "jira"}, {"jira", "gemini"}} {
		order := order
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := sems.AcquireMany(ctx, order)
			if err != nil {
				errs <- err
				return
			}
			// Hold briefly to interleave.
			time.Sleep(5 * time.Millisecond)
			release()
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// Pass.
	case <-time.After(5 * time.Second):
		t.Fatal("AcquireMany deadlock detected")
	}
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("AcquireMany error: %v", err)
		}
	}
}

func TestAcquire_Unknown(t *testing.T) {
	sems := jobs.NewSemaphores(jobs.DefaultRate())
	ctx := context.Background()

	release, err := sems.Acquire(ctx, "unknown_system")
	if err != nil {
		t.Fatalf("Acquire unknown: unexpected error: %v", err)
	}
	if release == nil {
		t.Fatal("release func should not be nil")
	}
	// Must not panic.
	release()
}

func TestIsRetryable_StatusTable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"http 200", jobs.NewHTTPError(200, "ok"), false},
		{"http 400", jobs.NewHTTPError(400, "bad request"), false},
		{"http 401", jobs.NewHTTPError(401, "unauthorized"), false},
		{"http 403", jobs.NewHTTPError(403, "forbidden"), false},
		{"http 404", jobs.NewHTTPError(404, "not found"), false},
		{"http 429", jobs.NewHTTPError(429, "rate limited"), true},
		{"http 500", jobs.NewHTTPError(500, "server error"), true},
		{"http 503", jobs.NewHTTPError(503, "unavailable"), true},
		{"ErrTransient", jobs.ErrTransient, true},
		{"ErrFatal", jobs.ErrFatal, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := jobs.IsRetryable(tc.err)
			if got != tc.want {
				t.Errorf("IsRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
