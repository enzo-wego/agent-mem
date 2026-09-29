package handlers

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

const (
	jiraUpdatesMigrationVersion = int64(20260929065720)
	migrationsDirFromHandlers   = "../../../migrations"
)

// TestJiraUpdatesMigration_UpDown runs the jira_updates_settings migration
// down and up again on the scratch database.
func TestJiraUpdatesMigration_UpDown(t *testing.T) {
	pool := openTestDB(t)
	ctx := context.Background()
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("SetDialect: %v", err)
	}
	up := func() {
		t.Helper()
		if err := goose.UpTo(db, migrationsDirFromHandlers, jiraUpdatesMigrationVersion, goose.WithAllowMissing()); err != nil {
			t.Fatalf("goose up: %v", err)
		}
	}
	// Always leave the scratch DB migrated, even if an assertion fails midway.
	t.Cleanup(func() {
		_ = goose.UpTo(db, migrationsDirFromHandlers, jiraUpdatesMigrationVersion, goose.WithAllowMissing())
	})
	seeded := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM settings WHERE key IN
			('jira_updates_enabled','jira_updates_interval_minutes','jira_updates_cursor')`).Scan(&n); err != nil {
			t.Fatalf("count seeded: %v", err)
		}
		return n
	}
	anyLeft := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM settings WHERE key LIKE 'jira_updates\_%'`).Scan(&n); err != nil {
			t.Fatalf("count left: %v", err)
		}
		return n
	}

	up()
	if n := seeded(); n != 3 {
		t.Fatalf("after up: %d seeded keys, want 3", n)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO settings (key, value) VALUES ('jira_updates_last_ok_at', '2026-09-29T06:00:00Z')
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`); err != nil {
		t.Fatalf("seed runtime key: %v", err)
	}
	if v, err := goose.GetDBVersion(db); err != nil || v != jiraUpdatesMigrationVersion {
		t.Fatalf("db version = %d (%v), want %d; refusing to roll back another migration", v, err, jiraUpdatesMigrationVersion)
	}
	if err := goose.Down(db, migrationsDirFromHandlers); err != nil {
		t.Fatalf("goose down: %v", err)
	}
	if n := anyLeft(); n != 0 {
		t.Fatalf("after down: %d jira_updates_ keys left, want 0", n)
	}
	up()
	if n := seeded(); n != 3 {
		t.Fatalf("after re-up: %d seeded keys, want 3", n)
	}
}
