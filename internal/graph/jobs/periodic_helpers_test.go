package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestPeriodicHelpersTimeoutAndRecovery(t *testing.T) {
	ctx := context.Background()
	for _, helper := range []string{"enqueue", "retry"} {
		for _, blocked := range []string{"pool", "advisory_lock"} {
			t.Run(helper+"/"+blocked, func(t *testing.T) {
				maxConns := int32(1)
				if blocked == "advisory_lock" {
					maxConns = 2
				}
				pool := periodicDB(t, maxConns)
				periodicReset(t, pool)
				typ := "notify_watch_channels"
				var id int64
				if helper == "retry" {
					var err error
					id, err = EnqueuePeriodicNow(ctx, pool, typ, "machine", "vps")
					periodicMust(t, err)
					periodicMust(t, Fail(ctx, pool, id, errors.New("original failure")))
				}
				conn, err := pool.Acquire(ctx)
				periodicMust(t, err)
				defer conn.Release()
				var tx pgx.Tx
				if blocked == "advisory_lock" {
					tx, err = conn.Begin(ctx)
					periodicMust(t, err)
					defer tx.Rollback(ctx)
					periodicExec(t, tx, `SELECT pg_advisory_xact_lock(hashtext('periodic:' || $1))`, typ)
				}
				call := func() error {
					if helper == "retry" {
						return RetryPeriodicNow(ctx, pool, id)
					}
					_, err := EnqueuePeriodicNow(ctx, pool, typ, "machine", "vps")
					return err
				}
				start := time.Now()
				err = call()
				elapsed := time.Since(start)
				if err == nil {
					t.Fatal("blocked helper succeeded")
				}
				if elapsed < 4*time.Second || elapsed > 8*time.Second {
					t.Fatalf("whole-operation timeout elapsed=%s: %v", elapsed, err)
				}
				if tx != nil {
					periodicMust(t, tx.Rollback(ctx))
				}
				conn.Release()
				if n := periodicCount(t, pool, typ); n != 0 {
					t.Fatalf("timed-out helper left pending=%d", n)
				}
				if helper == "retry" {
					var status, lastError string
					periodicMust(t, pool.QueryRow(ctx, `SELECT status, last_error FROM graph.jobs WHERE id=$1`, id).Scan(&status, &lastError))
					if status != "failed" || lastError != "original failure" {
						t.Fatal("timed-out retry changed failed row")
					}
				}
				periodicMust(t, call())
				if n := periodicCount(t, pool, typ); n != 1 {
					t.Fatalf("helper failed to recover: pending=%d", n)
				}
				if periodicSetting(t, pool, typ) != "" {
					t.Fatal("helper timeout/recovery changed schedule")
				}
				if pool.Stat().AcquiredConns() != 0 {
					t.Fatal("helper leaked a pooled connection")
				}
			})
		}
	}
}
