package entities_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/entities"
)

// -----------------------------------------------------------------------
// DB helpers
// -----------------------------------------------------------------------

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

func truncateEntities(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), "DELETE FROM graph.entities"); err != nil {
		t.Fatalf("truncate graph.entities: %v", err)
	}
}

func countEntities(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT COUNT(*) FROM graph.entities").Scan(&n); err != nil {
		t.Fatalf("count entities: %v", err)
	}
	return n
}

func countEntitiesKind(t *testing.T, pool *pgxpool.Pool, kind string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		"SELECT COUNT(*) FROM graph.entities WHERE kind = $1", kind).Scan(&n); err != nil {
		t.Fatalf("count entities kind=%s: %v", kind, err)
	}
	return n
}

func entityExists(t *testing.T, pool *pgxpool.Pool, id string) bool {
	t.Helper()
	var n int
	pool.QueryRow(context.Background(),
		"SELECT COUNT(*) FROM graph.entities WHERE id = $1", id).Scan(&n)
	return n > 0
}

// -----------------------------------------------------------------------
// SeedFromPaymentsRepo tests
// -----------------------------------------------------------------------

func TestSeedFromPaymentsRepo_ScansDir(t *testing.T) {
	pool := openTestDB(t)
	truncateEntities(t, pool)
	log := zerolog.Nop()

	// Create a temp dir with fake pkg/payment subdirs.
	root := t.TempDir()
	for _, name := range []string{"tabby", "checkout", "triplea"} {
		dir := root + "/pkg/payment/" + name
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	// Also create a file (not a dir) — should be skipped.
	f, err := os.Create(root + "/pkg/payment/README.md")
	if err != nil {
		t.Fatalf("create file: %v", err)
	}
	f.Close()

	count, err := entities.SeedFromPaymentsRepo(context.Background(), pool, root, log)
	if err != nil {
		t.Fatalf("SeedFromPaymentsRepo: %v", err)
	}
	if count != 3 {
		t.Errorf("expected count=3, got %d", count)
	}
	if n := countEntities(t, pool); n != 3 {
		t.Errorf("expected 3 rows in graph.entities, got %d", n)
	}
	for _, name := range []string{"tabby", "checkout", "triplea"} {
		wantID := "partner:" + name
		if !entityExists(t, pool, wantID) {
			t.Errorf("expected entity %q to exist", wantID)
		}
	}
}

func TestSeedFromPaymentsRepo_EmptyPath(t *testing.T) {
	pool := openTestDB(t)
	truncateEntities(t, pool)
	log := zerolog.Nop()

	count, err := entities.SeedFromPaymentsRepo(context.Background(), pool, "", log)
	if err != nil {
		t.Fatalf("expected no error for empty path, got: %v", err)
	}
	if count != 0 {
		t.Errorf("expected count=0 for empty path, got %d", count)
	}
}

func TestSeedFromPaymentsRepo_NonExistentPath(t *testing.T) {
	pool := openTestDB(t)
	truncateEntities(t, pool)
	log := zerolog.Nop()

	count, err := entities.SeedFromPaymentsRepo(context.Background(), pool, "/tmp/nonexistent-payments-repo-xyz", log)
	if err != nil {
		t.Fatalf("expected no error for non-existent path, got: %v", err)
	}
	if count != 0 {
		t.Errorf("expected count=0 for non-existent path, got %d", count)
	}
}

func TestSeedFromPaymentsRepo_Idempotent(t *testing.T) {
	pool := openTestDB(t)
	truncateEntities(t, pool)
	log := zerolog.Nop()

	root := t.TempDir()
	for _, name := range []string{"tabby", "checkout"} {
		if err := os.MkdirAll(root+"/pkg/payment/"+name, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}

	// First seed.
	count1, err := entities.SeedFromPaymentsRepo(context.Background(), pool, root, log)
	if err != nil {
		t.Fatalf("first seed: %v", err)
	}

	// Second seed — idempotent.
	count2, err := entities.SeedFromPaymentsRepo(context.Background(), pool, root, log)
	if err != nil {
		t.Fatalf("second seed: %v", err)
	}

	if count1 != count2 {
		t.Errorf("counts differ: first=%d second=%d", count1, count2)
	}
	if n := countEntities(t, pool); n != 2 {
		t.Errorf("expected exactly 2 rows after double seed, got %d", n)
	}
}

// -----------------------------------------------------------------------
// LoadFromCSV tests
// -----------------------------------------------------------------------

const sampleCSV = `kind,display_name,aliases
partner,TripleA,TripleA|3A|triple a
feature,Auto Refund,auto refund|auto-refund|auto_refund
status,None,none|null|empty
currency,TRY,TRY|try|Turkish Lira
`

func TestLoadFromCSV(t *testing.T) {
	pool := openTestDB(t)
	truncateEntities(t, pool)
	log := zerolog.Nop()

	count, err := entities.LoadFromCSV(context.Background(), pool, strings.NewReader(sampleCSV), log)
	if err != nil {
		t.Fatalf("LoadFromCSV: %v", err)
	}
	if count != 4 {
		t.Errorf("expected count=4, got %d", count)
	}

	// Verify specific IDs exist.
	for _, wantID := range []string{
		"partner:triplea",
		"feature:auto_refund",
		"status:none",
		"currency:try",
	} {
		if !entityExists(t, pool, wantID) {
			t.Errorf("expected entity %q to exist", wantID)
		}
	}
}

func TestLoadFromCSV_Idempotent(t *testing.T) {
	pool := openTestDB(t)
	truncateEntities(t, pool)
	log := zerolog.Nop()

	count1, err := entities.LoadFromCSV(context.Background(), pool, strings.NewReader(sampleCSV), log)
	if err != nil {
		t.Fatalf("first LoadFromCSV: %v", err)
	}

	count2, err := entities.LoadFromCSV(context.Background(), pool, strings.NewReader(sampleCSV), log)
	if err != nil {
		t.Fatalf("second LoadFromCSV: %v", err)
	}

	if count1 != count2 {
		t.Errorf("counts differ: first=%d second=%d", count1, count2)
	}
	if n := countEntities(t, pool); n != int(count1) {
		t.Errorf("expected %d rows after double load, got %d", count1, n)
	}
}

func TestLoadFromCSV_EmptyReader(t *testing.T) {
	pool := openTestDB(t)
	truncateEntities(t, pool)
	log := zerolog.Nop()

	count, err := entities.LoadFromCSV(context.Background(), pool, strings.NewReader(""), log)
	if err != nil {
		t.Fatalf("LoadFromCSV empty: %v", err)
	}
	if count != 0 {
		t.Errorf("expected count=0 for empty input, got %d", count)
	}
}

func TestLoadFromCSV_HeaderOnly(t *testing.T) {
	pool := openTestDB(t)
	truncateEntities(t, pool)
	log := zerolog.Nop()

	count, err := entities.LoadFromCSV(context.Background(), pool, strings.NewReader("kind,display_name,aliases\n"), log)
	if err != nil {
		t.Fatalf("LoadFromCSV header-only: %v", err)
	}
	if count != 0 {
		t.Errorf("expected count=0 for header-only input, got %d", count)
	}
}

func TestLoadFromCSV_KindPartner(t *testing.T) {
	pool := openTestDB(t)
	truncateEntities(t, pool)
	log := zerolog.Nop()

	csv := "kind,display_name,aliases\npartner,Checkout,checkout|ext-wego-checkout\n"
	count, err := entities.LoadFromCSV(context.Background(), pool, strings.NewReader(csv), log)
	if err != nil {
		t.Fatalf("LoadFromCSV: %v", err)
	}
	if count != 1 {
		t.Errorf("expected count=1, got %d", count)
	}
	if !entityExists(t, pool, "partner:checkout") {
		t.Error("expected entity 'partner:checkout' to exist")
	}
	if n := countEntitiesKind(t, pool, "partner"); n != 1 {
		t.Errorf("expected 1 partner entity, got %d", n)
	}
}
