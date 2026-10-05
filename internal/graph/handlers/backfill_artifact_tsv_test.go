package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

const tsvBaseVersion = int64(20261003154813) // last main migration before R2

// seedTSVRows inserts n nodes plus artifact_index rows with tsv forced to NULL
// (the state of rows that predate the trigger).
func seedTSVRows(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO graph.nodes (id, type, natural_key, metadata, machine_id)
		SELECT 'tsv:' || i, 'jira', 'tsv:' || i, '{}', 'test' FROM generate_series(1, $1) i`, n); err != nil {
		t.Fatalf("seed nodes: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO graph.artifact_index (node_id, summary, decisions_text, identifiers, summary_kind, machine_id)
		SELECT 'tsv:' || i, 'summary ' || i, 'decision ' || i, ARRAY['PAY-' || i], 'heuristic', 'test'
		FROM generate_series(1, $1) i`, n); err != nil {
		t.Fatalf("seed index: %v", err)
	}
	// UPDATE OF summary/decisions_text/identifiers fires the trigger; touching
	// only tsv does not.
	if _, err := pool.Exec(ctx, `UPDATE graph.artifact_index SET tsv = NULL`); err != nil {
		t.Fatalf("null tsv: %v", err)
	}
}

func nullTSV(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM graph.artifact_index WHERE tsv IS NULL`).Scan(&n); err != nil {
		t.Fatalf("count null: %v", err)
	}
	return n
}

// runTSVQueue claims and runs jobs of the type until none are due, making
// parked continuations due first. Returns the number of runs.
func runTSVQueue(t *testing.T, pool *pgxpool.Pool, h jobs.Handler) int {
	t.Helper()
	ctx := context.Background()
	runs := 0
	for {
		if _, err := pool.Exec(ctx, `UPDATE graph.jobs SET available_at = now() WHERE type='backfill_artifact_tsv' AND status='queued'`); err != nil {
			t.Fatal(err)
		}
		j, err := jobs.Claim(ctx, pool, "backfill_artifact_tsv", time.Minute, "test-worker", "any")
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if j == nil {
			return runs
		}
		if err := h(ctx, j.Payload); err != nil {
			t.Fatalf("handler job %d: %v", j.ID, err)
		}
		if err := jobs.Complete(ctx, pool, j.ID); err != nil {
			t.Fatal(err)
		}
		runs++
		if runs > 100 {
			t.Fatal("drain did not stop")
		}
	}
}

func enqueueTSV(t *testing.T, pool *pgxpool.Pool, batch int) {
	t.Helper()
	if _, err := jobs.Enqueue(context.Background(), pool, "backfill_artifact_tsv", backfillArtifactTSVPayload{Batch: batch},
		jobs.EnqueueOptions{MachineID: "test"}); err != nil {
		t.Fatal(err)
	}
}

func tsvTestDeps(pool *pgxpool.Pool) Deps {
	return Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "test"}
}

func assertTSVMatchesHelper(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var bad int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM graph.artifact_index
		WHERE tsv IS DISTINCT FROM graph.artifact_index_tsv_doc(summary, decisions_text, identifiers)`).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Fatalf("%d rows differ from helper output", bad)
	}
}

func TestBackfillArtifactTSV_Drains(t *testing.T) {
	pool := openTestDB(t)
	ctx := context.Background()
	h := NewBackfillArtifactTSVHandler(tsvTestDeps(pool)).Handler
	for _, tc := range []struct{ rows, runs int }{{1234, 3}, {1000, 3}} {
		t.Run(fmt.Sprint(tc.rows), func(t *testing.T) {
			truncateGraphHandlerTables(t, pool)
			seedTSVRows(t, pool, tc.rows)
			enqueueTSV(t, pool, 500)
			if runs := runTSVQueue(t, pool, h); runs != tc.runs {
				t.Fatalf("runs = %d, want %d", runs, tc.runs)
			}
			if n := nullTSV(t, pool); n != 0 {
				t.Fatalf("%d NULL rows left", n)
			}
			assertTSVMatchesHelper(t, pool)
		})
	}
	_ = ctx
}

func TestBackfillArtifactTSV_BadPayload(t *testing.T) {
	pool := openTestDB(t)
	h := NewBackfillArtifactTSVHandler(tsvTestDeps(pool)).Handler
	for _, p := range []string{`{"batch":0}`, `{"batch":-1}`, `{"batch":5001}`, `{"batch":"x"}`} {
		if err := h(context.Background(), []byte(p)); !errors.Is(err, jobs.ErrFatal) {
			t.Errorf("payload %s: err = %v, want ErrFatal", p, err)
		}
	}
}

func TestBackfillArtifactTSV_EnqueueFailureRetries(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	ctx := context.Background()
	seedTSVRows(t, pool, 1100)
	enqueueTSV(t, pool, 500)

	failing := backfillArtifactTSVHandler(tsvTestDeps(pool), func(context.Context, []byte) error {
		return errors.New("injected enqueue failure")
	})
	j, err := jobs.Claim(ctx, pool, "backfill_artifact_tsv", time.Minute, "w", "any")
	if err != nil || j == nil {
		t.Fatalf("claim: %v %v", j, err)
	}
	herr := failing(ctx, j.Payload)
	if herr == nil {
		t.Fatal("handler succeeded despite enqueue failure")
	}
	if !jobs.IsRetryable(herr) {
		t.Fatalf("enqueue failure must be retryable, got %v", herr)
	}
	if err := jobs.Retry(ctx, pool, j.ID, herr, time.Second); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM graph.jobs WHERE id=$1`, j.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "queued" {
		t.Fatalf("job status %q after failure, want queued for retry", status)
	}
	if n := nullTSV(t, pool); n != 600 {
		t.Fatalf("after failed run: %d NULL, want 600 (batch committed)", n)
	}
	// Retry (the failed job is re-queued with backoff) continues the drain.
	if _, err := pool.Exec(ctx, `UPDATE graph.jobs SET available_at = now() WHERE id=$1`, j.ID); err != nil {
		t.Fatal(err)
	}
	runTSVQueue(t, pool, NewBackfillArtifactTSVHandler(tsvTestDeps(pool)).Handler)
	if n := nullTSV(t, pool); n != 0 {
		t.Fatalf("%d NULL rows left after retry", n)
	}
}

func TestBackfillArtifactTSV_TwoChains(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	ctx := context.Background()
	seedTSVRows(t, pool, 1250)
	enqueueTSV(t, pool, 500)
	enqueueTSV(t, pool, 500)
	h := NewBackfillArtifactTSVHandler(tsvTestDeps(pool)).Handler

	a, err := jobs.Claim(ctx, pool, "backfill_artifact_tsv", time.Minute, "w1", "any")
	if err != nil || a == nil {
		t.Fatalf("claim a: %v %v", a, err)
	}
	b, err := jobs.Claim(ctx, pool, "backfill_artifact_tsv", time.Minute, "w2", "any")
	if err != nil || b == nil {
		t.Fatalf("claim b: %v %v", b, err)
	}
	for _, j := range []*jobs.Job{a, b} { // interleaved: a, b, then drain
		if err := h(ctx, j.Payload); err != nil {
			t.Fatal(err)
		}
	}
	for _, j := range []*jobs.Job{b, a} {
		if err := jobs.Complete(ctx, pool, j.ID); err != nil {
			t.Fatal(err)
		}
	}
	runTSVQueue(t, pool, h)
	if n := nullTSV(t, pool); n != 0 {
		t.Fatalf("%d NULL rows left", n)
	}
	var open int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph.jobs WHERE type='backfill_artifact_tsv' AND status IN ('queued','running')`).Scan(&open); err != nil || open != 0 {
		t.Fatalf("open jobs = %d (%v), want 0", open, err)
	}
}

func TestBackfillArtifactTSV_NoBlockingTableLock(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	ctx := context.Background()
	seedTSVRows(t, pool, 20)
	if _, err := pool.Exec(ctx, `INSERT INTO graph.nodes (id, type, natural_key, metadata, machine_id) VALUES ('other:1','jira','other:1','{}','test')`); err != nil {
		t.Fatal(err)
	}

	a, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Rollback(ctx)
	if n, err := backfillArtifactTSVBatch(ctx, a, 10); err != nil || n != 10 {
		t.Fatalf("batch in tx: n=%d err=%v", n, err)
	}

	// The UPDATE holds ROW EXCLUSIVE, which is compatible with reads and
	// inserts, plus row locks on the 10 updated rows only.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET lock_timeout = '1s'`); err != nil {
		t.Fatal(err)
	}
	var cnt int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM graph.artifact_index`).Scan(&cnt); err != nil || cnt != 20 {
		t.Fatalf("select while A open: cnt=%d err=%v", cnt, err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO graph.artifact_index (node_id, summary, summary_kind, machine_id) VALUES ('other:1','x','heuristic','test')`); err != nil {
		t.Fatalf("insert while A open: %v", err)
	}
	if err := a.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactTSV_PopulatedUpgrade(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	ctx := context.Background()
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// Match the ApplyVersion fixtures: older migrations may have been reapplied
	// after newer ones, so legacy insertion-order version lookup is not valid.
	provider, err := goose.NewProvider(goose.DialectPostgres, db, os.DirFS(migrationsDirFromHandlers), goose.WithAllowOutofOrder(true))
	if err != nil {
		t.Fatal(err)
	}
	// Leave the scratch DB fully migrated even if an assertion fails midway.
	t.Cleanup(func() {
		if _, err := provider.Up(ctx); err != nil {
			t.Errorf("restore migrations: %v", err)
		}
	})
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	downToTSVBase(t, ctx, provider)
	if v, err := provider.GetDBVersion(ctx); err != nil || v != tsvBaseVersion {
		t.Fatalf("db version after DownTo = %d (%v), want %d", v, err, tsvBaseVersion)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO graph.nodes (id, type, natural_key, metadata, machine_id)
		SELECT 'up:' || i, 'jira', 'up:' || i, '{}', 'test' FROM generate_series(1, 50) i`); err != nil {
		t.Fatalf("seed nodes: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO graph.artifact_index (node_id, summary, decisions_text, identifiers, summary_kind, machine_id)
		SELECT 'up:' || i, 'plain summary ' || i,
		       CASE WHEN i = 7 THEN 'zebracorn rollout decided' END, '{}', 'heuristic', 'test'
		FROM generate_series(1, 50) i`); err != nil {
		t.Fatalf("seed index: %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("up to head: %v", err)
	}
	if n := nullTSV(t, pool); n != 50 {
		t.Fatalf("after migrate: %d NULL, want 50", n)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO graph.nodes (id, type, natural_key, metadata, machine_id) VALUES ('up:new','jira','up:new','{}','test')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO graph.artifact_index (node_id, summary, summary_kind, machine_id) VALUES ('up:new','fresh','heuristic','test')`); err != nil {
		t.Fatal(err)
	}
	if n := nullTSV(t, pool); n != 50 {
		t.Fatalf("new insert should have tsv: %d NULL, want 50", n)
	}

	deps := tsvTestDeps(pool)
	rec := postEnqueue(t, deps, `{"type":"backfill_artifact_tsv","payload":{"batch":20}}`)
	if rec != 200 {
		t.Fatalf("admin enqueue status %d", rec)
	}
	runTSVQueue(t, pool, NewBackfillArtifactTSVHandler(deps).Handler)
	if n := nullTSV(t, pool); n != 0 {
		t.Fatalf("%d NULL left", n)
	}
	hits, err := keywordArm(ctx, pool, "zebracorn", searchFilter{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != "up:7" {
		t.Fatalf("keywordArm hits = %+v, want [up:7]", hits)
	}
}

func postEnqueue(t *testing.T, deps Deps, body string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	NewJobsEnqueueHandler(deps).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/graph/jobs/enqueue", strings.NewReader(body)))
	return rec.Code
}
