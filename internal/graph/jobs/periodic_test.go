package jobs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

var periodicCases = []struct {
	typ      string
	interval time.Duration
}{
	{"notify_watch_channels", 5 * time.Minute},
	{"detect_hot_topics", 5 * time.Minute},
	{"derive_person_roles", 24 * time.Hour},
	{"refresh_jira_board", 6 * time.Hour},
}

func periodicDB(t *testing.T, maxConns int32) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConnConfig.Database != "agentmem_test" {
		t.Fatal("periodic tests require agentmem_test database")
	}
	cfg.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	return pool
}

func periodicReset(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	periodicExec(t, pool, `DELETE FROM graph.jobs`)
	periodicExec(t, pool, `DELETE FROM settings WHERE key LIKE 'graph.periodic.%.last_enqueued_at'`)
}

func periodicExec(t *testing.T, db DB, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func periodicCount(t *testing.T, pool *pgxpool.Pool, typ string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM graph.jobs WHERE type=$1 AND status IN ('queued','running')`, typ).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func periodicSetting(t *testing.T, pool *pgxpool.Pool, typ string) string {
	t.Helper()
	var value string
	err := pool.QueryRow(context.Background(), `SELECT value FROM settings WHERE key=$1`, "graph.periodic."+typ+".last_enqueued_at").Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func periodicID(t *testing.T, pool *pgxpool.Pool, typ string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `SELECT id FROM graph.jobs WHERE type=$1 ORDER BY id DESC LIMIT 1`, typ).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func periodicMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestPeriodicTicker(t *testing.T) {
	pool := periodicDB(t, 16)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, tc := range periodicCases {
		t.Run(tc.typ, func(t *testing.T) {
			periodicReset(t, pool)
			PeriodicTick(ctx, pool, "ticker-machine", "vps", now, zerolog.Nop())
			if n := periodicCount(t, pool, tc.typ); n != 1 {
				t.Fatalf("initial pending=%d, want 1", n)
			}
			if got := periodicSetting(t, pool, tc.typ); got != now.Format(time.RFC3339Nano) {
				t.Fatalf("setting=%q, want %q", got, now.Format(time.RFC3339Nano))
			}
			id := periodicID(t, pool, tc.typ)
			var payload, runner, machine string
			var priority int16
			var available time.Time
			periodicMust(t, pool.QueryRow(ctx, `SELECT payload::text, priority, available_at, target_runner, machine_id FROM graph.jobs WHERE id=$1`, id).Scan(&payload, &priority, &available, &runner, &machine))
			wantRunner := "vps"
			if tc.typ == "derive_person_roles" {
				wantRunner = "any"
			}
			if payload != "{}" || priority != 5 || !available.Equal(now) || runner != wantRunner || machine != "ticker-machine" {
				t.Fatalf("unexpected row: payload=%s priority=%d available=%s runner=%s machine=%s", payload, priority, available, runner, machine)
			}
			for _, status := range []string{"running", "queued"} {
				periodicExec(t, pool, `UPDATE graph.jobs SET status=$2 WHERE id=$1`, id, status)
				PeriodicTick(ctx, pool, "another-machine", "local", now.Add(2*tc.interval), zerolog.Nop())
				if periodicCount(t, pool, tc.typ) != 1 || periodicSetting(t, pool, tc.typ) != now.Format(time.RFC3339Nano) {
					t.Fatalf("%s should skip pending and preserve setting", status)
				}
			}
			periodicMust(t, Complete(ctx, pool, id))
			PeriodicTick(ctx, pool, "ticker-machine", "vps", now.Add(tc.interval-time.Nanosecond), zerolog.Nop())
			if n := periodicCount(t, pool, tc.typ); n != 0 {
				t.Fatalf("within interval pending=%d", n)
			}
			PeriodicTick(ctx, pool, "ticker-machine", "vps", now.Add(tc.interval), zerolog.Nop())
			if n := periodicCount(t, pool, tc.typ); n != 1 {
				t.Fatalf("at interval pending=%d", n)
			}
			periodicMust(t, Complete(ctx, pool, periodicID(t, pool, tc.typ)))
			// A fresh pool models a restarted scheduler: cadence lives in PostgreSQL.
			restarted := periodicDB(t, 4)
			PeriodicTick(ctx, restarted, "restart-machine", "local", now.Add(tc.interval+time.Minute), zerolog.Nop())
			if n := periodicCount(t, pool, tc.typ); n != 0 {
				t.Fatalf("restart reset cadence: pending=%d", n)
			}
			PeriodicTick(ctx, restarted, "restart-machine", "local", now.Add(2*tc.interval+time.Second), zerolog.Nop())
			if n := periodicCount(t, pool, tc.typ); n != 1 {
				t.Fatalf("past interval pending=%d", n)
			}
		})
	}
}

func TestPeriodicTickerConcurrent(t *testing.T) {
	pool := periodicDB(t, 16)
	periodicReset(t, pool)
	now := time.Now()
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			PeriodicTick(context.Background(), pool, "machine", "vps", now, zerolog.Nop())
		}()
	}
	wg.Wait()
	for _, tc := range periodicCases {
		if n := periodicCount(t, pool, tc.typ); n != 1 {
			t.Fatalf("%s pending=%d", tc.typ, n)
		}
		if periodicSetting(t, pool, tc.typ) == "" {
			t.Fatalf("%s missing persisted cadence", tc.typ)
		}
	}
}

func TestPeriodicTickerRollback(t *testing.T) {
	pool := periodicDB(t, 4)
	for _, phase := range []string{"insert", "setting_insert", "setting_update"} {
		t.Run(phase, func(t *testing.T) {
			periodicReset(t, pool)
			before := ""
			if phase == "setting_update" {
				before = time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339Nano)
				periodicExec(t, pool, `INSERT INTO settings (key,value) VALUES ('graph.periodic.notify_watch_channels.last_enqueued_at',$1)`, before)
			}
			periodicExec(t, pool, `CREATE OR REPLACE FUNCTION graph.periodic_test_reject() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'periodic injected failure'; END $$`)
			t.Cleanup(func() { periodicExec(t, pool, `DROP FUNCTION IF EXISTS graph.periodic_test_reject() CASCADE`) })
			if phase == "insert" {
				periodicExec(t, pool, `CREATE TRIGGER periodic_test_failure BEFORE INSERT ON graph.jobs FOR EACH ROW WHEN (NEW.type='notify_watch_channels') EXECUTE FUNCTION graph.periodic_test_reject()`)
			} else if phase == "setting_update" {
				periodicExec(t, pool, `CREATE TRIGGER periodic_test_failure BEFORE UPDATE ON settings FOR EACH ROW WHEN (NEW.key='graph.periodic.notify_watch_channels.last_enqueued_at') EXECUTE FUNCTION graph.periodic_test_reject()`)
			} else {
				periodicExec(t, pool, `CREATE TRIGGER periodic_test_failure BEFORE INSERT OR UPDATE ON settings FOR EACH ROW WHEN (NEW.key='graph.periodic.notify_watch_channels.last_enqueued_at') EXECUTE FUNCTION graph.periodic_test_reject()`)
			}
			var log bytes.Buffer
			PeriodicTick(context.Background(), pool, "machine", "vps", time.Now(), zerolog.New(&log))
			if n := periodicCount(t, pool, "notify_watch_channels"); n != 0 {
				t.Fatalf("failed %s left pending=%d", phase, n)
			}
			if got := periodicSetting(t, pool, "notify_watch_channels"); got != before {
				t.Fatalf("failed %s changed setting=%q, want %q", phase, got, before)
			}
			if !strings.Contains(log.String(), "periodic injected failure") {
				t.Fatalf("missing failure log: %s", log.String())
			}
			if periodicCount(t, pool, "detect_hot_topics") != 1 {
				t.Fatal("failure stopped subsequent types")
			}
		})
	}
}

func TestPeriodicTickerLockTimeoutContinues(t *testing.T) {
	pool := periodicDB(t, 4)
	periodicReset(t, pool)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	periodicMust(t, err)
	defer tx.Rollback(ctx)
	periodicExec(t, tx, `SELECT pg_advisory_xact_lock(hashtext('periodic:' || $1))`, "notify_watch_channels")
	var log bytes.Buffer
	start := time.Now()
	PeriodicTick(ctx, pool, "machine", "vps", start, zerolog.New(&log))
	if elapsed := time.Since(start); elapsed < 4*time.Second || elapsed > 8*time.Second {
		t.Fatalf("lock timeout elapsed=%s", elapsed)
	}
	if periodicCount(t, pool, "notify_watch_channels") != 0 || periodicCount(t, pool, "detect_hot_topics") != 1 {
		t.Fatal("blocked type should time out while later types continue")
	}
	if !strings.Contains(log.String(), "notify_watch_channels") || !strings.Contains(log.String(), "error") {
		t.Fatalf("missing type timeout log: %s", log.String())
	}
	periodicMust(t, tx.Rollback(ctx))
	PeriodicTick(ctx, pool, "machine", "vps", start, zerolog.Nop())
	if periodicCount(t, pool, "notify_watch_channels") != 1 {
		t.Fatal("next tick failed to recover after lock release")
	}
}

func TestPeriodicTickerPoolTimeoutRecovery(t *testing.T) {
	pool := periodicDB(t, 1)
	periodicReset(t, pool)
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	periodicMust(t, err)
	defer conn.Release()
	var log bytes.Buffer
	start := time.Now()
	PeriodicTick(ctx, pool, "machine", "vps", start, zerolog.New(&log))
	elapsed := time.Since(start)
	if elapsed < 18*time.Second || elapsed > 25*time.Second {
		t.Fatalf("four bounded acquire operations elapsed=%s", elapsed)
	}
	if n := strings.Count(log.String(), "\"level\":\"error\""); n != 4 {
		t.Fatalf("error logs=%d, want four: %s", n, log.String())
	}
	conn.Release()
	PeriodicTick(ctx, pool, "machine", "vps", start, zerolog.Nop())
	for _, tc := range periodicCases {
		if periodicCount(t, pool, tc.typ) != 1 {
			t.Fatalf("%s failed recovery", tc.typ)
		}
	}
	if pool.Stat().AcquiredConns() != 0 {
		t.Fatalf("connection leak: %d acquired", pool.Stat().AcquiredConns())
	}
}

func TestPeriodicManualEnqueue(t *testing.T) {
	pool := periodicDB(t, 16)
	ctx := context.Background()
	for _, tc := range periodicCases {
		t.Run(tc.typ, func(t *testing.T) {
			periodicReset(t, pool)
			if !IsPeriodic(tc.typ) {
				t.Fatal("allowlisted type not recognized")
			}
			id, err := EnqueuePeriodicNow(ctx, pool, tc.typ, "manual-machine", "")
			periodicMust(t, err)
			if periodicSetting(t, pool, tc.typ) != "" {
				t.Fatal("manual enqueue changed schedule")
			}
			var runner, machine, payload string
			var priority int16
			var available time.Time
			periodicMust(t, pool.QueryRow(ctx, `SELECT target_runner, machine_id, priority, payload::text, available_at FROM graph.jobs WHERE id=$1`, id).Scan(&runner, &machine, &priority, &payload, &available))
			if runner != "any" || machine != "manual-machine" || priority != 5 || payload != "{}" || time.Since(available).Abs() > 5*time.Second {
				t.Fatal("manual enqueue lost defaults")
			}
			for _, status := range []string{"queued", "running"} {
				periodicExec(t, pool, `UPDATE graph.jobs SET status=$2 WHERE id=$1`, id, status)
				_, err = EnqueuePeriodicNow(ctx, pool, tc.typ, "manual-machine", "vps")
				if !errors.Is(err, ErrPeriodicPending) || err.Error() != "already queued or running" {
					t.Fatalf("pending error=%v", err)
				}
			}
		})
	}
	if IsPeriodic("fetch_body") {
		t.Fatal("nonperiodic type allowlisted")
	}
	if _, err := EnqueuePeriodicNow(ctx, pool, "fetch_body", "machine", "vps"); err == nil {
		t.Fatal("manual helper accepted unknown type")
	}
}

func TestPeriodicTickManualRace(t *testing.T) {
	pool := periodicDB(t, 16)
	ctx := context.Background()
	for _, tc := range periodicCases {
		t.Run(tc.typ, func(t *testing.T) {
			periodicReset(t, pool)
			start := make(chan struct{})
			errCh := make(chan error, 1)
			done := make(chan struct{})
			go func() { <-start; PeriodicTick(ctx, pool, "ticker", "vps", time.Now(), zerolog.Nop()); close(done) }()
			go func() { <-start; _, err := EnqueuePeriodicNow(ctx, pool, tc.typ, "manual", "vps"); errCh <- err }()
			close(start)
			err := <-errCh
			if err != nil && !errors.Is(err, ErrPeriodicPending) {
				t.Fatal(err)
			}
			<-done
			if n := periodicCount(t, pool, tc.typ); n != 1 {
				t.Fatalf("pending=%d, want one", n)
			}
		})
	}
}

func TestRetryPeriodicNow(t *testing.T) {
	pool := periodicDB(t, 16)
	ctx := context.Background()
	for _, typ := range []string{"notify_watch_channels", "detect_hot_topics", "derive_person_roles", "refresh_jira_board", "fetch_body"} {
		t.Run(typ, func(t *testing.T) {
			periodicReset(t, pool)
			id, err := EnqueueRaw(ctx, pool, typ, []byte(`{"original":true}`), EnqueueOptions{MachineID: "origin", TargetRunner: "local"})
			periodicMust(t, err)
			periodicExec(t, pool, `UPDATE graph.jobs SET status='failed', attempts=3, last_error='old', available_at=NOW()+interval '1 day' WHERE id=$1`, id)
			periodicMust(t, RetryPeriodicNow(ctx, pool, id))
			var status, payload, machine, runner string
			var attempts int16
			var lastError *string
			var available time.Time
			periodicMust(t, pool.QueryRow(ctx, `SELECT status, attempts, last_error, available_at, payload::text, machine_id, target_runner FROM graph.jobs WHERE id=$1`, id).Scan(&status, &attempts, &lastError, &available, &payload, &machine, &runner))
			if status != "queued" || attempts != 0 || lastError != nil || time.Since(available).Abs() > 5*time.Second || machine != "origin" || runner != "local" || !strings.Contains(payload, "original") {
				t.Fatal("retry changed existing failed-only update semantics")
			}
			if periodicSetting(t, pool, typ) != "" {
				t.Fatal("manual retry changed schedule")
			}
			periodicExec(t, pool, `UPDATE graph.jobs SET attempts=2 WHERE id=$1`, id)
			periodicMust(t, RetryPeriodicNow(ctx, pool, id))
			periodicMust(t, pool.QueryRow(ctx, `SELECT attempts FROM graph.jobs WHERE id=$1`, id).Scan(&attempts))
			if attempts != 2 {
				t.Fatal("nonfailed retry changed row")
			}
			other, err := EnqueueRaw(ctx, pool, typ, []byte(`{}`), EnqueueOptions{MachineID: "other"})
			periodicMust(t, err)
			for _, pendingStatus := range []string{"queued", "running"} {
				periodicExec(t, pool, `UPDATE graph.jobs SET status='failed', attempts=3, last_error='old' WHERE id=$1`, id)
				periodicExec(t, pool, `UPDATE graph.jobs SET status=$2 WHERE id=$1`, other, pendingStatus)
				err = RetryPeriodicNow(ctx, pool, id)
				if IsPeriodic(typ) {
					if !errors.Is(err, ErrPeriodicPending) {
						t.Fatalf("retry with %s sibling=%v", pendingStatus, err)
					}
					periodicMust(t, pool.QueryRow(ctx, `SELECT status, attempts, last_error FROM graph.jobs WHERE id=$1`, id).Scan(&status, &attempts, &lastError))
					if status != "failed" || attempts != 3 || lastError == nil || *lastError != "old" {
						t.Fatal("conflict mutated failed job")
					}
				} else {
					periodicMust(t, err)
				}
			}
		})
	}
	periodicMust(t, RetryPeriodicNow(ctx, pool, -1))
}

func TestPeriodicTickRetryRace(t *testing.T) {
	pool := periodicDB(t, 16)
	ctx := context.Background()
	for _, tc := range periodicCases {
		t.Run(tc.typ, func(t *testing.T) {
			periodicReset(t, pool)
			id, err := EnqueuePeriodicNow(ctx, pool, tc.typ, "machine", "vps")
			periodicMust(t, err)
			periodicMust(t, Fail(ctx, pool, id, ErrFatal))
			start := make(chan struct{})
			done := make(chan struct{})
			errs := make(chan error, 1)
			go func() { <-start; PeriodicTick(ctx, pool, "ticker", "vps", time.Now(), zerolog.Nop()); close(done) }()
			go func() { <-start; errs <- RetryPeriodicNow(ctx, pool, id) }()
			close(start)
			err = <-errs
			if err != nil && !errors.Is(err, ErrPeriodicPending) {
				t.Fatal(err)
			}
			<-done
			if n := periodicCount(t, pool, tc.typ); n != 1 {
				t.Fatalf("pending=%d, want one", n)
			}
		})
	}
}

func periodicWaitStatus(t *testing.T, pool *pgxpool.Pool, id int64, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		periodicMust(t, pool.QueryRow(context.Background(), `SELECT status FROM graph.jobs WHERE id=$1`, id).Scan(&status))
		if status == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %d did not reach %s", id, want)
}

func TestPeriodicTickerDispatcherLiveness(t *testing.T) {
	pool := periodicDB(t, 4)
	ctx := context.Background()
	for _, scenario := range []string{"terminal_failure", "refunded_limit"} {
		t.Run(scenario, func(t *testing.T) {
			periodicReset(t, pool)
			typ := "notify_watch_channels"
			now := time.Now().UTC()
			PeriodicTick(ctx, pool, "machine", "vps", now, zerolog.Nop())
			id := periodicID(t, pool, typ)
			periodicExec(t, pool, `UPDATE graph.jobs SET max_attempts=$2 WHERE id=$1`, id, map[string]int{"terminal_failure": 2, "refunded_limit": 1}[scenario])
			var calls atomic.Int32
			reached := make(chan struct{})
			release := make(chan struct{})
			defer close(release)
			refundErr := errors.New("infrastructure capped")
			reg := NewRegistry()
			reg.Register(typ, Entry{PoolSize: 1, Lease: 10 * time.Second, Handler: func(ctx context.Context, _ []byte) error {
				n := calls.Add(1)
				PeriodicTick(ctx, pool, "machine", "vps", now.Add(10*time.Minute), zerolog.Nop())
				var pending int
				if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph.jobs WHERE type=$1 AND status IN ('queued','running')`, typ).Scan(&pending); err != nil {
					t.Errorf("attempt %d pending query: %v", n, err)
					return err
				}
				if pending != 1 {
					t.Errorf("attempt %d has %d pending rows, want one", n, pending)
					return ErrFatal
				}
				if scenario == "terminal_failure" {
					return ErrTransient
				}
				if n <= 2 {
					return refundErr
				}
				if scenario == "refunded_limit" && n == 3 {
					close(reached)
					select {
					case <-release:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				return nil
			}})
			runCtx, cancel := context.WithCancel(ctx)
			stopped := make(chan struct{})
			d := NewTypeDispatcher(DispatcherConfig{Type: typ, Registry: reg, DB: pool, WorkerID: "periodic-test", Runner: "vps", IdleInterval: 5 * time.Millisecond, BackoffBase: 10 * time.Millisecond, BackoffCap: 10 * time.Millisecond, Logger: zerolog.Nop(), RefundAttempt: func(err error) bool { return errors.Is(err, refundErr) }})
			go func() { d.Run(runCtx); close(stopped) }()
			defer func() { cancel(); <-stopped }()
			if scenario == "terminal_failure" {
				periodicWaitStatus(t, pool, id, "failed")
				if calls.Load() != 2 {
					t.Fatalf("calls=%d, want explicit attempt limit 2", calls.Load())
				}
			} else {
				select {
				case <-reached:
				case <-time.After(5 * time.Second):
					t.Fatal("refunded max-attempt job failed to remain live")
				}
				var attempts int16
				periodicMust(t, pool.QueryRow(ctx, `SELECT attempts FROM graph.jobs WHERE id=$1`, id).Scan(&attempts))
				if attempts != 1 {
					t.Fatalf("attempts=%d after two refunds, want 1", attempts)
				}
				PeriodicTick(ctx, pool, "machine", "vps", now.Add(5*time.Minute), zerolog.Nop())
				if periodicID(t, pool, typ) != id || periodicCount(t, pool, typ) != 1 {
					t.Fatal("refunded/running job duplicated")
				}
				release <- struct{}{}
				periodicWaitStatus(t, pool, id, "done")
			}
			PeriodicTick(ctx, pool, "machine", "vps", now.Add(5*time.Minute), zerolog.Nop())
			next := periodicID(t, pool, typ)
			if next == id {
				t.Fatal("terminal job prevented next due recurrence")
			}
			// The synthetic due clock is only for cadence; make the row runnable now.
			periodicExec(t, pool, `UPDATE graph.jobs SET available_at=NOW(), max_attempts=2 WHERE id=$1`, next)
			if scenario == "terminal_failure" {
				periodicWaitStatus(t, pool, next, "failed")
				if calls.Load() != 4 {
					t.Fatalf("calls=%d across two always-failing rows, want 4", calls.Load())
				}
			} else {
				periodicWaitStatus(t, pool, next, "done")
			}
		})
	}
}

func TestPeriodicTickerReclaimInterleavings(t *testing.T) {
	pool := periodicDB(t, 4)
	ctx := context.Background()
	for _, scenario := range []string{"reclaim_before_original_complete", "original_then_reclaimed_complete", "stale_retry_after_replacement"} {
		t.Run(scenario, func(t *testing.T) {
			periodicReset(t, pool)
			typ := "notify_watch_channels"
			now := time.Now().UTC()
			PeriodicTick(ctx, pool, "machine", "vps", now, zerolog.Nop())
			original, err := Claim(ctx, pool, typ, time.Minute, "original", "vps")
			periodicMust(t, err)
			if original == nil {
				t.Fatal("no original claim")
			}
			started := make(chan struct{})
			releaseOriginal := make(chan struct{})
			originalDone := make(chan error, 1)
			var releaseOnce sync.Once
			originalFinished := false
			go func() {
				close(started)
				<-releaseOriginal
				originalDone <- Complete(ctx, pool, original.ID)
			}()
			<-started
			defer func() {
				releaseOnce.Do(func() { close(releaseOriginal) })
				if !originalFinished {
					<-originalDone
				}
			}()
			finishOriginal := func() {
				releaseOnce.Do(func() { close(releaseOriginal) })
				err := <-originalDone
				originalFinished = true
				periodicMust(t, err)
			}
			periodicExec(t, pool, `UPDATE graph.jobs SET lease_until=NOW()-interval '1 second' WHERE id=$1`, original.ID)
			n, err := NewJanitor(JanitorConfig{DB: pool}).scan(ctx)
			periodicMust(t, err)
			if n != 1 {
				t.Fatalf("reclaimed=%d", n)
			}
			PeriodicTick(ctx, pool, "machine", "vps", now.Add(5*time.Minute), zerolog.Nop())
			if n := periodicCount(t, pool, typ); n != 1 || periodicID(t, pool, typ) != original.ID {
				t.Fatalf("tick duplicated reclaimed queued original: pending=%d", n)
			}
			if scenario == "reclaim_before_original_complete" {
				finishOriginal()
			} else {
				reclaimed, err := Claim(ctx, pool, typ, time.Minute, "reclaimed", "vps")
				periodicMust(t, err)
				if reclaimed == nil || reclaimed.ID != original.ID {
					t.Fatal("reclaim did not reuse original row")
				}
				PeriodicTick(ctx, pool, "machine", "vps", now.Add(5*time.Minute), zerolog.Nop())
				if n := periodicCount(t, pool, typ); n != 1 || periodicID(t, pool, typ) != original.ID {
					t.Fatalf("tick duplicated reclaimed running original: pending=%d", n)
				}
				finishOriginal()
				if scenario == "original_then_reclaimed_complete" {
					PeriodicTick(ctx, pool, "machine", "vps", now.Add(5*time.Minute), zerolog.Nop())
					replacement := periodicID(t, pool, typ)
					if replacement == original.ID {
						t.Fatal("missing replacement before reclaimed completion")
					}
					periodicMust(t, Complete(ctx, pool, reclaimed.ID))
					if n := periodicCount(t, pool, typ); n != 1 {
						t.Fatalf("stale completion affected replacement: pending=%d", n)
					}
				} else {
					PeriodicTick(ctx, pool, "machine", "vps", now.Add(5*time.Minute), zerolog.Nop())
					replacement := periodicID(t, pool, typ)
					if replacement == original.ID {
						t.Fatal("missing replacement")
					}
					// No fencing change: a stale worker Retry intentionally revives the old row.
					periodicMust(t, Retry(ctx, pool, original.ID, ErrTransient, 0))
					if n := periodicCount(t, pool, typ); n != 2 {
						t.Fatalf("documented stale retry pending=%d, want 2", n)
					}
					PeriodicTick(ctx, pool, "machine", "vps", now.Add(10*time.Minute), zerolog.Nop())
					if n := periodicCount(t, pool, typ); n != 2 {
						t.Fatalf("ticker added a third pending row: %d", n)
					}
					periodicMust(t, Complete(ctx, pool, replacement))
					periodicMust(t, Complete(ctx, pool, original.ID))
				}
			}
			PeriodicTick(ctx, pool, "machine", "vps", now.Add(10*time.Minute), zerolog.Nop())
			if n := periodicCount(t, pool, typ); n != 1 {
				t.Fatalf("next due did not restore one pending: %d", n)
			}
		})
	}
}

func TestPeriodicTickerNoImmediateTick(t *testing.T) {
	pool := periodicDB(t, 4)
	periodicReset(t, pool)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { RunPeriodicJobsTicker(ctx, pool, "machine", "vps", zerolog.Nop()); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ticker did not stop on cancellation")
	}
	for _, tc := range periodicCases {
		if periodicCount(t, pool, tc.typ) != 0 {
			t.Fatalf("%s enqueued before first 30s tick", tc.typ)
		}
	}
}
