package identity_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// parseScratchDSN validates the database pgx will actually connect to.
func parseScratchDSN(dsn string) (*pgxpool.Config, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("refusing to run: invalid DATABASE_URL: %w", err)
	}
	if cfg.ConnConfig.Database != "agentmem_test" {
		return nil, fmt.Errorf("refusing to run: DATABASE_URL database name %q is not \"agentmem_test\"; tests may delete graph rows", cfg.ConnConfig.Database)
	}
	return cfg, nil
}

func checkScratchDSN(dsn string) error {
	_, err := parseScratchDSN(dsn)
	return err
}

// openTestDB connects to the Postgres instance identified by DATABASE_URL.
// If DATABASE_URL is not set the test is skipped.
func openTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	cfg, err := parseScratchDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pgxpool.NewWithConfig: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("DB ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// truncateGraphTables removes test data from all graph tables used by identity tests.
func truncateGraphTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	// Order matters due to FK constraints.
	tables := []string{
		"graph.identity_map",
		"graph.nodes",
		"graph.people",
	}
	for _, tbl := range tables {
		if _, err := pool.Exec(ctx, "DELETE FROM "+tbl); err != nil {
			t.Fatalf("truncate %s: %v", tbl, err)
		}
	}
}
