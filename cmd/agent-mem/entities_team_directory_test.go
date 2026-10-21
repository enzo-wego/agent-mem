package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/config"
)

const teamDirFixture = `<html><script>
const KB = {"kb_teams":[{"team_id":"a","team_name":"Team A","mission":"m","owns":"o","source":"https://example.test/team-src"}],
"kb_people":[{"team_id":"a","name":"Fake Person","hr_name":"","role":"Eng","is_lead":"yes","slack_handle":"SENTINEL_HANDLE_7f3","hr_manager":"SENTINEL_HR_1"}],
"kb_directory":[{"name":"Fake Person","teams":"a","team_basis":"via manager (SENTINEL_MGR_9q1)","hr_title":"SENTINEL_TITLE"}],
"kb_change_rules":[{"rule":"SENTINEL_RULE_5","n":3}]};
render(KB);
</script></html>`

func TestBuildTeamDirectoryPayload_NoSentinels(t *testing.T) {
	var out bytes.Buffer
	payload, err := buildTeamDirectoryPayload([]byte(teamDirFixture), &out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(payload), `{"directory":{`) {
		t.Fatalf("payload shape: %.60s", payload)
	}
	for _, s := range []string{string(payload), out.String()} {
		if strings.Contains(s, "SENTINEL_HANDLE") || strings.Contains(s, "SENTINEL_HR") ||
			strings.Contains(s, "SENTINEL_MGR") || strings.Contains(s, "SENTINEL_TITLE") ||
			strings.Contains(s, "SENTINEL_RULE") {
			t.Fatalf("leak: %s", s)
		}
	}
	if strings.Contains(out.String(), "Fake Person") {
		t.Fatalf("CLI output names a person: %s", out.String())
	}
	if !strings.Contains(out.String(), "teams=1 people=1 directory=1") {
		t.Fatalf("counts line: %q", out.String())
	}
	if _, err := buildTeamDirectoryPayload([]byte("<html></html>"), &out); err == nil {
		t.Fatal("missing KB must fail")
	}
}

func TestImportTeamDirectoryCLI_EnqueuesSanitizedJob(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if pcfg.ConnConfig.Database != "agentmem_test" {
		t.Fatal("refusing to run: requires database agentmem_test")
	}
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	clean := func() { _, _ = pool.Exec(ctx, `DELETE FROM graph.jobs WHERE type='import_team_directory'`) }
	clean()
	t.Cleanup(clean)

	file := filepath.Join(t.TempDir(), "page.html")
	if err := os.WriteFile(file, []byte(teamDirFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newEntitiesCmd(func() *config.Config { return &config.Config{DatabaseURL: dsn, MachineID: "test"} })
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"import-team-directory", "--file", file})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v\n%s", err, out.String())
	}
	var payload string
	if err := pool.QueryRow(ctx, `SELECT payload::text FROM graph.jobs WHERE type='import_team_directory'`).Scan(&payload); err != nil {
		t.Fatalf("job row: %v", err)
	}
	if !strings.Contains(payload, `"directory"`) || strings.Contains(payload, "SENTINEL") || strings.Contains(payload, "hr_manager") || strings.Contains(payload, "hr_title") {
		t.Fatalf("payload: %s", payload)
	}
}
