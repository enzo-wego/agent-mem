package jobs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

func TestComplete_MarksDone(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	id := mustEnqueue(t, pool, "fetch_body", []byte(`{}`), 0)
	_, err := jobs.Claim(ctx, pool, "fetch_body", 60*time.Second, "w-1", "vps")
	must(t, err)

	if err := jobs.Complete(ctx, pool, id); err != nil {
		t.Fatal(err)
	}
	row := pool.QueryRow(ctx, `SELECT status, completed_at FROM graph.jobs WHERE id=$1`, id)
	var status string
	var completed *time.Time
	if err := row.Scan(&status, &completed); err != nil {
		t.Fatal(err)
	}
	if status != "done" || completed == nil {
		t.Errorf("got status=%q completed_at=%v", status, completed)
	}
}

func TestRetry_ResetsAndDelays(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	id := mustEnqueue(t, pool, "fetch_body", []byte(`{}`), 0)
	_, err := jobs.Claim(ctx, pool, "fetch_body", 60*time.Second, "w-1", "vps")
	must(t, err)

	if err := jobs.Retry(ctx, pool, id, errors.New("transient"), 90*time.Second); err != nil {
		t.Fatal(err)
	}
	var status, lastErr string
	var availableAt time.Time
	row := pool.QueryRow(ctx,
		`SELECT status, last_error, available_at FROM graph.jobs WHERE id=$1`, id)
	if err := row.Scan(&status, &lastErr, &availableAt); err != nil {
		t.Fatal(err)
	}
	if status != "queued" {
		t.Errorf("status: got %q want queued", status)
	}
	if lastErr != "transient" {
		t.Errorf("last_error: got %q want %q", lastErr, "transient")
	}
	want := time.Now().Add(90 * time.Second)
	if d := availableAt.Sub(want); d < -5*time.Second || d > 5*time.Second {
		t.Errorf("available_at off by %v from now+90s", d)
	}
}

func TestFail_MarksFailed(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	id := mustEnqueue(t, pool, "fetch_body", []byte(`{}`), 0)
	_, err := jobs.Claim(ctx, pool, "fetch_body", 60*time.Second, "w-1", "vps")
	must(t, err)

	if err := jobs.Fail(ctx, pool, id, errors.New("4xx body")); err != nil {
		t.Fatal(err)
	}
	row := pool.QueryRow(ctx, `SELECT status, last_error FROM graph.jobs WHERE id=$1`, id)
	var status, lastErr string
	if err := row.Scan(&status, &lastErr); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || lastErr != "4xx body" {
		t.Errorf("got status=%q last_error=%q", status, lastErr)
	}
}
