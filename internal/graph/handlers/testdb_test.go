package handlers

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// databaseName extracts the database name from a postgres DSN. A DSN that does
// not parse returns "", which fails the test-database guard closed.
func databaseName(dsn string) string {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return ""
	}
	return config.ConnConfig.Database
}

// openTestDB connects to the Postgres instance identified by DATABASE_URL.
// If DATABASE_URL is not set the test is skipped.
func openTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	// This helper DELETEs graph rows. On 2026-07-14 an integration test run
	// against the live dev database hard-deleted the graph and synced the damage
	// to prod. Require the dedicated scratch database exactly.
	if databaseName(dsn) != "agentmem_test" {
		t.Fatalf("refusing to run: DATABASE_URL database name %q is not \"agentmem_test\"; "+
			"handler tests may delete graph rows", databaseName(dsn))
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
		"graph.eligibility_decisions",
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
