package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

func membersReset(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	truncateGraphHandlerTables(t, db)
	membersExec(t, db, `DELETE FROM graph.member_scopes; DELETE FROM settings WHERE key LIKE 'graph.slack_members.%'`)
	t.Cleanup(func() {
		membersExec(t, db, `DELETE FROM graph.member_scopes; DELETE FROM settings WHERE key LIKE 'graph.slack_members.%'`)
	})
	membersExec(t, db, `INSERT INTO graph.people(id,display_name,slack_user_id,eeid,machine_id) VALUES (71001,'one','U1',1001,'test'),(71002,'two','U2',1002,'test'),(71003,'null','UN',NULL,'test'),(71004,'merged','UM',1003,'test')`)
	membersExec(t, db, `UPDATE graph.people SET merged_into=71001 WHERE id=71004`)
}
func membersExec(t *testing.T, db *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}
func membersNode(t *testing.T, db *pgxpool.Pool, channel string) {
	t.Helper()
	membersExec(t, db, `INSERT INTO graph.nodes(id,type,natural_key,scope,machine_id) VALUES($1,'slack_message',$1,$2,'test')`, "members:"+channel, "slack:"+channel)
}
func membersGrant(t *testing.T, db *pgxpool.Pool, eeid int, scope string) {
	t.Helper()
	membersExec(t, db, `INSERT INTO graph.member_scopes(eeid,scope) VALUES($1,$2) ON CONFLICT(eeid,scope) DO UPDATE SET refreshed_at=now()`, eeid, scope)
}
func membersWant(t *testing.T, db *pgxpool.Pool, scope string, want ...int) {
	t.Helper()
	rows, err := db.Query(context.Background(), `SELECT eeid FROM graph.member_scopes WHERE scope=$1 ORDER BY eeid`, scope)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		got = append(got, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s members=%v want=%v", scope, got, want)
	}
}
func membersClient(t *testing.T, serve http.HandlerFunc) slackMembersClient {
	t.Helper()
	srv := httptest.NewServer(serve)
	t.Cleanup(srv.Close)
	return slackMembersClient{baseURL: srv.URL, http: srv.Client(), wait: func(ctx context.Context, _ time.Duration) error { return ctx.Err() }}
}
func membersReply(w http.ResponseWriter, ids []string, cursor string) {
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "members": ids, "response_metadata": map[string]string{"next_cursor": cursor}})
}
func TestRefreshSlackMembers(t *testing.T) {
	t.Run("mapping_pagination_revocation_archived", func(t *testing.T) {
		db := openTestDB(t)
		membersReset(t, db)
		membersNode(t, db, "C1")
		membersNode(t, db, "G2")
		membersExec(t, db, `UPDATE graph.nodes SET metadata='{"is_archived":true}' WHERE scope='slack:G2'`)
		membersGrant(t, db, 1001, "jira:PAY")
		membersGrant(t, db, 1001, "slack:D1")
		removed := false
		var calls int
		client := membersClient(t, func(w http.ResponseWriter, r *http.Request) {
			calls++
			if r.URL.Path != "/conversations.members" || r.URL.Query().Get("limit") != "1000" || r.Header.Get("Authorization") != "Bearer fake" {
				t.Errorf("bad request %s", r.URL)
			}
			if r.URL.Query().Get("channel") == "C1" {
				if r.URL.Query().Get("cursor") == "" {
					membersReply(w, nil, "next")
				} else if removed {
					membersReply(w, []string{"U2"}, "")
				} else {
					membersReply(w, []string{"U1", "U2", "UN", "UM", "WEXTERNAL", "UX"}, "")
				}
			} else {
				membersReply(w, []string{"U1"}, "")
			}
		})
		var logs bytes.Buffer
		h := refreshSlackMembersWithClient(Deps{DB: db, SlackBotToken: "fake", Logger: zerolog.New(&logs)}, client)
		if err := h(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		membersWant(t, db, "slack:C1", 1001, 1002)
		membersWant(t, db, "slack:G2", 1001)
		if !bytes.Contains(logs.Bytes(), []byte(`"unmapped_members":4`)) {
			t.Fatalf("missing mapping counts: %s", logs.String())
		}
		before := calls
		if err := h(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		if calls != before {
			t.Fatal("duplicate fetched Slack")
		}
		removed = true
		if err := h(context.Background(), []byte(`{"force":true}`)); err != nil {
			t.Fatal(err)
		}
		membersWant(t, db, "slack:C1", 1002)
		membersWant(t, db, "jira:PAY", 1001)
		membersWant(t, db, "slack:D1", 1001)
	})
	t.Run("per_page_outcomes", func(t *testing.T) {
		for _, tc := range []struct {
			name, body string
			status     int
			want       error
			delete     bool
			unknown    bool
		}{
			{"inaccessible", `{"ok":false,"error":"not_in_channel"}`, 200, nil, true, false},
			{"page2_inaccessible", `{"ok":false,"error":"channel_not_found"}`, 200, nil, true, false},
			{"http429", "", 429, jobs.ErrTransient, false, false},
			{"json_rate", `{"ok":false,"error":"ratelimited"}`, 200, jobs.ErrTransient, false, false},
			{"http500", "", 500, jobs.ErrTransient, false, false},
			{"network", "", 0, jobs.ErrTransient, false, false},
			{"invalid_auth", `{"ok":false,"error":"invalid_auth"}`, 200, jobs.ErrFatal, false, false},
			{"token_revoked", `{"ok":false,"error":"token_revoked"}`, 200, jobs.ErrFatal, false, false},
			{"missing_scope", `{"ok":false,"error":"missing_scope"}`, 200, jobs.ErrFatal, false, false},
			{"not_authed", `{"ok":false,"error":"not_authed"}`, 200, jobs.ErrFatal, false, false},
			{"account_inactive", `{"ok":false,"error":"account_inactive"}`, 200, jobs.ErrFatal, false, false},
			{"unknown", `{"ok":false,"error":"other"}`, 200, nil, false, true},
			{"decode", "{", 200, nil, false, true},
			{"empty", `{"ok":true,"members":[],"response_metadata":{"next_cursor":""}}`, 200, nil, true, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				db := openTestDB(t)
				membersReset(t, db)
				membersNode(t, db, "C1")
				membersNode(t, db, "C2")
				membersGrant(t, db, 1001, "slack:C1")
				membersGrant(t, db, 1001, "slack:C2")
				membersGrant(t, db, 1001, "slack:CSTALE")
				membersGrant(t, db, 1001, "slack:CEXPIRED")
				membersExec(t, db, `UPDATE graph.member_scopes SET refreshed_at=now()-interval '25 hours' WHERE scope='slack:CEXPIRED'`)
				var failures int
				client := membersClient(t, func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Query().Get("channel") == "C1" {
						membersReply(w, []string{"U2"}, "")
						return
					}
					if r.URL.Query().Get("cursor") == "" && tc.name != "inaccessible" {
						var first []string
						if tc.name != "empty" {
							first = []string{"U2"}
						}
						membersReply(w, first, "second")
						return
					}
					failures++
					if tc.name == "network" {
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = conn.Close()
						return
					}
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				})
				if tc.name == "network" {
					// A fresh connection avoids net/http's implicit retry of a
					// reused keep-alive connection's EOF, isolating our retry.
					client.http.Transport = &http.Transport{DisableKeepAlives: true}
				}
				h := refreshSlackMembersWithClient(Deps{DB: db, SlackBotToken: "fake", Logger: zerolog.Nop()}, client)
				err := h(context.Background(), []byte(`{"force":true}`))
				if !errors.Is(err, tc.want) {
					t.Fatalf("err=%v want=%v", err, tc.want)
				}
				membersWant(t, db, "slack:C1", 1002)
				if tc.delete {
					membersWant(t, db, "slack:C2")
				} else {
					membersWant(t, db, "slack:C2", 1001)
				}
				membersWant(t, db, "slack:CEXPIRED")
				if tc.want != nil || tc.unknown {
					membersWant(t, db, "slack:CSTALE", 1001)
				} else {
					membersWant(t, db, "slack:CSTALE")
				}
				if errors.Is(tc.want, jobs.ErrTransient) && failures != 2 {
					t.Fatalf("retries=%d want2", failures)
				}
			})
		}
	})
	t.Run("pagination_unknown", func(t *testing.T) {
		for _, mode := range []string{"cap", "repeat"} {
			t.Run(mode, func(t *testing.T) {
				db := openTestDB(t)
				membersReset(t, db)
				membersNode(t, db, "C1")
				membersGrant(t, db, 1001, "slack:C1")
				membersGrant(t, db, 1001, "slack:CSTALE")
				n := 0
				client := membersClient(t, func(w http.ResponseWriter, r *http.Request) {
					n++
					cursor := fmt.Sprint(n)
					if mode == "repeat" {
						cursor = "repeat"
					}
					membersReply(w, []string{"U2"}, cursor)
				})
				if err := refreshSlackMembersWithClient(Deps{DB: db, SlackBotToken: "fake", Logger: zerolog.Nop()}, client)(context.Background(), nil); err != nil {
					t.Fatal(err)
				}
				membersWant(t, db, "slack:C1", 1001)
				membersWant(t, db, "slack:CSTALE", 1001)
				if mode == "cap" && n != 20 {
					t.Fatalf("pages=%d", n)
				}
				if mode == "repeat" && n != 2 {
					t.Fatalf("pages=%d", n)
				}
			})
		}
	})
	t.Run("cleanup_rechecks_live_nodes", func(t *testing.T) {
		db := openTestDB(t)
		membersReset(t, db)
		membersNode(t, db, "C1")
		membersNode(t, db, "CDELETED")
		membersExec(t, db, `UPDATE graph.nodes SET deleted_at=now() WHERE scope='slack:CDELETED'`)
		for _, scope := range []string{"slack:CDELETED", "slack:CNEW", "slack:D1", "jira:PAY"} {
			membersGrant(t, db, 1001, scope)
		}
		client := membersClient(t, func(w http.ResponseWriter, r *http.Request) {
			membersNode(t, db, "CNEW")
			membersReply(w, []string{"U1"}, "")
		})
		if err := refreshSlackMembersWithClient(Deps{DB: db, SlackBotToken: "fake", Logger: zerolog.Nop()}, client)(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		membersWant(t, db, "slack:CDELETED")
		membersWant(t, db, "slack:CNEW", 1001)
		membersWant(t, db, "slack:D1", 1001)
		membersWant(t, db, "jira:PAY", 1001)
	})
	t.Run("fatal_first_page_keeps_fresh_grants", func(t *testing.T) {
		db := openTestDB(t)
		membersReset(t, db)
		membersNode(t, db, "C1")
		membersGrant(t, db, 1001, "slack:C1")
		membersGrant(t, db, 1002, "slack:CUNVISITED")
		client := membersClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
		})
		err := refreshSlackMembersWithClient(Deps{DB: db, SlackBotToken: "fake", Logger: zerolog.Nop()}, client)(context.Background(), nil)
		if !errors.Is(err, jobs.ErrFatal) {
			t.Fatalf("fatal=%v", err)
		}
		membersWant(t, db, "slack:C1", 1001)
		membersWant(t, db, "slack:CUNVISITED", 1002)
	})
	t.Run("retry_after_caps_and_recovers_once", func(t *testing.T) {
		db := openTestDB(t)
		membersReset(t, db)
		membersNode(t, db, "C1")
		membersGrant(t, db, 1001, "slack:C1")
		calls := 0
		client := membersClient(t, func(w http.ResponseWriter, r *http.Request) {
			calls++
			if calls == 1 {
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(429)
				return
			}
			membersReply(w, []string{"U2"}, "")
		})
		var waits []time.Duration
		client.wait = func(ctx context.Context, delay time.Duration) error {
			waits = append(waits, delay)
			membersWant(t, db, "slack:C1", 1001)
			return ctx.Err()
		}
		if err := refreshSlackMembersWithClient(Deps{DB: db, SlackBotToken: "fake", Logger: zerolog.Nop()}, client)(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		if calls != 2 || !reflect.DeepEqual(waits, []time.Duration{60 * time.Second}) {
			t.Fatalf("requests=%d waits=%v", calls, waits)
		}
		membersWant(t, db, "slack:C1", 1002)
	})
}

func TestRefreshSlackMembers_LockLifecycle(t *testing.T) {
	db := openTestDB(t)
	membersReset(t, db)
	membersNode(t, db, "C1")
	cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 2
	small, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer small.Close()
	t.Run("overlap_cancellation", func(t *testing.T) {
		entered := make(chan struct{})
		var calls atomic.Int32
		client := membersClient(t, func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				close(entered)
				<-r.Context().Done()
				return
			}
			membersReply(w, nil, "")
		})
		h := refreshSlackMembersWithClient(Deps{DB: small, SlackBotToken: "fake", Logger: zerolog.Nop()}, client)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- h(ctx, nil) }()
		<-entered
		var stamp string
		if err := db.QueryRow(context.Background(), `SELECT value FROM settings WHERE key=$1`, slackMembersLastAttemptKey).Scan(&stamp); err != nil {
			t.Fatal(err)
		}
		if err := h(context.Background(), []byte(`{"force":true}`)); err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 1 {
			t.Fatal("lock loser fetched")
		}
		var after string
		_ = db.QueryRow(context.Background(), `SELECT value FROM settings WHERE key=$1`, slackMembersLastAttemptKey).Scan(&after)
		if stamp != after {
			t.Fatal("lock loser wrote stamp")
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel=%v", err)
		}
		check, err := db.Acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer check.Release()
		var locked bool
		if err := check.QueryRow(context.Background(), `SELECT pg_try_advisory_lock(hashtext('refresh_slack_members'))`).Scan(&locked); err != nil || !locked {
			t.Fatalf("lock leaked: %v %v", locked, err)
		}
		if _, err := check.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext('refresh_slack_members'))`); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("unlock_failure_discards_and_releases", func(t *testing.T) {
		membersExec(t, db, `DELETE FROM settings WHERE key=$1`, slackMembersLastAttemptKey)
		client := membersClient(t, func(w http.ResponseWriter, r *http.Request) {
			// Losing the pinned backend forces the bounded unlock-error path.
			var pid int
			if err := db.QueryRow(context.Background(), `SELECT pid FROM pg_locks WHERE locktype='advisory' AND granted LIMIT 1`).Scan(&pid); err != nil {
				t.Error(err)
			}
			if _, err := db.Exec(context.Background(), `SELECT pg_terminate_backend($1)`, pid); err != nil {
				t.Error(err)
			}
			membersReply(w, []string{"U1"}, "")
		})
		_ = refreshSlackMembersWithClient(Deps{DB: small, SlackBotToken: "fake", Logger: zerolog.Nop()}, client)(context.Background(), []byte(`{"force":true}`))
		for i := range 5 {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			conn, err := small.Acquire(ctx)
			cancel()
			if err != nil {
				t.Fatalf("acquire%d: %v", i, err)
			}
			var locked bool
			if err := conn.QueryRow(context.Background(), `SELECT pg_try_advisory_lock(hashtext('refresh_slack_members'))`).Scan(&locked); err != nil || !locked {
				t.Fatalf("lock after failure: %v %v", locked, err)
			}
			_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext('refresh_slack_members'))`)
			conn.Release()
		}
	})
	t.Run("unlock_false_closes_live_session", func(t *testing.T) {
		membersExec(t, db, `CREATE SCHEMA members_unlock_test;
			CREATE FUNCTION members_unlock_test.pg_advisory_unlock(bigint) RETURNS boolean LANGUAGE sql AS 'SELECT false'`)
		defer membersExec(t, db, `DROP SCHEMA members_unlock_test CASCADE`)
		faultCfg := cfg.Copy()
		faultCfg.ConnConfig.RuntimeParams["search_path"] = "members_unlock_test,pg_catalog,public"
		faultPool, err := pgxpool.NewWithConfig(context.Background(), faultCfg)
		if err != nil {
			t.Fatal(err)
		}
		defer faultPool.Close()
		var holderPID uint32
		client := membersClient(t, func(w http.ResponseWriter, r *http.Request) {
			if err := db.QueryRow(context.Background(), `SELECT pid FROM pg_locks WHERE locktype='advisory' AND granted LIMIT 1`).Scan(&holderPID); err != nil {
				t.Error(err)
			}
			membersReply(w, []string{"U1"}, "")
		})
		if err := refreshSlackMembersWithClient(Deps{DB: faultPool, SlackBotToken: "fake", Logger: zerolog.Nop()}, client)(context.Background(), []byte(`{"force":true}`)); err != nil {
			t.Fatal(err)
		}
		// Reserve one slot while acquiring the other repeatedly: leaking the
		// released wrapper now exhausts this two-slot pool instead of hiding.
		held, err := faultPool.Acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer held.Release()
		if held.Conn().PgConn().PID() == holderPID {
			t.Fatal("failed unlock returned its live session to the pool")
		}
		for i := range 5 {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			conn, err := faultPool.Acquire(ctx)
			cancel()
			if err != nil {
				t.Fatalf("acquire %d: %v", i, err)
			}
			var locked bool
			err = conn.QueryRow(context.Background(), `SELECT pg_catalog.pg_try_advisory_lock(hashtext('refresh_slack_members'))`).Scan(&locked)
			if err != nil || !locked {
				conn.Release()
				t.Fatalf("old session retained lock: %v %v", locked, err)
			}
			_, err = conn.Exec(context.Background(), `SELECT pg_catalog.pg_advisory_unlock(hashtext('refresh_slack_members'))`)
			conn.Release()
			if err != nil {
				t.Fatal(err)
			}
		}
	})
	t.Run("pinned_small_pool", func(t *testing.T) {
		client := membersClient(t, func(w http.ResponseWriter, r *http.Request) { membersReply(w, []string{"U1"}, "") })
		held, err := small.Acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer held.Release()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := refreshSlackMembersWithClient(Deps{DB: small, SlackBotToken: "fake", Logger: zerolog.Nop()}, client)(ctx, []byte(`{"force":true}`)); err != nil {
			t.Fatal(err)
		}
		membersWant(t, db, "slack:C1", 1001)
	})
}
