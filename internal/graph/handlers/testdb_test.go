package handlers

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
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

// truncateGraphHandlerTables removes test data from all graph tables used by handler tests.
func truncateGraphHandlerTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	tables := []string{
		"graph.artifact_index",
		"graph.artifact_bodies",
		"graph.jira_epic_map",
		"graph.pinned_threads",
		"graph.edges",
		"graph.jobs",
		"graph.nodes",
		"graph.identity_map",
		"graph.people",
	}
	for _, tbl := range tables {
		if _, err := pool.Exec(ctx, "DELETE FROM "+tbl); err != nil {
			t.Fatalf("truncate %s: %v", tbl, err)
		}
	}
}
