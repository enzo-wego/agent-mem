package jobs_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestCleanupPeriodicJobs(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Fatal("DATABASE_URL is required for periodic cleanup integration tests")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConnConfig.Host != "127.0.0.1" || cfg.ConnConfig.Port != 5446 || cfg.ConnConfig.Database != "agentmem_test" {
		t.Fatal("periodic cleanup tests require the isolated 127.0.0.1:5446/agentmem_test database")
	}
	pool := openTestDB(t)
	ctx := context.Background()
	types := []string{"notify_watch_channels", "detect_hot_topics", "derive_person_roles", "refresh_jira_board"}
	keys := []string{"graph.periodic.future_job.last_enqueued_at"}
	for _, typ := range types {
		keys = append(keys, "graph.periodic."+typ+".last_enqueued_at")
	}
	previous := make(map[string]string)
	for _, key := range keys {
		var value string
		if err := pool.QueryRow(ctx, `SELECT value FROM public.settings WHERE key=$1`, key).Scan(&value); err == nil {
			previous[key] = value
		}
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `DELETE FROM graph.jobs`); err != nil {
			t.Error(err)
		}
		for _, key := range keys {
			if value, ok := previous[key]; ok {
				_, err := pool.Exec(ctx, `INSERT INTO public.settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`, key, value)
				if err != nil {
					t.Error(err)
				}
			} else if _, err := pool.Exec(ctx, `DELETE FROM public.settings WHERE key=$1`, key); err != nil {
				t.Error(err)
			}
		}
	})
	for _, tc := range []struct {
		name     string
		expected int
		success  bool
	}{
		{"matching_count", 4, true}, {"wrong_count", 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			truncateJobsTable(t, pool)
			for _, typ := range types {
				if _, err := jobs.Enqueue(ctx, pool, typ, map[string]any{}, jobs.EnqueueOptions{MachineID: "cleanup-test"}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := pool.Exec(ctx, `INSERT INTO graph.jobs(type,payload,machine_id,status) VALUES('refresh_jira_board','{}','cleanup-test','running'),('backfill_slack_thread','{}','cleanup-test','queued')`); err != nil {
				t.Fatal(err)
			}
			for _, key := range keys {
				if _, err := pool.Exec(ctx, `INSERT INTO public.settings(key,value) VALUES($1,'2026-10-05T00:00:00Z') ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`, key); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("psql", dsn, "-X", "-v", fmt.Sprintf("expected=%d", tc.expected), "-f", filepath.Join("..", "..", "..", "scripts", "cleanup-periodic-jobs.sql"))
			out, err := cmd.CombinedOutput()
			if tc.success && err != nil {
				t.Fatalf("matching cleanup failed: %v\n%s", err, out)
			}
			if !tc.success && (err == nil || !strings.Contains(string(out), "deleted 4 rows, expected 3")) {
				t.Fatalf("wrong count did not raise guard: %v\n%s", err, out)
			}
			var queued, running, other, schedules, future int
			if err := pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE type=ANY($1) AND status='queued'),count(*) FILTER(WHERE type=ANY($1) AND status='running'),count(*) FILTER(WHERE type='backfill_slack_thread') FROM graph.jobs`, types).Scan(&queued, &running, &other); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE key=ANY($1)),count(*) FILTER(WHERE key=$2) FROM public.settings`, keys[1:], keys[0]).Scan(&schedules, &future); err != nil {
				t.Fatal(err)
			}
			wantQueued, wantSchedules := 4, 4
			if tc.success {
				wantQueued, wantSchedules = 0, 0
			}
			if queued != wantQueued || schedules != wantSchedules || running != 1 || other != 1 || future != 1 {
				t.Fatalf("cleanup results queued=%d schedules=%d running=%d continuation=%d future-setting=%d", queued, schedules, running, other, future)
			}
			if !tc.success {
				for _, key := range keys {
					var value string
					if err := pool.QueryRow(ctx, `SELECT value FROM public.settings WHERE key=$1`, key).Scan(&value); err != nil || value != "2026-10-05T00:00:00Z" {
						t.Fatalf("rollback setting %s = %q, %v", key, value, err)
					}
				}
			}
		})
	}
}
