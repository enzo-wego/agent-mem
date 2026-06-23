package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
	"github.com/rs/zerolog"
)

func TestHeartbeat_ExtendsLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool := testDB(t)

	id := mustEnqueue(t, pool, "refresh_slack_groups", []byte(`{}`), 0)
	_, err := jobs.Claim(ctx, pool, "refresh_slack_groups", 2*time.Second, "w-1", "vps")
	must(t, err)

	hb := jobs.StartHeartbeat(ctx, pool, id, 2*time.Second, zerolog.Nop())
	defer hb.Stop()

	time.Sleep(2500 * time.Millisecond) // > initial lease

	var leaseUntil time.Time
	row := pool.QueryRow(ctx, `SELECT lease_until FROM graph.jobs WHERE id=$1`, id)
	if err := row.Scan(&leaseUntil); err != nil {
		t.Fatal(err)
	}
	if leaseUntil.Before(time.Now()) {
		t.Errorf("lease should have been extended; got %v", leaseUntil)
	}
}

func TestHeartbeat_StopsOnSignal(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	id := mustEnqueue(t, pool, "refresh_slack_groups", []byte(`{}`), 0)
	_, err := jobs.Claim(ctx, pool, "refresh_slack_groups", 5*time.Second, "w-1", "vps")
	must(t, err)

	hb := jobs.StartHeartbeat(ctx, pool, id, 5*time.Second, zerolog.Nop())
	hb.Stop()
	// No assertion needed beyond "doesn't hang" — test timeout proves it.
}
