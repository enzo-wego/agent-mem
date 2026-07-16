package hydrate_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/hydrate"
)

func testDB(t *testing.T) *pgxpool.Pool {
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
	t.Cleanup(func() {
		cleanupHydrateTables(t, pool)
		pool.Close()
	})
	cleanupHydrateTables(t, pool)
	return pool
}

func cleanupHydrateTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, tbl := range []string{"graph.artifact_bodies", "graph.thread_summaries", "graph.nodes"} {
		if _, err := pool.Exec(ctx, "DELETE FROM "+tbl); err != nil {
			t.Logf("cleanup %s: %v", tbl, err)
		}
	}
}

func seedNode(t *testing.T, pool *pgxpool.Pool, id, typ, title string) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
INSERT INTO graph.nodes (id, type, natural_key, title, machine_id)
VALUES ($1, $2, $1, $3, 'test')
ON CONFLICT (id) DO NOTHING`, id, typ, title)
	if err != nil {
		t.Fatalf("seedNode %s: %v", id, err)
	}
}

func seedBody(t *testing.T, pool *pgxpool.Pool, nodeID, body string) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
INSERT INTO graph.artifact_bodies (node_id, body_full, machine_id)
VALUES ($1, $2, 'test')
ON CONFLICT (node_id) DO UPDATE SET body_full = EXCLUDED.body_full`, nodeID, body)
	if err != nil {
		t.Fatalf("seedBody %s: %v", nodeID, err)
	}
}

func TestHydrate_StopsAtBudget(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	// Seed 3 bodies of ~200 tokens each (800 chars approx).
	// Budget is 600 tokens → first two fit (200+200=400≤600), third does not (600+200>600).
	for i, id := range []string{"a", "b", "c"} {
		seedNode(t, pool, id, "slack_thread", "thread "+id)
		seedBody(t, pool, id, strings.Repeat("word ", 160)+" #"+string(rune('a'+i)))
	}
	cands := []hydrate.Candidate{
		{NodeID: "a", Score: 0.9},
		{NodeID: "b", Score: 0.8},
		{NodeID: "c", Score: 0.7},
	}
	out, missed, err := hydrate.Greedy(ctx, pool, cands, 600)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Errorf("want 2 hydrated, got %d", len(out))
	}
	if len(missed) != 0 {
		t.Errorf("unexpected cache misses %v", missed)
	}
	if out[0].NodeID != "a" {
		t.Errorf("highest score should be loaded first; got %s", out[0].NodeID)
	}
}

// A slack thread whose n.title is empty must hydrate with the title from
// graph.thread_summaries, so the opened-node header shows readable text instead
// of the raw slack:CHANNEL:TS id.
func TestHydrate_SlackTitleFallsBackToThreadSummary(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	const id = "slack:CTEST:111.222"
	if _, err := pool.Exec(ctx, `
INSERT INTO graph.nodes (id, type, natural_key, title, scope, machine_id)
VALUES ($1, 'slack', $1, '', 'slack:CTEST', 'test')`, id); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	seedBody(t, pool, id, "the raw thread body")
	if _, err := pool.Exec(ctx, `
INSERT INTO graph.thread_summaries (channel_id, thread_ts, signature, summary)
VALUES ('CTEST', '111.222', 'sig', 'Umrah 422 investigation')`); err != nil {
		t.Fatalf("seed thread_summary: %v", err)
	}

	out, _, err := hydrate.Greedy(ctx, pool, []hydrate.Candidate{{NodeID: id, Score: 1}}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("want 1 hydrated, got %d", len(out))
	}
	if out[0].Title != "Umrah 422 investigation" {
		t.Errorf("want thread-summary title fallback, got %q", out[0].Title)
	}
}

func TestHydrate_ReportsCacheMisses(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	seedNode(t, pool, "a", "jira", "PAY-1") // node exists, body does NOT
	cands := []hydrate.Candidate{{NodeID: "a", Score: 0.9}}
	out, missed, _ := hydrate.Greedy(ctx, pool, cands, 1000)
	if len(out) != 0 {
		t.Errorf("nothing should be hydrated")
	}
	if len(missed) != 1 || missed[0] != "a" {
		t.Errorf("missed=%v want [a]", missed)
	}
}
