package handlers

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

func membersJobCount(t *testing.T, db *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(), `SELECT count(*) FROM graph.jobs WHERE type='refresh_slack_members'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func TestSlackMembersTicker(t *testing.T) {
	db := openTestDB(t)
	membersReset(t, db)
	ctx := context.Background()
	now := time.Now().UTC()
	tick := func(at time.Time) {
		t.Helper()
		if err := slackMembersTick(ctx, db, "test", "any", at); err != nil {
			t.Fatal(err)
		}
	}
	tick(now)
	if n := membersJobCount(t, db); n != 1 {
		t.Fatalf("absent=%d", n)
	}
	tick(now)
	if n := membersJobCount(t, db); n != 1 {
		t.Fatalf("queued=%d", n)
	}
	membersExec(t, db, `DELETE FROM graph.jobs`)
	for _, tc := range []struct {
		age  time.Duration
		want int
	}{{10 * time.Minute, 0}, {61 * time.Minute, 1}} {
		membersExec(t, db, `DELETE FROM graph.jobs`)
		if err := putSetting(ctx, db, slackMembersLastAttemptKey, now.Add(-tc.age).Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
		tick(now)
		if n := membersJobCount(t, db); n != tc.want {
			t.Fatalf("age=%v jobs=%d", tc.age, n)
		}
	}
	membersExec(t, db, `UPDATE graph.jobs SET status='done',completed_at=now()-interval '2 hours'`)
	if _, err := jobs.Enqueue(ctx, db, "refresh_slack_members", map[string]any{}, jobs.EnqueueOptions{MachineID: "test"}); err != nil {
		t.Fatal(err)
	}
	membersExec(t, db, `UPDATE graph.jobs SET status='done',completed_at=now() WHERE status='queued'`)
	if err := putSetting(ctx, db, slackMembersLastAttemptKey, now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	tick(now)
	if n := membersJobCount(t, db); n != 2 {
		t.Fatalf("done history caused enqueue=%d", n)
	}
	t.Run("persistent_fatal_120_ticks", func(t *testing.T) {
		membersExec(t, db, `DELETE FROM graph.jobs; DELETE FROM settings WHERE key='graph.slack_members.last_attempt_at'`)
		membersNode(t, db, "C1")
		var calls int
		client := membersClient(t, func(w http.ResponseWriter, r *http.Request) {
			calls++
			_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
		})
		h := refreshSlackMembersWithClient(Deps{DB: db, SlackBotToken: "fake", Logger: zerolog.Nop()}, client)
		for minute := range 120 {
			at := now.Add(time.Duration(minute) * time.Minute)
			tick(at)
			var id int64
			err := db.QueryRow(ctx, `SELECT id FROM graph.jobs WHERE status='queued' LIMIT 1`).Scan(&id)
			if err != nil {
				continue
			}
			// Advance handler due state as well as the ticker's simulated clock.
			if err := putSetting(ctx, db, slackMembersLastAttemptKey, time.Now().Add(-61*time.Minute).UTC().Format(time.RFC3339Nano)); err != nil {
				t.Fatal(err)
			}
			if err := h(ctx, nil); !errors.Is(err, jobs.ErrFatal) {
				t.Fatalf("fatal=%v", err)
			}
			if err := putSetting(ctx, db, slackMembersLastAttemptKey, at.Format(time.RFC3339Nano)); err != nil {
				t.Fatal(err)
			}
			if err := jobs.Fail(ctx, db, id, jobs.ErrFatal); err != nil {
				t.Fatal(err)
			}
		}
		if calls != 2 || membersJobCount(t, db) != 2 {
			t.Fatalf("calls=%d jobs=%d", calls, membersJobCount(t, db))
		}
	})
}

func TestSlackMembersTicker_LockSkipThenFatal(t *testing.T) {
	db := openTestDB(t)
	membersReset(t, db)
	membersNode(t, db, "C1")
	membersGrant(t, db, 1001, "slack:C1")
	entered, release := make(chan struct{}), make(chan struct{})
	client := membersClient(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_, _ = w.Write([]byte(`{"ok":false,"error":"token_revoked"}`))
	})
	h := refreshSlackMembersWithClient(Deps{DB: db, SlackBotToken: "fake", Logger: zerolog.Nop()}, client)
	done := make(chan error, 1)
	go func() { done <- h(context.Background(), nil) }()
	<-entered
	now := time.Now()
	// A no-op duplicate can finish done, but must not postpone the holder's
	// next due tick if its actual refresh fails.
	if err := h(context.Background(), nil); err != nil {
		close(release)
		t.Fatal(err)
	}
	id, err := jobs.Enqueue(context.Background(), db, "refresh_slack_members", map[string]any{}, jobs.EnqueueOptions{MachineID: "test"})
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	if err := jobs.Complete(context.Background(), db, id); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	if err := <-done; !errors.Is(err, jobs.ErrFatal) {
		t.Fatalf("holder error=%v", err)
	}
	membersWant(t, db, "slack:C1", 1001)
	if err := slackMembersTick(context.Background(), db, "test", "any", now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if membersJobCount(t, db) != 1 {
		t.Fatal("recent failed attempt retried too early")
	}
	if err := slackMembersTick(context.Background(), db, "test", "any", now.Add(61*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if membersJobCount(t, db) != 2 {
		t.Fatal("done lock-skip suppressed next due tick")
	}
}

func membersWaitJob(t *testing.T, db *pgxpool.Pool, id int64, status string, attempts int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var got string
		var n int
		if err := db.QueryRow(context.Background(), `SELECT status,attempts FROM graph.jobs WHERE id=$1`, id).Scan(&got, &n); err != nil {
			t.Fatal(err)
		}
		if got == status && n == attempts {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	var got string
	var n int
	_ = db.QueryRow(context.Background(), `SELECT status,attempts FROM graph.jobs WHERE id=$1`, id).Scan(&got, &n)
	t.Fatalf("job state=%s attempts=%d want %s/%d", got, n, status, attempts)
}
func TestRefreshSlackMembers_DispatcherRetry(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(map[bool]string{false: "scheduled", true: "forced"}[force], func(t *testing.T) {
			db := openTestDB(t)
			membersReset(t, db)
			membersNode(t, db, "C1")
			membersGrant(t, db, 1001, "slack:C1")
			var calls atomic.Int32
			client := membersClient(t, func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				if n <= 2 {
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(429)
					return
				}
				membersReply(w, []string{"U2"}, "")
			})
			reg := jobs.NewRegistry()
			entry := NewRefreshSlackMembersHandler(Deps{DB: db})
			entry.Handler = refreshSlackMembersWithClient(Deps{DB: db, SlackBotToken: "fake", Logger: zerolog.Nop()}, client)
			reg.Register("refresh_slack_members", entry)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// Queue Retry stores whole seconds; sub-second delays become immediate.
			dispatcher := jobs.NewTypeDispatcher(jobs.DispatcherConfig{Type: "refresh_slack_members", Registry: reg, DB: db, WorkerID: "members-test", Runner: "local", IdleInterval: 5 * time.Millisecond, BackoffBase: 2 * time.Second, BackoffCap: 2 * time.Second, Logger: zerolog.Nop()})
			done := make(chan struct{})
			go func() { defer close(done); dispatcher.Run(ctx) }()
			defer func() { cancel(); <-done }()
			id, err := jobs.Enqueue(ctx, db, "refresh_slack_members", map[string]bool{"force": force}, jobs.EnqueueOptions{MachineID: "test"})
			if err != nil {
				t.Fatal(err)
			}
			if force {
				membersWaitJob(t, db, id, "queued", 1)
				membersWant(t, db, "slack:C1", 1001)
				membersWaitJob(t, db, id, "done", 2)
				if calls.Load() != 3 {
					t.Fatalf("force requests=%d", calls.Load())
				}
				membersWant(t, db, "slack:C1", 1002)
			} else {
				membersWaitJob(t, db, id, "done", 1)
				if calls.Load() != 2 {
					t.Fatalf("scheduled retry requests=%d", calls.Load())
				}
				membersWant(t, db, "slack:C1", 1001)
				if err := slackMembersTick(ctx, db, "test", "any", time.Now()); err != nil {
					t.Fatal(err)
				}
				if membersJobCount(t, db) != 1 {
					t.Fatal("premature ticker retry")
				}
				// Even another queued duplicate cannot call Slack before the interval.
				duplicate, err := jobs.Enqueue(ctx, db, "refresh_slack_members", map[string]any{}, jobs.EnqueueOptions{MachineID: "test"})
				if err != nil {
					t.Fatal(err)
				}
				membersWaitJob(t, db, duplicate, "done", 1)
				if calls.Load() != 2 {
					t.Fatal("duplicate bypassed cadence")
				}
				membersExec(t, db, `UPDATE settings SET value=$2 WHERE key=$1`, slackMembersLastAttemptKey, time.Now().Add(-61*time.Minute).UTC().Format(time.RFC3339Nano))
				if err := slackMembersTick(ctx, db, "test", "any", time.Now()); err != nil {
					t.Fatal(err)
				}
				var next int64
				if err := db.QueryRow(ctx, `SELECT max(id) FROM graph.jobs WHERE type='refresh_slack_members'`).Scan(&next); err != nil {
					t.Fatal(err)
				}
				membersWaitJob(t, db, next, "done", 1)
				membersWant(t, db, "slack:C1", 1002)
				if calls.Load() != 3 {
					t.Fatalf("next due requests=%d", calls.Load())
				}
			}
		})
	}
}
