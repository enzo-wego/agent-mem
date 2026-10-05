package handlers

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"
)

func TestArtifactTSV_Trigger(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO graph.nodes (id, type, natural_key, metadata, machine_id) VALUES ('trg:1','jira','trg:1','{}','test')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO graph.artifact_index (node_id, summary, decisions_text, identifiers, summary_kind, machine_id)
		VALUES ('trg:1', 'alphaword', 'betaword', ARRAY['QWX77'], 'heuristic', 'test')`); err != nil {
		t.Fatal(err)
	}
	get := func() string {
		var s string
		if err := pool.QueryRow(ctx, `SELECT tsv::text FROM graph.artifact_index WHERE node_id='trg:1'`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	first := get()
	for _, w := range []string{"alphaword", "betaword", "qwx77"} {
		if !strings.Contains(first, "'"+w+"'") {
			t.Fatalf("tsv %q lacks %q", first, w)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE graph.artifact_index SET decisions_text='gammaword' WHERE node_id='trg:1'`); err != nil {
		t.Fatal(err)
	}
	second := get()
	if second == first || !strings.Contains(second, "'gammaword'") {
		t.Fatalf("decisions_text update did not change tsv: %q", second)
	}
	// Embedding-only update must not recompute (prove it by corrupting tsv
	// first: a recompute would restore it).
	if _, err := pool.Exec(ctx, `UPDATE graph.artifact_index SET tsv = to_tsvector('simple','sentinel') WHERE node_id='trg:1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE graph.artifact_index SET embedding = NULL, refreshed_at = now() WHERE node_id='trg:1'`); err != nil {
		t.Fatal(err)
	}
	if got := get(); got != "'sentinel':1" {
		t.Fatalf("embedding-only update changed tsv: %q", got)
	}
}

func tsvDownSQL(t *testing.T) string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(migrationsDirFromHandlers, "*_artifact_index_tsv.sql"))
	if len(files) != 1 {
		t.Fatalf("want one artifact_index_tsv migration, got %v", files)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	_, down, ok := strings.Cut(string(b), "-- +goose Down")
	if !ok {
		t.Fatal("no Down section")
	}
	return down
}

func TestArtifactTSV_DownLockBounded(t *testing.T) {
	pool := openTestDB(t)
	ctx := context.Background()
	down := tsvDownSQL(t)

	a, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Rollback(ctx)
	if _, err := a.Exec(ctx, `LOCK TABLE graph.artifact_index IN ACCESS SHARE MODE`); err != nil {
		t.Fatal(err)
	}

	b, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = b.Exec(ctx, down)
	_ = b.Rollback(ctx)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "55P03" {
		t.Fatalf("down error = %v, want lock_not_available 55P03", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("down took %v, want < 10s", d)
	}
	_ = a.Rollback(ctx)

	var cols, trgs int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM information_schema.columns WHERE table_schema='graph' AND table_name='artifact_index' AND column_name='tsv'),
		(SELECT count(*) FROM pg_trigger WHERE tgrelid='graph.artifact_index'::regclass AND tgname='artifact_index_tsv_trg')`).Scan(&cols, &trgs); err != nil {
		t.Fatal(err)
	}
	if cols != 1 || trgs != 1 {
		t.Fatalf("after failed down: tsv column=%d trigger=%d, want 1/1", cols, trgs)
	}
}

func tsvUpSQL(t *testing.T) string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(migrationsDirFromHandlers, "*_artifact_index_tsv.sql"))
	if len(files) != 1 {
		t.Fatalf("want one artifact_index_tsv migration, got %v", files)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	up, _, ok := strings.Cut(string(b), "-- +goose Down")
	if !ok {
		t.Fatal("no Down section")
	}
	return up
}

// downToTSVBase selects by version, not application order. ApplyVersion fixtures
// reapply older migrations, which makes DownTo stop at an older row too early.
func downToTSVBase(t *testing.T, ctx context.Context, provider *goose.Provider) {
	t.Helper()
	statuses, err := provider.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var versions []int64
	for _, status := range statuses {
		if status.State == goose.StateApplied && status.Source.Version > tsvBaseVersion {
			versions = append(versions, status.Source.Version)
		}
	}
	slices.Sort(versions)
	slices.Reverse(versions)
	for _, version := range versions {
		if _, err := provider.ApplyVersion(ctx, version, false); err != nil {
			t.Fatalf("down migration %d: %v", version, err)
		}
	}
	if version, err := provider.GetDBVersion(ctx); err != nil || version != tsvBaseVersion {
		t.Fatalf("down to base: version=%d error=%v, want %d", version, err, tsvBaseVersion)
	}
}

// TestArtifactTSV_UpLockBounded: with a reader holding ACCESS SHARE on
// artifact_index, the Up section fails within its lock_timeout and applies
// nothing.
func TestArtifactTSV_UpLockBounded(t *testing.T) {
	pool := openTestDB(t)
	ctx := context.Background()
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// ApplyVersion tests can reapply older migrations after newer ones.
	// Use the provider's version-aware history, not legacy insertion-order state.
	provider, err := goose.NewProvider(goose.DialectPostgres, db, os.DirFS(migrationsDirFromHandlers), goose.WithAllowOutofOrder(true))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := provider.Up(ctx); err != nil {
			t.Errorf("restore migrations: %v", err)
		}
	})
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	downToTSVBase(t, ctx, provider)
	up := tsvUpSQL(t)

	a, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Rollback(ctx)
	if _, err := a.Exec(ctx, `LOCK TABLE graph.artifact_index IN ACCESS SHARE MODE`); err != nil {
		t.Fatal(err)
	}
	b, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = b.Exec(ctx, up)
	_ = b.Rollback(ctx)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "55P03" {
		t.Fatalf("up error = %v, want lock_not_available 55P03", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("up took %v, want < 10s", d)
	}
	_ = a.Rollback(ctx)

	var cols, trgs, fns int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM information_schema.columns WHERE table_schema='graph' AND table_name='artifact_index' AND column_name='tsv'),
		(SELECT count(*) FROM pg_trigger WHERE tgrelid='graph.artifact_index'::regclass AND tgname='artifact_index_tsv_trg'),
		(SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='graph' AND p.proname IN ('identifiers_text','artifact_index_tsv_doc','artifact_index_tsv_trg'))`).Scan(&cols, &trgs, &fns); err != nil {
		t.Fatal(err)
	}
	if cols != 0 || trgs != 0 || fns != 0 {
		t.Fatalf("after failed up: tsv column=%d trigger=%d functions=%d, want 0/0/0", cols, trgs, fns)
	}
}
