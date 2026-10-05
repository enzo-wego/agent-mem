package jobs

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"os"
	"testing"
)

func TestJanitorScan_NullLeaseGrace(t *testing.T) {
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
	if _, err := pool.Exec(ctx, `DELETE FROM graph.jobs`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `DELETE FROM graph.jobs`); err != nil {
			t.Error(err)
		}
	})
	var old, fresh int64
	for _, seed := range []struct {
		age string
		id  *int64
	}{{"1 hour", &old}, {"1 minute", &fresh}} {
		err := pool.QueryRow(ctx, `INSERT INTO graph.jobs(type,payload,status,locked_by,locked_at,lease_until,attempts,max_attempts,machine_id) VALUES('fetch_body','{}','running','abandoned',NOW()-$1::interval,NULL,1,5,'m-test') RETURNING id`, seed.age).Scan(seed.id)
		if err != nil {
			t.Fatal(err)
		}
	}
	j := NewJanitor(JanitorConfig{DB: pool, Logger: zerolog.Nop()})
	reclaimed, err := j.scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if reclaimed != 1 {
		t.Errorf("reclaimed=%d, want 1", reclaimed)
	}
	for _, want := range []struct {
		id      int64
		status  string
		cleared bool
	}{{old, "queued", true}, {fresh, "running", false}} {
		var status string
		var cleared bool
		var attempts int
		err := pool.QueryRow(ctx, `SELECT status,locked_by IS NULL AND locked_at IS NULL AND lease_until IS NULL,attempts FROM graph.jobs WHERE id=$1`, want.id).Scan(&status, &cleared, &attempts)
		if err != nil {
			t.Fatal(err)
		}
		if status != want.status || cleared != want.cleared || attempts != 1 {
			t.Errorf("job %d: status=%s cleared=%v attempts=%d, want %s/%v/1", want.id, status, cleared, attempts, want.status, want.cleared)
		}
	}
}
