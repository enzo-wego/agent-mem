package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/agent-mem/agent-mem/internal/graph/extractor"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rs/zerolog"
)

func TestReconcileEdges_NoSelfLoop(t *testing.T) {
	pool := openTestDB(t)
	ctx := context.Background()
	truncateGraphHandlerTables(t, pool)
	if _, err := pool.Exec(ctx, `ALTER TABLE graph.edges DROP CONSTRAINT IF EXISTS edges_no_self_loop`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		truncateGraphHandlerTables(t, pool)
		if _, err := pool.Exec(ctx, `ALTER TABLE graph.edges ADD CONSTRAINT edges_no_self_loop CHECK (from_node_id <> to_node_id)`); err != nil {
			t.Fatal(err)
		}
	})
	deps := Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "test", Extractor: extractor.New(nil, zerolog.Nop())}
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"self_and_other", "PAY-2400 PAY-2401", 1},
		{"only_self", "PAY-2400", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			truncateGraphHandlerTables(t, pool)
			if _, err := pool.Exec(ctx, `INSERT INTO graph.nodes (id, type, natural_key, updated_at, machine_id) VALUES ('jira:PAY-2400', 'jira', 'PAY-2400', NOW(), 'test')`); err != nil {
				t.Fatal(err)
			}
			res, err := deps.Extractor.Extract(ctx, tc.body)
			if err != nil {
				t.Fatal(err)
			}
			ids, err := reconcileEdges(ctx, deps, "jira:PAY-2400", res.Findings)
			if err != nil {
				t.Fatal(err)
			}
			if len(ids) != tc.want {
				t.Errorf("returned %d edge IDs, want %d", len(ids), tc.want)
			}
			var total, self, other int
			if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE from_node_id = to_node_id), count(*) FILTER (WHERE from_node_id = 'jira:PAY-2400' AND to_node_id = 'jira:PAY-2401' AND kind = 'REFERENCES') FROM graph.edges`).Scan(&total, &self, &other); err != nil {
				t.Fatal(err)
			}
			if total != tc.want || self != 0 || other != tc.want {
				t.Errorf("edges total=%d self=%d other=%d, want %d/0/%d", total, self, other, tc.want, tc.want)
			}
		})
	}
}

func TestEdges_NoSelfLoopConstraint(t *testing.T) {
	pool := openTestDB(t)
	ctx := context.Background()
	truncateGraphHandlerTables(t, pool)
	t.Cleanup(func() { truncateGraphHandlerTables(t, pool) })
	if _, err := pool.Exec(ctx, `INSERT INTO graph.nodes (id, type, natural_key, updated_at, machine_id) VALUES ('jira:PAY-2400', 'jira', 'PAY-2400', NOW(), 'test')`); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(ctx, `INSERT INTO graph.edges (from_node_id, to_node_id, kind, machine_id) VALUES ('jira:PAY-2400', 'jira:PAY-2400', 'REFERENCES', 'test')`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "edges_no_self_loop" {
		t.Fatalf("want edges_no_self_loop SQLSTATE 23514, got %v", err)
	}
}
