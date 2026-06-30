package handlers

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

// TestRecomputePersonDistance seeds a small org tree and checks distances are
// computed without the ON-CONFLICT-twice bug (shared ancestors collapse to LCA).
func TestRecomputePersonDistance(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	_, _ = pool.Exec(ctx, "DELETE FROM graph.person_distance")
	_, _ = pool.Exec(ctx, "DELETE FROM graph.people")

	// Tree: 1(CEO) ← 2(VP) ← 3(Lead) ← {4(DevA), 5(DevB)}
	for _, p := range []struct {
		eeid     int
		reports  any
		name     string
	}{
		{1, nil, "CEO"}, {2, 1, "VP"}, {3, 2, "Lead"}, {4, 3, "DevA"}, {5, 3, "DevB"},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO graph.people (eeid, reports_to, display_name, machine_id) VALUES ($1,$2,$3,'test')`,
			p.eeid, p.reports, p.name); err != nil {
			t.Fatalf("seed eeid %d: %v", p.eeid, err)
		}
	}

	if err := RecomputePersonDistance(pool, zerolog.Nop())(ctx, nil); err != nil {
		t.Fatalf("recompute: %v", err) // the dup-pair bug surfaced here (SQLSTATE 21000)
	}

	var total int
	pool.QueryRow(ctx, "SELECT count(*) FROM graph.person_distance").Scan(&total)
	if total == 0 {
		t.Fatal("no distances computed")
	}
	// DevA(4) ↔ DevB(5): LCA = Lead(3), hops = 1 + 1 = 2.
	var hops, lca int
	if err := pool.QueryRow(ctx,
		"SELECT hops, lca_eeid FROM graph.person_distance WHERE a_eeid=4 AND b_eeid=5").Scan(&hops, &lca); err != nil {
		t.Fatalf("lookup 4↔5: %v", err)
	}
	if hops != 2 || lca != 3 {
		t.Errorf("4↔5 hops=%d lca=%d, want hops=2 lca=3", hops, lca)
	}
	// DevA(4) ↔ CEO(1): straight line up, hops = 3.
	var h2 int
	pool.QueryRow(ctx, "SELECT hops FROM graph.person_distance WHERE a_eeid=1 AND b_eeid=4").Scan(&h2)
	if h2 != 3 {
		t.Errorf("1↔4 hops=%d, want 3", h2)
	}
}
