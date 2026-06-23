package scoring_test

import (
	"context"
	"math"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/scoring"
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
	t.Cleanup(pool.Close)
	return pool
}

func seedAppSetting(t *testing.T, pool *pgxpool.Pool, key, value string) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
INSERT INTO public.settings (key, value)
VALUES ($1, $2)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, key, value)
	if err != nil {
		t.Fatalf("seedAppSetting: %v", err)
	}
}

func TestCombine_WithDefaultWeights(t *testing.T) {
	w := scoring.Weights{Sem: 0.50, Rec: 0.15, Edge: 0.15, Team: 0.15, Auth: 0.05}
	c := scoring.Components{Sem: 0.92, Rec: 1.0, Edge: 1.0, Team: 0.9, Auth: 0.33}
	got := scoring.Combine(w, c)
	want := 0.50*0.92 + 0.15*1.0 + 0.15*1.0 + 0.15*0.9 + 0.05*0.33
	if math.Abs(got-want) > 0.001 {
		t.Errorf("got %.4f want %.4f", got, want)
	}
}

func TestLoadWeights_FromAppSettings(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	seedAppSetting(t, pool, "graph.weights.sem", "0.40")
	seedAppSetting(t, pool, "graph.weights.rec", "0.20")
	seedAppSetting(t, pool, "graph.weights.edge", "0.20")
	seedAppSetting(t, pool, "graph.weights.team", "0.15")
	seedAppSetting(t, pool, "graph.weights.auth", "0.05")

	w, err := scoring.LoadWeights(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if w.Sem != 0.40 || w.Rec != 0.20 || w.Edge != 0.20 || w.Team != 0.15 || w.Auth != 0.05 {
		t.Errorf("got %+v", w)
	}
}
