package trace

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPruneDeletesOnlyOlderThanThirtyDays(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil || cfg.ConnConfig.Database != "agentmem_test" {
		t.Fatalf("DATABASE_URL must select agentmem_test: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	store := PostgresStore{DB: pool}
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		id  string
		age time.Duration
	}{{"ctx_old", 31 * 24 * time.Hour}, {"ctx_boundary", 30 * 24 * time.Hour}, {"ctx_new", 29 * 24 * time.Hour}} {
		_, _ = pool.Exec(context.Background(), `DELETE FROM graph.context_traces WHERE id=$1`, row.id)
		if err := store.Write(context.Background(), Record{ID: row.id, CreatedAt: now.Add(-row.age), Route: "resolve", BuildRev: "test", Caller: "test", Status: 200, AskerInput: map[string]any{}, ACLMode: "unfiltered", Request: map[string]any{}, Items: []any{}, ResponseSHA256: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := Prune(context.Background(), store, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("deleted=%d want=1", n)
	}
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM graph.context_traces WHERE id IN ('ctx_old','ctx_boundary','ctx_new')`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("remaining=%d want=2", count)
	}
}
