package scoring_test

import (
	"context"
	"testing"

	"github.com/agent-mem/agent-mem/internal/graph/scoring"
)

func seedPersonDistance(t *testing.T, pool interface {
	Exec(ctx context.Context, sql string, args ...any) (interface{ RowsAffected() int64 }, error)
}, a, b, hops, lca int) {
	t.Helper()
	// Use pgxpool.Pool directly via testDB.
}

func seedPersonDistanceRow(t *testing.T, pool interface {
	Exec(ctx context.Context, sql string, arguments ...any) (interface{ RowsAffected() int64 }, error)
}, a, b, hops, lca int) {
	t.Helper()
}

func TestLookupDistance_HitFromMaterialised(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	// Seed the person_distance table directly.
	_, err := pool.Exec(ctx, `
INSERT INTO graph.person_distance (a_eeid, b_eeid, hops, lca_eeid)
VALUES ($1, $2, $3, $4)
ON CONFLICT (a_eeid, b_eeid) DO UPDATE SET hops = EXCLUDED.hops`, 259, 982, 1, 1192)
	if err != nil {
		t.Fatalf("seed person_distance: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(ctx, `DELETE FROM graph.person_distance WHERE a_eeid=259 AND b_eeid=982`)
	})

	d, err := scoring.LookupDistance(ctx, pool, 982, 259)
	if err != nil {
		t.Fatal(err)
	}
	if d != 1 {
		t.Errorf("got %d want 1", d)
	}
}

func TestLookupDistance_MissReturnsMaxInt(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	d, err := scoring.LookupDistance(ctx, pool, 982, 99999)
	if err != nil {
		t.Fatal(err)
	}
	if d < 100 {
		t.Errorf("missing pair should be huge; got %d", d)
	}
}
