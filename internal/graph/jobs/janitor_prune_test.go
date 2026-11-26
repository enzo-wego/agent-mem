package jobs

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

func TestJanitorPrune_DeletesOnlyOldDoneJobs(t *testing.T) {
	dsn := os.Getenv("AGENT_MEM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AGENT_MEM_TEST_DATABASE_URL not set")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConnConfig.Database != "agentmem_test" {
		t.Fatalf("refusing database %q; require agentmem_test", cfg.ConnConfig.Database)
	}
	ctx := context.Background()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	clear := func() {
		if _, err := pool.Exec(ctx, `DELETE FROM graph.jobs`); err != nil {
			t.Fatal(err)
		}
	}
	clear()
	t.Cleanup(clear)

	seeds := []struct {
		typ, status, age string
		kept             bool
	}{
		{"summarize_thread", "done", "40 days", false},
		{"summarize_thread", "done", "5 days", true},
		{"summarize_thread", "failed", "40 days", true},
		{"fetch_body", "done", "40 days", true},
		{"describe_attachment", "done", "40 days", true},
	}
	ids := make([]int64, len(seeds))
	for i, s := range seeds {
		err := pool.QueryRow(ctx, `INSERT INTO graph.jobs(type,payload,status,completed_at,machine_id)
			VALUES($1,'{}',$2,NOW()-$3::interval,'m-test') RETURNING id`, s.typ, s.status, s.age).Scan(&ids[i])
		if err != nil {
			t.Fatal(err)
		}
	}

	n, err := NewJanitor(JanitorConfig{DB: pool, Logger: zerolog.Nop()}).prune(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("deleted %d rows, want 1", n)
	}
	for i, s := range seeds {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM graph.jobs WHERE id=$1)`, ids[i]).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists != s.kept {
			t.Errorf("%s/%s/%s: exists=%v, want %v", s.typ, s.status, s.age, exists, s.kept)
		}
	}
}
