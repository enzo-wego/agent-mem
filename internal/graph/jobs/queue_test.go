package jobs_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// openTestDB connects to the Postgres instance identified by DATABASE_URL.
// If DATABASE_URL is not set the test is skipped.
func openTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("DB ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func truncateJobsTable(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "DELETE FROM graph.jobs"); err != nil {
		t.Fatalf("truncate graph.jobs: %v", err)
	}
}

func defaultOpts(machineID string) jobs.EnqueueOptions {
	return jobs.EnqueueOptions{MachineID: machineID}
}

func TestEnqueue_AssignsDefaults(t *testing.T) {
	pool := openTestDB(t)
	truncateJobsTable(t, pool)
	ctx := context.Background()

	id, err := jobs.EnqueueRaw(ctx, pool, "test_type", []byte(`{}`), defaultOpts("machine1"))
	if err != nil {
		t.Fatalf("EnqueueRaw: %v", err)
	}

	var priority, maxAttempts int16
	err = pool.QueryRow(ctx,
		`SELECT priority, max_attempts FROM graph.jobs WHERE id = $1`, id,
	).Scan(&priority, &maxAttempts)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if priority != 5 {
		t.Errorf("priority = %d, want 5", priority)
	}
	if maxAttempts != 5 {
		t.Errorf("max_attempts = %d, want 5", maxAttempts)
	}
}

func TestClaim_RespectsAvailableAt(t *testing.T) {
	pool := openTestDB(t)
	truncateJobsTable(t, pool)
	ctx := context.Background()

	opts := defaultOpts("machine1")
	opts.AvailableAt = time.Now().Add(time.Minute)
	_, err := jobs.EnqueueRaw(ctx, pool, "future_type", []byte(`{}`), opts)
	if err != nil {
		t.Fatalf("EnqueueRaw: %v", err)
	}

	job, err := jobs.Claim(ctx, pool, "future_type", "worker1", "any")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if job != nil {
		t.Errorf("expected nil job (not yet available), got job id=%d", job.ID)
	}
}

func TestClaim_SkipLocked(t *testing.T) {
	pool := openTestDB(t)
	truncateJobsTable(t, pool)
	ctx := context.Background()

	_, err := jobs.EnqueueRaw(ctx, pool, "skip_type", []byte(`{}`), defaultOpts("machine1"))
	if err != nil {
		t.Fatalf("EnqueueRaw: %v", err)
	}

	// Hold a transaction that locks the row.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	job1, err := jobs.Claim(ctx, tx, "skip_type", "worker1", "any")
	if err != nil {
		t.Fatalf("Claim tx: %v", err)
	}
	if job1 == nil {
		t.Fatal("expected job from tx, got nil")
	}

	// Second claim (outside the tx) should find nothing locked.
	job2, err := jobs.Claim(ctx, pool, "skip_type", "worker2", "any")
	if err != nil {
		t.Fatalf("Claim pool: %v", err)
	}
	if job2 != nil {
		t.Errorf("expected nil (skip locked), got job id=%d", job2.ID)
	}
}

func TestClaim_TargetRunner(t *testing.T) {
	pool := openTestDB(t)
	truncateJobsTable(t, pool)
	ctx := context.Background()

	// Enqueue a vps-only job.
	opts := defaultOpts("machine1")
	opts.TargetRunner = "vps"
	_, err := jobs.EnqueueRaw(ctx, pool, "runner_type", []byte(`{}`), opts)
	if err != nil {
		t.Fatalf("EnqueueRaw vps: %v", err)
	}

	// local runner must not claim it.
	job, err := jobs.Claim(ctx, pool, "runner_type", "worker1", "local")
	if err != nil {
		t.Fatalf("Claim local: %v", err)
	}
	if job != nil {
		t.Errorf("local runner should not claim vps job, got id=%d", job.ID)
	}

	// vps runner claims it.
	job, err = jobs.Claim(ctx, pool, "runner_type", "worker1", "vps")
	if err != nil {
		t.Fatalf("Claim vps: %v", err)
	}
	if job == nil {
		t.Fatal("vps runner should have claimed the job")
	}
	if err := jobs.Complete(ctx, pool, job.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Enqueue an "any" job and verify "any" runner can claim it.
	opts2 := defaultOpts("machine1")
	opts2.TargetRunner = "any"
	_, err = jobs.EnqueueRaw(ctx, pool, "runner_type", []byte(`{}`), opts2)
	if err != nil {
		t.Fatalf("EnqueueRaw any: %v", err)
	}
	job2, err := jobs.Claim(ctx, pool, "runner_type", "worker1", "any")
	if err != nil {
		t.Fatalf("Claim any: %v", err)
	}
	if job2 == nil {
		t.Fatal("any runner should have claimed the any job")
	}
	if err := jobs.Complete(ctx, pool, job2.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

func TestComplete_SetsCompletedAt(t *testing.T) {
	pool := openTestDB(t)
	truncateJobsTable(t, pool)
	ctx := context.Background()

	id, err := jobs.EnqueueRaw(ctx, pool, "complete_type", []byte(`{}`), defaultOpts("machine1"))
	if err != nil {
		t.Fatalf("EnqueueRaw: %v", err)
	}
	job, err := jobs.Claim(ctx, pool, "complete_type", "worker1", "any")
	if err != nil || job == nil {
		t.Fatalf("Claim: %v / nil=%v", err, job == nil)
	}
	if err := jobs.Complete(ctx, pool, id); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	var status string
	var completedAt *time.Time
	err = pool.QueryRow(ctx,
		`SELECT status, completed_at FROM graph.jobs WHERE id = $1`, id,
	).Scan(&status, &completedAt)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if status != "done" {
		t.Errorf("status = %q, want done", status)
	}
	if completedAt == nil {
		t.Error("completed_at is NULL, want a timestamp")
	}
}

func TestRetry_PushesAvailableAt(t *testing.T) {
	pool := openTestDB(t)
	truncateJobsTable(t, pool)
	ctx := context.Background()

	id, err := jobs.EnqueueRaw(ctx, pool, "retry_type", []byte(`{}`), defaultOpts("machine1"))
	if err != nil {
		t.Fatalf("EnqueueRaw: %v", err)
	}
	job, err := jobs.Claim(ctx, pool, "retry_type", "worker1", "any")
	if err != nil || job == nil {
		t.Fatalf("Claim: err=%v nil=%v", err, job == nil)
	}

	delay := 30 * time.Second
	retryErr := jobs.ErrTransient
	if err := jobs.Retry(ctx, pool, id, retryErr, delay); err != nil {
		t.Fatalf("Retry: %v", err)
	}

	var status, lastError string
	var availableAt time.Time
	err = pool.QueryRow(ctx,
		`SELECT status, last_error, available_at FROM graph.jobs WHERE id = $1`, id,
	).Scan(&status, &lastError, &availableAt)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if status != "queued" {
		t.Errorf("status = %q, want queued", status)
	}
	if lastError == "" {
		t.Error("last_error should be set after retry")
	}
	minExpected := time.Now().Add(25 * time.Second)
	if availableAt.Before(minExpected) {
		t.Errorf("available_at=%v too early, want >= ~NOW()+30s", availableAt)
	}
}

func TestFail_FinalState(t *testing.T) {
	pool := openTestDB(t)
	truncateJobsTable(t, pool)
	ctx := context.Background()

	id, err := jobs.EnqueueRaw(ctx, pool, "fail_type", []byte(`{}`), defaultOpts("machine1"))
	if err != nil {
		t.Fatalf("EnqueueRaw: %v", err)
	}
	job, err := jobs.Claim(ctx, pool, "fail_type", "worker1", "any")
	if err != nil || job == nil {
		t.Fatalf("Claim: err=%v nil=%v", err, job == nil)
	}
	if err := jobs.Fail(ctx, pool, id, jobs.NewHTTPError(404, "not found")); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	var status, lastError string
	err = pool.QueryRow(ctx,
		`SELECT status, last_error FROM graph.jobs WHERE id = $1`, id,
	).Scan(&status, &lastError)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if status != "failed" {
		t.Errorf("status = %q, want failed", status)
	}
	if lastError == "" {
		t.Error("last_error should be set after fail")
	}
}

func TestQueueDepth(t *testing.T) {
	pool := openTestDB(t)
	truncateJobsTable(t, pool)
	ctx := context.Background()

	// Enqueue 3 jobs; claim 1 so we have queued=2, running=1.
	for i := 0; i < 3; i++ {
		if _, err := jobs.EnqueueRaw(ctx, pool, "depth_type", []byte(`{}`), defaultOpts("machine1")); err != nil {
			t.Fatalf("EnqueueRaw: %v", err)
		}
	}
	job, err := jobs.Claim(ctx, pool, "depth_type", "worker1", "any")
	if err != nil || job == nil {
		t.Fatalf("Claim: err=%v nil=%v", err, job == nil)
	}

	depths, err := jobs.QueueDepth(ctx, pool, "depth_type")
	if err != nil {
		t.Fatalf("QueueDepth: %v", err)
	}
	if depths["queued"] != 2 {
		t.Errorf("queued = %d, want 2", depths["queued"])
	}
	if depths["running"] != 1 {
		t.Errorf("running = %d, want 1", depths["running"])
	}
}
