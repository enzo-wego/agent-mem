package handlers

// R1a acceptance item 10: the two lead-mode migrations apply, roll back one at
// a time and re-apply, on the scratch database itself. Every test restores the
// fully migrated state in t.Cleanup, also when it fails half way.

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

const (
	leadCIndexName = "graph_nodes_thread_key_idx"
	leadCBotSeed   = "U0AG1DJP9K9"
)

type leadCMigrations struct {
	provider           *goose.Provider
	botsVer, indexVer  int64
	botsFile, indexSQL string
}

func leadCMigrationSetup(t *testing.T) (*leadCMigrations, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	openTestDB(t) // skips without DATABASE_URL and refuses a non-scratch database
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	provider, err := goose.NewProvider(goose.DialectPostgres, db, os.DirFS(migrationsDirFromHandlers), goose.WithAllowOutofOrder(true))
	if err != nil {
		t.Fatal(err)
	}
	// Registered before anything is rolled back: whatever happens, end fully migrated.
	t.Cleanup(func() {
		if _, err := provider.Up(ctx); err != nil {
			t.Errorf("restore migrations: %v", err)
		}
	})
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply all: %v", err)
	}
	m := &leadCMigrations{provider: provider}
	for _, s := range provider.ListSources() {
		base := filepath.Base(s.Path)
		switch {
		case strings.HasSuffix(base, "_graph_answer_bots.sql"):
			m.botsVer = s.Version
			b, err := os.ReadFile(filepath.Join(migrationsDirFromHandlers, base))
			if err != nil {
				t.Fatal(err)
			}
			m.botsFile = string(b)
		case strings.HasSuffix(base, "_graph_nodes_thread_key_idx.sql"):
			m.indexVer = s.Version
			b, err := os.ReadFile(filepath.Join(migrationsDirFromHandlers, base))
			if err != nil {
				t.Fatal(err)
			}
			m.indexSQL = string(b)
		}
	}
	if m.botsVer == 0 || m.indexVer == 0 {
		t.Fatalf("lead migrations not found (answer_bots=%d, thread_key_idx=%d)", m.botsVer, m.indexVer)
	}
	if m.indexVer <= m.botsVer {
		t.Fatalf("thread_key_idx (%d) must be after answer_bots (%d)", m.indexVer, m.botsVer)
	}
	return m, db
}

func (m *leadCMigrations) state(t *testing.T, version int64) goose.State {
	t.Helper()
	statuses, err := m.provider.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range statuses {
		if s.Source.Version == version {
			return s.State
		}
	}
	t.Fatalf("version %d not listed", version)
	return ""
}

// recorded reads goose_db_version directly: the latest row of a version says
// whether it is applied.
func leadCRecorded(t *testing.T, db *sql.DB, version int64) (applied, present bool) {
	t.Helper()
	err := db.QueryRow(`SELECT is_applied FROM goose_db_version WHERE version_id = $1 ORDER BY id DESC LIMIT 1`, version).Scan(&applied)
	if err == sql.ErrNoRows {
		return false, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return applied, true
}

type leadCIndexInfo struct {
	exists, valid, ready, partial bool
	def                           string
}

func leadCIndexState(t *testing.T, db *sql.DB) leadCIndexInfo {
	t.Helper()
	var i leadCIndexInfo
	err := db.QueryRow(`
SELECT true, x.indisvalid, x.indisready, x.indpred IS NOT NULL, pg_get_indexdef(x.indexrelid)
FROM pg_index x
JOIN pg_class c ON c.oid = x.indexrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'graph' AND c.relname = $1`, leadCIndexName).Scan(&i.exists, &i.valid, &i.ready, &i.partial, &i.def)
	if err == sql.ErrNoRows {
		return leadCIndexInfo{}
	}
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func leadCBots(t *testing.T, db *sql.DB) (table bool, seedRows int, seedNote string, total int) {
	t.Helper()
	if err := db.QueryRow(`SELECT to_regclass('graph.answer_bots') IS NOT NULL`).Scan(&table); err != nil {
		t.Fatal(err)
	}
	if !table {
		return false, 0, "", 0
	}
	if err := db.QueryRow(`SELECT count(*), COALESCE(max(note), ''), (SELECT count(*) FROM graph.answer_bots) FROM graph.answer_bots WHERE slack_user_id = $1`, leadCBotSeed).Scan(&seedRows, &seedNote, &total); err != nil {
		t.Fatal(err)
	}
	return true, seedRows, seedNote, total
}

func (m *leadCMigrations) assertIndexBack(t *testing.T, db *sql.DB) {
	t.Helper()
	i := leadCIndexState(t, db)
	if !i.exists || !i.valid || !i.ready || !i.partial {
		t.Fatalf("index after re-apply: %+v, want present, valid, ready and partial", i)
	}
	if !strings.Contains(i.def, "(scope, COALESCE(NULLIF((metadata ->> 'thread_ts'::text), ''::text), split_part(id, ':'::text, 3)))") ||
		!strings.Contains(i.def, "deleted_at IS NULL") {
		t.Errorf("index definition drifted: %s", i.def)
	}
}

func TestLeadC_MigrationRollbackLastKeepsAnswerBots(t *testing.T) {
	ctx := context.Background()
	m, db := leadCMigrationSetup(t)

	if st := m.state(t, m.botsVer); st != goose.StateApplied {
		t.Fatalf("answer_bots state = %s after apply-all", st)
	}
	if st := m.state(t, m.indexVer); st != goose.StateApplied {
		t.Fatalf("thread_key_idx state = %s after apply-all", st)
	}
	m.assertIndexBack(t, db)
	table, seed, note, _ := leadCBots(t, db)
	if !table || seed != 1 || note != "EnzoBot" {
		t.Fatalf("after apply-all: table=%v seed rows=%d note=%q, want the single EnzoBot seed", table, seed, note)
	}
	// A marker row proves the table itself survives (not just a re-seeded copy).
	if _, err := db.Exec(`INSERT INTO graph.answer_bots (slack_user_id, note) VALUES ('ULCMIGMARK','leadC marker')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(`DELETE FROM graph.answer_bots WHERE slack_user_id = 'ULCMIGMARK'`); err != nil {
			t.Errorf("remove marker: %v", err)
		}
	})

	// Roll back only the newest migration.
	res, err := m.provider.ApplyVersion(ctx, m.indexVer, false)
	if err != nil {
		t.Fatalf("down thread_key_idx: %v", err)
	}
	if res.Source.Version != m.indexVer {
		t.Fatalf("rolled back version %d, want %d", res.Source.Version, m.indexVer)
	}
	if i := leadCIndexState(t, db); i.exists {
		t.Errorf("index still present after down: %+v", i)
	}
	if st := m.state(t, m.indexVer); st != goose.StatePending {
		t.Errorf("thread_key_idx state = %s after down, want pending", st)
	}
	if applied, _ := leadCRecorded(t, db, m.indexVer); applied {
		t.Errorf("goose_db_version still lists thread_key_idx as applied")
	}
	if st := m.state(t, m.botsVer); st != goose.StateApplied {
		t.Errorf("answer_bots state = %s after rolling back the newer migration, want applied", st)
	}
	if applied, present := leadCRecorded(t, db, m.botsVer); !applied || !present {
		t.Errorf("goose_db_version lost the answer_bots migration (applied=%v present=%v)", applied, present)
	}
	table, seed, note, _ = leadCBots(t, db)
	if !table || seed != 1 || note != "EnzoBot" {
		t.Errorf("answer_bots after rolling back only the index: table=%v seed rows=%d note=%q", table, seed, note)
	}
	var marker int
	if err := db.QueryRow(`SELECT count(*) FROM graph.answer_bots WHERE slack_user_id = 'ULCMIGMARK'`).Scan(&marker); err != nil || marker != 1 {
		t.Errorf("answer_bots marker row = %d (%v), want it kept", marker, err)
	}

	// Re-apply.
	res, err = m.provider.ApplyVersion(ctx, m.indexVer, true)
	if err != nil {
		t.Fatalf("re-apply thread_key_idx: %v", err)
	}
	if res.Source.Version != m.indexVer {
		t.Fatalf("re-applied version %d, want %d", res.Source.Version, m.indexVer)
	}
	m.assertIndexBack(t, db)
	if applied, _ := leadCRecorded(t, db, m.indexVer); !applied {
		t.Errorf("goose_db_version does not list thread_key_idx as applied after re-apply")
	}
	if table, seed, _, _ := leadCBots(t, db); !table || seed != 1 {
		t.Errorf("answer_bots seed after re-apply: table=%v rows=%d", table, seed)
	}
	if st := m.state(t, m.indexVer); st != goose.StateApplied {
		t.Errorf("thread_key_idx state = %s after re-apply", st)
	}
}

func TestLeadC_MigrationRollbackBothAndReapply(t *testing.T) {
	ctx := context.Background()
	m, db := leadCMigrationSetup(t)

	for _, v := range []int64{m.indexVer, m.botsVer} {
		if _, err := m.provider.ApplyVersion(ctx, v, false); err != nil {
			t.Fatalf("down %d: %v", v, err)
		}
	}
	if table, _, _, _ := leadCBots(t, db); table {
		t.Errorf("answer_bots survived its own down migration")
	}
	if i := leadCIndexState(t, db); i.exists {
		t.Errorf("index survived its down migration")
	}
	for _, v := range []int64{m.botsVer, m.indexVer} {
		if st := m.state(t, v); st != goose.StatePending {
			t.Errorf("version %d state = %s after down, want pending", v, st)
		}
	}
	// Applying everything (the order is by version) recreates both, seed once.
	if _, err := m.provider.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	m.assertIndexBack(t, db)
	table, seed, note, total := leadCBots(t, db)
	if !table || seed != 1 || note != "EnzoBot" || total != 1 {
		t.Errorf("after up: table=%v seed rows=%d note=%q total=%d, want exactly the seed", table, seed, note, total)
	}
	for _, v := range []int64{m.botsVer, m.indexVer} {
		if applied, _ := leadCRecorded(t, db, v); !applied {
			t.Errorf("version %d not recorded as applied", v)
		}
	}
}

// goose.Up sections of a migration file, without the Down half.
func leadCUpSection(t *testing.T, file string) string {
	t.Helper()
	up, _, ok := strings.Cut(file, "-- +goose Down")
	if !ok {
		t.Fatal("migration has no Down section")
	}
	return up
}

func TestLeadC_MigrationUpIsIdempotent(t *testing.T) {
	ctx := context.Background()
	m, db := leadCMigrationSetup(t)

	// answer_bots: running the Up SQL again neither duplicates nor overwrites the seed.
	upBots := leadCUpSection(t, m.botsFile)
	_, _, _, before := leadCBots(t, db)
	for range 2 {
		if _, err := db.ExecContext(ctx, upBots); err != nil {
			t.Fatalf("re-run answer_bots up: %v", err)
		}
	}
	table, seed, note, after := leadCBots(t, db)
	if !table || seed != 1 || note != "EnzoBot" || after != before {
		t.Errorf("answer_bots after re-running up: table=%v seed rows=%d note=%q total %d -> %d", table, seed, note, before, after)
	}
	// An operator edit of the seed note survives a re-run (ON CONFLICT DO NOTHING).
	if _, err := db.Exec(`UPDATE graph.answer_bots SET note = 'edited by leadC' WHERE slack_user_id = $1`, leadCBotSeed); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.Exec(`UPDATE graph.answer_bots SET note = 'EnzoBot' WHERE slack_user_id = $1`, leadCBotSeed); err != nil {
			t.Errorf("restore seed note: %v", err)
		}
	})
	if _, err := db.ExecContext(ctx, upBots); err != nil {
		t.Fatal(err)
	}
	if _, _, note, _ := leadCBots(t, db); note != "edited by leadC" {
		t.Errorf("re-running up overwrote the seed note: %q", note)
	}

	// The index migration's SQL is IF NOT EXISTS: no error, still one valid index.
	upIdx := leadCUpSection(t, m.indexSQL)
	if _, err := db.ExecContext(ctx, upIdx); err != nil {
		t.Fatalf("re-run thread_key_idx up: %v", err)
	}
	m.assertIndexBack(t, db)
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM pg_indexes WHERE schemaname = 'graph' AND tablename = 'nodes' AND indexname = $1`, leadCIndexName).Scan(&n); err != nil || n != 1 {
		t.Errorf("index count = %d (%v), want 1", n, err)
	}
	// Down of the absent index is a no-op too.
	_, downIdx, ok := strings.Cut(m.indexSQL, "-- +goose Down")
	if !ok {
		t.Fatal("thread_key_idx has no Down section")
	}
	if _, err := m.provider.ApplyVersion(ctx, m.indexVer, false); err != nil {
		t.Fatalf("down: %v", err)
	}
	if _, err := db.ExecContext(ctx, downIdx); err != nil {
		t.Errorf("down SQL on an absent index: %v", err)
	}
	if _, err := m.provider.ApplyVersion(ctx, m.indexVer, true); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	m.assertIndexBack(t, db)
}
