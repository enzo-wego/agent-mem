package handlers

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// seedJiraNodes inserts PAY-<n> nodes with the given ids; first_seen_at is
// derived from the position in order (earlier in the slice = older), which the
// caller makes differ from id order where it matters.
func seedJiraNodes(t *testing.T, pool *pgxpool.Pool, nums []int) []string {
	t.Helper()
	var ids []string
	for i, n := range nums {
		id := fmt.Sprintf("jira:PAY-%03d", n)
		if _, err := pool.Exec(context.Background(), `INSERT INTO graph.nodes (id, type, natural_key, metadata, machine_id, first_seen_at)
			VALUES ($1, 'jira', $2, '{}', 'test', now() - interval '1 day' * $3)`, id, fmt.Sprintf("PAY-%03d", n), len(nums)-i); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
		ids = append(ids, id)
	}
	return ids
}

func insertFetchBodyJob(t *testing.T, pool *pgxpool.Pool, nodeID, status string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `INSERT INTO graph.jobs (type, payload, priority, max_attempts, target_runner, machine_id, status)
		VALUES ('fetch_body', jsonb_build_object('node_id', $1::text), 5, 5, 'any', 'test', $2) RETURNING id`, nodeID, status).Scan(&id); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	return id
}

func markFetchBodyJobsDone(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE graph.jobs SET status='done' WHERE type='fetch_body' AND status='queued'`); err != nil {
		t.Fatal(err)
	}
}

func TestBackfillJiraMetadata_SkipsLatestFailed(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	ctx := context.Background()
	seedJiraNodes(t, pool, []int{1})

	jobID := insertFetchBodyJob(t, pool, "jira:PAY-001", "running")
	if _, err := pool.Exec(ctx, `UPDATE graph.jobs SET enqueued_at = now() - interval '30 day' WHERE id = $1`, jobID); err != nil {
		t.Fatal(err)
	}
	if err := jobs.Fail(ctx, pool, jobID, errors.New("jira_http_404")); err != nil {
		t.Fatal(err)
	}

	p := jiraBackfillParams{Limit: 10}
	if r := BackfillJiraMetadata(ctx, pool, zerolog.Nop(), "test", p); r.Matched != 0 || r.Remaining != 0 {
		t.Fatalf("failed node: matched=%d remaining=%d, want 0/0", r.Matched, r.Remaining)
	}
	p.Force = true
	if r := BackfillJiraMetadata(ctx, pool, zerolog.Nop(), "test", p); r.Matched != 0 {
		t.Fatalf("force must not re-pick a failed node: matched=%d", r.Matched)
	}

	insertFetchBodyJob(t, pool, "jira:PAY-001", "done")
	if r := BackfillJiraMetadata(ctx, pool, zerolog.Nop(), "test", jiraBackfillParams{Limit: 10}); r.Matched != 1 {
		t.Fatalf("after newer done job: matched=%d, want 1", r.Matched)
	}
}

func TestBackfillJiraMetadata_ForceCursor(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	ctx := context.Background()
	// first_seen order (slice order) differs from id order.
	ids := seedJiraNodes(t, pool, []int{5, 2, 7, 1, 6, 3, 4})

	seen := map[string]int{}
	after := ""
	pages := 0
	for {
		r := BackfillJiraMetadata(ctx, pool, zerolog.Nop(), "test", jiraBackfillParams{Limit: 3, Force: true, AfterID: after})
		pages++
		rows, err := pool.Query(ctx, `SELECT payload->>'node_id' FROM graph.jobs WHERE type='fetch_body' AND status='queued'`)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			seen[id]++
		}
		rows.Close()
		markFetchBodyJobsDone(t, pool)
		if r.NextAfterID == "" {
			break
		}
		after = r.NextAfterID
		if pages > 10 {
			t.Fatal("cursor did not terminate")
		}
	}
	if pages != 3 {
		t.Errorf("pages = %d, want 3", pages)
	}
	if len(seen) != len(ids) {
		t.Errorf("seen %d nodes, want %d: %v", len(seen), len(ids), seen)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("%s enqueued %d times", id, n)
		}
	}
}

func TestBackfillJiraMetadata_ForceCursorEnqueueError(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	ctx := context.Background()
	ids := seedJiraNodes(t, pool, []int{1, 2, 3, 4, 5})

	orig := enqueueFetchBodyForBackfill
	t.Cleanup(func() { enqueueFetchBodyForBackfill = orig })
	enqueueFetchBodyForBackfill = func(ctx context.Context, db jobs.DB, nodeID, machineID string, at time.Time) error {
		if nodeID == ids[2] {
			return errors.New("boom")
		}
		return orig(ctx, db, nodeID, machineID, at)
	}
	r := BackfillJiraMetadata(ctx, pool, zerolog.Nop(), "test", jiraBackfillParams{Limit: 5, Force: true})
	if r.NextAfterID != ids[1] || r.EnqueueErrors != 1 || r.Enqueued != 2 {
		t.Fatalf("got next=%q errors=%d enqueued=%d, want next=%q errors=1 enqueued=2", r.NextAfterID, r.EnqueueErrors, r.Enqueued, ids[1])
	}

	enqueueFetchBodyForBackfill = orig
	markFetchBodyJobsDone(t, pool)
	r = BackfillJiraMetadata(ctx, pool, zerolog.Nop(), "test", jiraBackfillParams{Limit: 5, Force: true, AfterID: r.NextAfterID})
	if r.Enqueued != 3 || r.EnqueueErrors != 0 {
		t.Fatalf("resume enqueued=%d errors=%d, want 3/0", r.Enqueued, r.EnqueueErrors)
	}
	var got []string
	rows, err := pool.Query(ctx, `SELECT payload->>'node_id' FROM graph.jobs WHERE type='fetch_body' AND status='queued' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	if fmt.Sprint(got) != fmt.Sprint(ids[2:]) {
		t.Errorf("resumed ids = %v, want %v", got, ids[2:])
	}
}

func TestBackfillJiraMetadata_AfterIDExcludes(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	ctx := context.Background()
	ids := seedJiraNodes(t, pool, []int{1, 2, 3, 4, 5, 6, 7})

	r := BackfillJiraMetadata(ctx, pool, zerolog.Nop(), "test", jiraBackfillParams{Limit: 10, Force: true, AfterID: ids[3]})
	if r.Enqueued != 3 || r.NextAfterID != "" {
		t.Fatalf("enqueued=%d next=%q, want 3 and empty", r.Enqueued, r.NextAfterID)
	}
	var below int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph.jobs WHERE type='fetch_body' AND payload->>'node_id' <= $1`, ids[3]).Scan(&below); err != nil || below != 0 {
		t.Fatalf("jobs for ids <= after_id: %d (%v)", below, err)
	}
}

func TestBackfillJiraMetadata_RetryFailed(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	ctx := context.Background()
	seedJiraNodes(t, pool, []int{1})
	failedID := insertFetchBodyJob(t, pool, "jira:PAY-001", "running")
	if err := jobs.Fail(ctx, pool, failedID, errors.New("boom")); err != nil {
		t.Fatal(err)
	}

	if r := BackfillJiraMetadata(ctx, pool, zerolog.Nop(), "test", jiraBackfillParams{Limit: 10}); r.Matched != 0 {
		t.Fatalf("normal call matched=%d, want 0", r.Matched)
	}
	r := BackfillJiraMetadata(ctx, pool, zerolog.Nop(), "test", jiraBackfillParams{Limit: 10, RetryFailed: true})
	if r.Enqueued != 1 {
		t.Fatalf("retry_failed enqueued=%d, want 1", r.Enqueued)
	}
	var skip bool
	if err := pool.QueryRow(ctx, `SELECT (payload->>'skip_attachments')::bool FROM graph.jobs WHERE type='fetch_body' AND status='queued'`).Scan(&skip); err != nil || !skip {
		t.Fatalf("skip_attachments = %v (%v), want true", skip, err)
	}
	markFetchBodyJobsDone(t, pool)
	if r := BackfillJiraMetadata(ctx, pool, zerolog.Nop(), "test", jiraBackfillParams{Limit: 10}); r.Matched != 1 {
		t.Fatalf("after done job a normal call matched=%d, want 1 (no longer excluded as failed)", r.Matched)
	}
}
