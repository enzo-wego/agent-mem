package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testDB is an alias for openTestDB for use in plan-derived tests.
func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := openTestDB(t)
	truncateJobsTable(t, pool)
	return pool
}

// mustEnqueue enqueues a job and returns its id, failing the test on error.
func mustEnqueue(t *testing.T, pool *pgxpool.Pool, jobType string, payload []byte, priority int16) int64 {
	t.Helper()
	opts := jobs.EnqueueOptions{
		MachineID: "m-test",
		Priority:  priority,
	}
	id, err := jobs.EnqueueRaw(context.Background(), pool, jobType, payload, opts)
	if err != nil {
		t.Fatalf("mustEnqueue %s: %v", jobType, err)
	}
	return id
}

// mustEnqueueWithMaxAttempts enqueues a job with a specific max_attempts.
func mustEnqueueWithMaxAttempts(t *testing.T, pool *pgxpool.Pool, jobType string, payload []byte, priority int16, maxAttempts int16) int64 {
	t.Helper()
	opts := jobs.EnqueueOptions{
		MachineID:   "m-test",
		Priority:    priority,
		MaxAttempts: maxAttempts,
	}
	id, err := jobs.EnqueueRaw(context.Background(), pool, jobType, payload, opts)
	if err != nil {
		t.Fatalf("mustEnqueueWithMaxAttempts %s: %v", jobType, err)
	}
	return id
}

// must fails the test if err != nil.
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("must: %v", err)
	}
}

// waitForStatus polls until graph.jobs.status == want or deadline expires.
func waitForStatus(t *testing.T, pool *pgxpool.Pool, id int64, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var status string
		err := pool.QueryRow(context.Background(),
			`SELECT status FROM graph.jobs WHERE id=$1`, id).Scan(&status)
		if err == nil && status == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	var status string
	pool.QueryRow(context.Background(), `SELECT status FROM graph.jobs WHERE id=$1`, id).Scan(&status)
	t.Fatalf("waitForStatus: job %d: got %q, want %q after %v", id, status, want, timeout)
}

// waitForDone waits for a job to reach status='done'.
func waitForDone(t *testing.T, pool *pgxpool.Pool, id int64, timeout time.Duration) {
	t.Helper()
	waitForStatus(t, pool, id, "done", timeout)
}
