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
	provider, err := goose.NewProvider(goose.DialectPostgres, db, os.DirFS(migrationsDirFromHandlers), goose.WithAllowOutofOrder(true))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	initialVersion, err := provider.GetDBVersion(ctx)
	if err != nil {
		t.Fatalf("initial DB version: %v", err)
	}
	up := func() {
		t.Helper()
		if _, err := provider.ApplyVersion(ctx, jiraUpdatesMigrationVersion, true); err != nil {
			t.Fatalf("apply jira_updates up: %v", err)
		}
	}
	// Restore only this migration if an assertion stops the test after Down.
	t.Cleanup(func() {
		statuses, err := provider.Status(ctx)
		if err != nil {
			t.Errorf("migration cleanup status: %v", err)
			return
		}
		for _, status := range statuses {
			if status.Source.Version == jiraUpdatesMigrationVersion {
				if status.State == goose.StatePending {
					if _, err := provider.ApplyVersion(ctx, jiraUpdatesMigrationVersion, true); err != nil {
						t.Errorf("migration cleanup up: %v", err)
					}
				}
				return
			}
		}
		t.Errorf("migration cleanup: version %d not found", jiraUpdatesMigrationVersion)
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

	if n := seeded(); n != 3 {
		t.Fatalf("initial state: %d seeded keys, want 3", n)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO settings (key, value) VALUES ('jira_updates_last_ok_at', '2026-09-29T06:00:00Z')
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`); err != nil {
		t.Fatalf("seed runtime key: %v", err)
	}
	if _, err := provider.ApplyVersion(ctx, jiraUpdatesMigrationVersion, false); err != nil {
		t.Fatalf("apply jira_updates down: %v", err)
	}
	if v, err := provider.GetDBVersion(ctx); err != nil || v != initialVersion {
		t.Fatalf("DB version after down = %d (%v), want unchanged %d", v, err, initialVersion)
	}
	if n := anyLeft(); n != 0 {
		t.Fatalf("after down: %d jira_updates_ keys left, want 0", n)
	}
	up()
	if n := seeded(); n != 3 {
		t.Fatalf("after re-up: %d seeded keys, want 3", n)
	}
	if v, err := provider.GetDBVersion(ctx); err != nil || v != initialVersion {
		t.Fatalf("DB version after re-up = %d (%v), want unchanged %d", v, err, initialVersion)
	}
}
