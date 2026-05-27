package handlers_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testDB opens a pool to the DATABASE_URL postgres instance.
// Skips the test if DATABASE_URL is not set.
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
		truncateHandlerTables(t, pool)
		pool.Close()
	})
	truncateHandlerTables(t, pool)
	return pool
}

func truncateHandlerTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, tbl := range []string{
		"graph.artifact_index",
		"graph.artifact_bodies",
		"graph.edges",
		"graph.jobs",
		"graph.nodes",
		"graph.identity_map",
		"graph.people",
	} {
		if _, err := pool.Exec(ctx, "DELETE FROM "+tbl); err != nil {
			t.Logf("truncate %s: %v", tbl, err)
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

func seedNodeURL(t *testing.T, pool *pgxpool.Pool, id, url string) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
UPDATE graph.nodes SET url = $2 WHERE id = $1`, id, url)
	if err != nil {
		t.Fatalf("seedNodeURL %s: %v", id, err)
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

func seedEdge(t *testing.T, pool *pgxpool.Pool, from, to, kind string) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
INSERT INTO graph.edges (from_node_id, to_node_id, kind, machine_id)
VALUES ($1, $2, $3, 'test')
ON CONFLICT (from_node_id, to_node_id, kind) DO NOTHING`, from, to, kind)
	if err != nil {
		t.Fatalf("seedEdge %s->%s: %v", from, to, err)
	}
}
