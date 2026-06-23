package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
	"github.com/rs/zerolog"
)

func TestJanitor_RequeuesExpiredLeases(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool := testDB(t)

	// Insert a row in 'running' state with an already-expired lease.
	var id int64
	row := pool.QueryRow(ctx, `
		INSERT INTO graph.jobs
		  (type, payload, priority, status, locked_by, locked_at, lease_until,
		   attempts, max_attempts, machine_id)
		VALUES ('fetch_body', '{}', 0, 'running', 'dead-worker',
		        NOW() - INTERVAL '5 minutes',
		        NOW() - INTERVAL '4 minutes',
		        1, 5, 'm-test')
		RETURNING id
	`)
	must(t, row.Scan(&id))

	j := jobs.NewJanitor(jobs.JanitorConfig{
		DB:           pool,
		ScanInterval: 100 * time.Millisecond,
		BatchSize:    100,
		Logger:       zerolog.Nop(),
	})
	go j.Run(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		pool.QueryRow(ctx, `SELECT status FROM graph.jobs WHERE id=$1`, id).Scan(&status)
		if status == "queued" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("janitor did not requeue expired job within 2s")
}

func TestJanitor_LeavesFreshLeasesAlone(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	pool := testDB(t)

	var id int64
	row := pool.QueryRow(ctx, `
		INSERT INTO graph.jobs
		  (type, payload, priority, status, locked_by, locked_at, lease_until,
		   attempts, max_attempts, machine_id)
		VALUES ('fetch_body', '{}', 0, 'running', 'live-worker',
		        NOW(), NOW() + INTERVAL '5 minutes',
		        1, 5, 'm-test')
		RETURNING id
	`)
	must(t, row.Scan(&id))

	j := jobs.NewJanitor(jobs.JanitorConfig{
		DB:           pool,
		ScanInterval: 100 * time.Millisecond,
		BatchSize:    100,
		Logger:       zerolog.Nop(),
	})
	go j.Run(ctx)
	time.Sleep(500 * time.Millisecond)

	var status string
	pool.QueryRow(ctx, `SELECT status FROM graph.jobs WHERE id=$1`, id).Scan(&status)
	if status != "running" {
		t.Errorf("janitor disturbed a live job: status=%q", status)
	}
}
