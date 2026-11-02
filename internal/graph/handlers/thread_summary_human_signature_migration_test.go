package handlers

import (
	"context"
	"database/sql"
	"os"
	"slices"
	"testing"

	"github.com/pressly/goose/v3"
)

const threadSummaryHumanSignatureBaseVersion int64 = 20261027095115

func TestThreadSummaryHumanSignatureMigration(t *testing.T) {
	ctx := context.Background()
	// Enforce the repository's scratch-database guard before opening a second
	// database/sql connection for goose.
	_ = openTestDB(t)
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
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
		t.Fatal(err)
	}
	assertHumanSignatureColumn(t, db, true)
	statuses, err := provider.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var versions []int64
	for _, status := range statuses {
		if status.State == goose.StateApplied && status.Source.Version > threadSummaryHumanSignatureBaseVersion {
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
	assertHumanSignatureColumn(t, db, false)
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	assertHumanSignatureColumn(t, db, true)
}

func assertHumanSignatureColumn(t *testing.T, db *sql.DB, want bool) {
	t.Helper()
	var dataType, nullable string
	var defaultValue sql.NullString
	err := db.QueryRow(`SELECT data_type,is_nullable,column_default FROM information_schema.columns WHERE table_schema='graph' AND table_name='thread_summaries' AND column_name='human_signature'`).Scan(&dataType, &nullable, &defaultValue)
	if !want {
		if err == sql.ErrNoRows {
			return
		}
		t.Fatalf("human_signature remains after down: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if dataType != "text" || nullable != "NO" || !defaultValue.Valid || defaultValue.String != "''::text" {
		t.Fatalf("human_signature type=%q nullable=%q default=%q", dataType, nullable, defaultValue.String)
	}
}
