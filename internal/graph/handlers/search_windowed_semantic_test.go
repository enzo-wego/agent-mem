package handlers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Record the actual arm query for EXPLAIN, and transaction statements to prove
// that an unwindowed call still uses the direct pool query.
type windowedSemanticTrace struct {
	sql          string
	args         []any
	transactions int
}

func (tr *windowedSemanticTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(strings.TrimSpace(d.SQL), "SELECT n.id, 1.0 -") {
		tr.sql, tr.args = d.SQL, d.Args
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(d.SQL)), "begin") {
		tr.transactions++
	}
	return ctx
}
func (*windowedSemanticTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func windowedSemanticDB(t *testing.T) (*pgxpool.Pool, *windowedSemanticTrace) {
	t.Helper()
	base := winReset(t)
	// The shared reset DELETEs rows; it does not remove dead HNSW entries.
	// Rebuild the empty index so recall depends only on this fixture, not on
	// earlier tests or autovacuum timing. Leave all search settings unchanged.
	winExec(t, base, "REINDEX INDEX graph.idx_artifact_index_embedding")
	cfg := base.Config()
	cfg.MaxConns = 1
	// Connection-local planner controls avoid changing database defaults shared
	// by other tests. Discourage exact scans AND sorts; EXPLAIN verifies HNSW.
	cfg.ConnConfig.RuntimeParams["enable_seqscan"] = "off"
	cfg.ConnConfig.RuntimeParams["enable_sort"] = "off"
	tr := &windowedSemanticTrace{}
	cfg.ConnConfig.Tracer = tr
	db, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	for name, want := range map[string]string{"hnsw.ef_search": "40", "hnsw.iterative_scan": "off"} {
		var got string
		// Load pgvector before SHOW on a fresh connection.
		winExec(t, db, "SELECT '[1,0]'::vector")
		if err := db.QueryRow(context.Background(), "SHOW "+name).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s=%s, want %s", name, got, want)
		}
	}
	for i := range 60 {
		id := fmt.Sprintf("ordinary:outside:%02d", i)
		winNode(t, db, id, "jira", "2026-10-07T00:00:00Z", "2026-10-07T00:00:00Z")
		windowArmsIndex(t, db, id, "unrelated", windowArmsVector(0.99-float64(i)*0.001))
	}
	for i := range 3 {
		id := fmt.Sprintf("ordinary:inside:%02d", i)
		winNode(t, db, id, "jira", "2026-09-30T00:00:00Z", "2026-09-30T00:00:00Z")
		windowArmsIndex(t, db, id, "unrelated", windowArmsVector(0.84-float64(i)*0.02))
	}
	winExec(t, db, "ANALYZE graph.nodes; ANALYZE graph.artifact_index")
	return db, tr
}

type windowedSemanticCall func(context.Context, *pgxpool.Pool, []float32, searchFilter, int) ([]armHit, error)

func TestWindowedSemanticRefill(t *testing.T) {
	for _, arm := range []struct {
		name string
		call windowedSemanticCall
	}{
		{"semanticArm", semanticArm}, {"semanticArmFolded", semanticArmFolded},
	} {
		t.Run(arm.name, func(t *testing.T) {
			db, tr := windowedSemanticDB(t)
			vec := windowArmsVector(1)
			win := windowArmsWindow()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			hits, err := arm.call(ctx, db, vec, searchFilter{win: &win}, 3)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := db.Query(ctx, "EXPLAIN "+tr.sql, tr.args...)
			if err != nil {
				t.Fatal(err)
			}
			var plan strings.Builder
			for rows.Next() {
				var line string
				if err := rows.Scan(&line); err != nil {
					rows.Close()
					t.Fatal(err)
				}
				fmt.Fprintln(&plan, line)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			t.Logf("actual windowed query EXPLAIN:\n%s", plan.String())
			if !strings.Contains(plan.String(), "idx_artifact_index_embedding") {
				t.Fatal("fixture did not use HNSW")
			}
			t.Logf("windowed hits: %v", hits)
			if len(hits) != 3 {
				t.Fatalf("windowed semantic hits=%d, want 3", len(hits))
			}
			for i, h := range hits {
				want := fmt.Sprintf("ordinary:inside:%02d", i)
				if h.ID != want || h.Score < semanticMinCosine {
					t.Fatalf("hit %d=%v, want %s above floor", i, h, want)
				}
				if i > 0 && hits[i-1].Score < h.Score {
					t.Fatal("cosine order increased")
				}
			}
			tr.transactions = 0
			hits, err = arm.call(ctx, db, vec, searchFilter{}, 3)
			if err != nil {
				t.Fatal(err)
			}
			if tr.transactions != 0 {
				t.Fatal("unwindowed search opened a transaction")
			}
			if len(hits) != 3 {
				t.Fatalf("unwindowed hits=%v", hits)
			}
			for i, h := range hits {
				if h.ID != fmt.Sprintf("ordinary:outside:%02d", i) {
					t.Fatalf("unwindowed closest hit %d=%v", i, h)
				}
			}
			t.Logf("unwindowed closest hits: %v; transactions=%d", hits, tr.transactions)
		})
	}
}

func TestWindowedSemanticCleanup(t *testing.T) {
	for _, arm := range []struct {
		name string
		call windowedSemanticCall
	}{
		{"semanticArm", semanticArm}, {"semanticArmFolded", semanticArmFolded},
	} {
		t.Run(arm.name, func(t *testing.T) {
			db, _ := windowedSemanticDB(t)
			win := windowArmsWindow()
			for _, scenario := range []string{"success", "wrong_dimension", "cancel_inside_transaction"} {
				t.Run(scenario, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					vec := windowArmsVector(1)
					if scenario == "wrong_dimension" {
						vec = []float32{1, 0}
					}
					hookCalled := false
					if scenario == "cancel_inside_transaction" {
						afterSetLocalHook = func() { hookCalled = true; cancel() }
						defer func() { afterSetLocalHook = nil }()
					}
					hits, err := arm.call(ctx, db, vec, searchFilter{win: &win}, 3)
					afterSetLocalHook = nil
					switch scenario {
					case "success":
						if err != nil || len(hits) != 3 {
							t.Fatalf("success hits=%v err=%v", hits, err)
						}
					case "wrong_dimension":
						if err == nil {
							t.Fatal("wrong dimension error was swallowed")
						}
					case "cancel_inside_transaction":
						if !hookCalled || !errors.Is(err, context.Canceled) {
							t.Fatalf("hook called=%v err=%v, want context.Canceled", hookCalled, err)
						}
					}
					// Never reuse the cancelled context for cleanup verification.
					showCtx, showCancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer showCancel()
					var setting string
					if err := db.QueryRow(showCtx, "SHOW hnsw.iterative_scan").Scan(&setting); err != nil {
						t.Fatal(err)
					}
					if setting != "off" {
						t.Fatalf("leaked iterative_scan=%s", setting)
					}
					searchCtx, searchCancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer searchCancel()
					hits, err = arm.call(searchCtx, db, windowArmsVector(1), searchFilter{}, 3)
					if err != nil {
						t.Fatal(err)
					}
					if len(hits) != 3 || hits[0].ID != "ordinary:outside:00" {
						t.Fatalf("unwindowed recovery hits=%v", hits)
					}
					t.Logf("%s: iterative_scan=%s; unwindowed recovery closest=%s", scenario, setting, hits[0].ID)
				})
			}
		})
	}
}
