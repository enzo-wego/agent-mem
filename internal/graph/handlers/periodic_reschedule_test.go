package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

func periodicHandlerDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Fatal("DATABASE_URL is required for periodic integration tests")
	}
	if databaseName(dsn) != "agentmem_test" {
		t.Fatal("periodic tests require agentmem_test database")
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	truncateGraphHandlerTables(t, pool)
	t.Cleanup(func() { truncateGraphHandlerTables(t, pool) })
	return pool
}

type periodicStubTransport struct{ t *testing.T }

func (s periodicStubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	s.t.Errorf("unexpected external request: %s", r.URL)
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true,"issues":[],"values":[],"isLast":true}`)), Request: r}, nil
}

func TestPeriodicHandlers_NoSelfReschedule(t *testing.T) {
	pool := periodicHandlerDB(t)
	original := http.DefaultTransport
	http.DefaultTransport = periodicStubTransport{t}
	t.Cleanup(func() { http.DefaultTransport = original })
	t.Setenv("AGENT_MEM_JIRA_BASE_URL", "https://stub.invalid")
	t.Setenv("AGENT_MEM_JIRA_EMAIL", "stub")
	t.Setenv("AGENT_MEM_JIRA_TOKEN", "stub")
	if _, err := pool.Exec(t.Context(), `DELETE FROM graph.topic_subscriptions`); err != nil {
		t.Fatal(err)
	}
	llm := &fakeGateway{}
	deps := Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "periodic-test", Runner: "local", SlackBotToken: "stub", Gemini: llm}
	cases := []struct {
		name string
		run  func(context.Context, []byte) error
	}{
		{"derive_person_roles", NewDerivePersonRolesHandler(deps).Handler},
		{"detect_hot_topics", NewDetectHotTopics(deps)},
		{"notify_watch_channels", NewNotifyWatchChannels(deps)},
		{"refresh_jira_board", NewRefreshJiraBoardHandler(deps).Handler},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.run(t.Context(), []byte(`{}`)); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM graph.jobs WHERE type=$1`, c.name).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("successor rows = %d, want zero", count)
			}
		})
	}
	if llm.called != "" {
		t.Fatalf("unexpected LLM call: %s", llm.called)
	}
}

func periodicPost(h http.Handler, typ, payload string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/graph/jobs/enqueue", strings.NewReader(fmt.Sprintf(`{"type":%q,"payload":%s}`, typ, payload))))
	return w
}

func TestEnqueuePeriodicNow_Endpoints(t *testing.T) {
	pool := periodicHandlerDB(t)
	h := NewJobsEnqueueHandler(Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "caller", Runner: "local"})
	for _, typ := range []string{"derive_person_roles", "refresh_jira_board"} {
		t.Run(typ, func(t *testing.T) {
			key := "graph.periodic." + typ + ".last_enqueued_at"
			if _, err := pool.Exec(t.Context(), `INSERT INTO settings(key,value) VALUES($1,'2020-01-01T00:00:00Z') ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`, key); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM settings WHERE key=$1`, key) })
			w := periodicPost(h, typ, `{"ignored":true}`)
			if w.Code != 200 {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
			var response struct {
				ID   int64  `json:"id"`
				Type string `json:"type"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.ID <= 0 || response.Type != typ {
				t.Fatalf("response=%+v", response)
			}
			var machine, runner, payload, schedule string
			var priority int
			if err := pool.QueryRow(t.Context(), `SELECT machine_id,target_runner,payload::text,priority FROM graph.jobs WHERE id=$1`, response.ID).Scan(&machine, &runner, &payload, &priority); err != nil {
				t.Fatal(err)
			}
			wantRunner := "local"
			if typ == "derive_person_roles" {
				wantRunner = "any"
			}
			if machine != "caller" || runner != wantRunner || payload != "{}" || priority != 5 {
				t.Fatalf("row machine=%s runner=%s payload=%s priority=%d", machine, runner, payload, priority)
			}
			for _, status := range []string{"queued", "running"} {
				if _, err := pool.Exec(t.Context(), `UPDATE graph.jobs SET status=$1 WHERE id=$2`, status, response.ID); err != nil {
					t.Fatal(err)
				}
				w = periodicPost(h, typ, `{}`)
				if w.Code != 409 || strings.TrimSpace(w.Body.String()) != `{"error":"already queued or running"}` {
					t.Fatalf("pending %s: %d %s", status, w.Code, w.Body)
				}
			}
			if err := pool.QueryRow(t.Context(), `SELECT value FROM settings WHERE key=$1`, key).Scan(&schedule); err != nil {
				t.Fatal(err)
			}
			if schedule != "2020-01-01T00:00:00Z" {
				t.Fatalf("manual enqueue changed schedule: %s", schedule)
			}
		})
	}
	for _, typ := range []string{"detect_hot_topics", "notify_watch_channels"} {
		if w := periodicPost(h, typ, `{}`); w.Code != 400 {
			t.Fatalf("%s status=%d", typ, w.Code)
		}
	}
	for _, c := range []struct {
		typ, payload string
		code         int
	}{
		{"refresh_epic_brief", `{"epic_key":"PAY-1"}`, 400},
		{"refresh_epic_brief", `{"epic_key":"PAY-1","dry_run":true}`, 200},
		{"import_bamboohr", `{"csv_path":"/stub.csv"}`, 200},
		{"refresh_slack_members", `{"force":true}`, 200},
	} {
		w := periodicPost(h, c.typ, c.payload)
		if w.Code != c.code {
			t.Fatalf("%s status=%d body=%s", c.typ, w.Code, w.Body)
		}
		if c.code == 200 {
			var same bool
			if err := pool.QueryRow(t.Context(), `SELECT payload=$2::jsonb FROM graph.jobs WHERE type=$1 ORDER BY id DESC LIMIT 1`, c.typ, c.payload).Scan(&same); err != nil || !same {
				t.Fatalf("%s payload preservation=%v err=%v", c.typ, same, err)
			}
		}
	}
}

func TestAdminRetry_Periodic(t *testing.T) {
	pool := periodicHandlerDB(t)
	h := NewJobsRetryHandler(Deps{DB: pool, Logger: zerolog.Nop()})
	for _, typ := range []string{"derive_person_roles", "refresh_jira_board", "detect_hot_topics", "notify_watch_channels", "fetch_body"} {
		t.Run(typ, func(t *testing.T) {
			if _, err := pool.Exec(t.Context(), `DELETE FROM graph.jobs`); err != nil {
				t.Fatal(err)
			}
			var id int64
			key := "graph.periodic." + typ + ".last_enqueued_at"
			if _, err := pool.Exec(t.Context(), `INSERT INTO settings(key,value) VALUES($1,'2020-01-01T00:00:00Z') ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`, key); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM settings WHERE key=$1`, key) })
			if err := pool.QueryRow(t.Context(), `INSERT INTO graph.jobs(type,payload,status,machine_id,target_runner,attempts,last_error) VALUES($1,'{}','failed','test','any',3,'timeout') RETURNING id`, typ).Scan(&id); err != nil {
				t.Fatal(err)
			}
			retry := func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, chiRequest(http.MethodPost, "/retry", "", "id", fmt.Sprint(id)))
				return w
			}
			for _, status := range []string{"queued", "running"} {
				if _, err := pool.Exec(t.Context(), `INSERT INTO graph.jobs(type,payload,status,machine_id,target_runner) VALUES($1,'{}',$2,'test','any')`, typ, status); err != nil {
					t.Fatal(err)
				}
				w := retry()
				want := 409
				if typ == "fetch_body" {
					want = 200
				}
				if w.Code != want {
					t.Fatalf("pending %s: status=%d body=%s", status, w.Code, w.Body)
				}
				if want == 409 && strings.TrimSpace(w.Body.String()) != `{"error":"already queued or running"}` {
					t.Fatalf("conflict body=%s", w.Body)
				}
				var count int
				if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM graph.jobs WHERE type=$1 AND status IN ('queued','running')`, typ).Scan(&count); err != nil {
					t.Fatal(err)
				}
				expected := 1
				if typ == "fetch_body" {
					expected = 2
				}
				if count != expected {
					t.Fatalf("pending rows=%d want=%d", count, expected)
				}
				if _, err := pool.Exec(t.Context(), `DELETE FROM graph.jobs WHERE id<>$1`, id); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(t.Context(), `UPDATE graph.jobs SET status='failed',attempts=3,last_error='timeout' WHERE id=$1`, id); err != nil {
					t.Fatal(err)
				}
			}
			if w := retry(); w.Code != 200 {
				t.Fatalf("retry status=%d body=%s", w.Code, w.Body)
			}
			var status string
			var attempts int
			var cleared bool
			if err := pool.QueryRow(t.Context(), `SELECT status,attempts,last_error IS NULL FROM graph.jobs WHERE id=$1`, id).Scan(&status, &attempts, &cleared); err != nil {
				t.Fatal(err)
			}
			if status != "queued" || attempts != 0 || !cleared {
				t.Fatalf("retry row status=%s attempts=%d cleared=%v", status, attempts, cleared)
			}
			var schedule string
			if err := pool.QueryRow(t.Context(), `SELECT value FROM settings WHERE key=$1`, key).Scan(&schedule); err != nil {
				t.Fatal(err)
			}
			if schedule != "2020-01-01T00:00:00Z" {
				t.Fatalf("retry changed schedule: %s", schedule)
			}
		})
	}
}

func TestAdminRetry_PeriodicConcurrentTickerAndHelper(t *testing.T) {
	pool := periodicHandlerDB(t)
	deps := Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "caller", Runner: "local"}
	for _, typ := range []string{"derive_person_roles", "refresh_jira_board", "detect_hot_topics", "notify_watch_channels"} {
		t.Run(typ, func(t *testing.T) {
			if _, err := pool.Exec(t.Context(), `DELETE FROM graph.jobs`); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(t.Context(), `DELETE FROM settings WHERE key LIKE 'graph.periodic.%.last_enqueued_at'`); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_, _ = pool.Exec(context.Background(), `DELETE FROM settings WHERE key LIKE 'graph.periodic.%.last_enqueued_at'`)
			})
			var id int64
			if err := pool.QueryRow(t.Context(), `INSERT INTO graph.jobs(type,payload,status,machine_id,target_runner) VALUES($1,'{}','failed','test','any') RETURNING id`, typ).Scan(&id); err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(3)
			var retry *httptest.ResponseRecorder
			var helperErr error
			go func() {
				defer wg.Done()
				<-start
				retry = httptest.NewRecorder()
				NewJobsRetryHandler(deps).ServeHTTP(retry, chiRequest(http.MethodPost, "/retry", "", "id", fmt.Sprint(id)))
			}()
			go func() {
				defer wg.Done()
				<-start
				_, helperErr = jobs.EnqueuePeriodicNow(t.Context(), pool, typ, "caller", "local")
			}()
			go func() {
				defer wg.Done()
				<-start
				jobs.PeriodicTick(t.Context(), pool, "ticker", "local", time.Now(), zerolog.Nop())
			}()
			close(start)
			wg.Wait()
			if retry.Code != 200 && retry.Code != 409 {
				t.Fatalf("retry status=%d body=%s", retry.Code, retry.Body)
			}
			if helperErr != nil && helperErr != jobs.ErrPeriodicPending {
				t.Fatalf("helper: %v", helperErr)
			}
			var count int
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM graph.jobs WHERE type=$1 AND status IN ('queued','running')`, typ).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("concurrent pending rows=%d, want exactly one", count)
			}
		})
	}
}
