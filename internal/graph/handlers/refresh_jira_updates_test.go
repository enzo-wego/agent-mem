package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

// jiraUpdatesSeed mirrors the migration's seed rows; tests restore it on cleanup.
var jiraUpdatesSeed = map[string]string{
	jiraUpdatesKeyEnabled:  "true",
	jiraUpdatesKeyInterval: "15",
	jiraUpdatesKeyCursor:   "2026-09-29T04:00:00Z",
}

// setJiraUpdatesSettings replaces every jira_updates_* setting with kv and
// restores the migration seed when the test ends.
func setJiraUpdatesSettings(t *testing.T, pool *pgxpool.Pool, kv map[string]string) {
	t.Helper()
	write := func(kv map[string]string) error {
		ctx := context.Background()
		if _, err := pool.Exec(ctx, `DELETE FROM settings WHERE key LIKE 'jira_updates\_%'`); err != nil {
			return err
		}
		for k, v := range kv {
			if err := putSetting(ctx, pool, k, v); err != nil {
				return err
			}
		}
		return nil
	}
	if err := write(kv); err != nil {
		t.Fatalf("set settings: %v", err)
	}
	t.Cleanup(func() {
		if err := write(jiraUpdatesSeed); err != nil {
			t.Errorf("restore settings: %v", err)
		}
	})
}

func setting(t *testing.T, pool *pgxpool.Pool, key string) (string, bool) {
	t.Helper()
	v, ok, err := getSetting(context.Background(), pool, key)
	if err != nil {
		t.Fatalf("getSetting %s: %v", key, err)
	}
	return v, ok
}

// fakeJira is a search/jql server serving fixed pages.
type fakeJira struct {
	mu     sync.Mutex
	calls  int
	bodies []map[string]any
	// pages[i] is the issues JSON (array) of page i+1.
	pages    []string
	failPage int // 1-based page answering 500; 0 = none
	sleep    time.Duration
}

func (f *fakeJira) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		f.calls++
		n := f.calls
		f.bodies = append(f.bodies, b)
		f.mu.Unlock()
		if f.sleep > 0 {
			time.Sleep(f.sleep)
		}
		if n == f.failPage {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		issues := "[]"
		if n <= len(f.pages) {
			issues = f.pages[n-1]
		}
		last := n >= len(f.pages)
		next := ""
		if !last {
			next = "tok" + string(rune('0'+n))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issues":        json.RawMessage(issues),
			"isLast":        last,
			"nextPageToken": next,
		})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("AGENT_MEM_JIRA_BASE_URL", srv.URL)
	t.Setenv("AGENT_MEM_JIRA_EMAIL", "me@x")
	t.Setenv("AGENT_MEM_JIRA_TOKEN", "tok")
	return srv
}

func (f *fakeJira) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func issuesJSON(pairs ...string) string {
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, `{"key":"`+pairs[i]+`","fields":{"updated":"`+pairs[i+1]+`"}}`)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

const jiraUpd = "2026-09-29T11:41:19.123+0700" // 04:41:19.123Z
var jiraUpdT = time.Date(2026, 9, 29, 4, 41, 19, 123e6, time.UTC)

type seedNode struct {
	key       string
	body      string
	bodyTS    *time.Time
	updatedAt time.Time
	deleted   bool
}

func seedJiraNodes(t *testing.T, pool *pgxpool.Pool, nodes ...seedNode) {
	t.Helper()
	for _, n := range nodes {
		var deletedAt *time.Time
		if n.deleted {
			d := time.Now()
			deletedAt = &d
		}
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO graph.nodes (id, type, natural_key, body, body_ts, updated_at, deleted_at, machine_id)
			VALUES ($1, 'jira', $2, $3, $4, $5, $6, 'test')`,
			"jira:"+n.key, n.key, n.body, n.bodyTS, n.updatedAt, deletedAt); err != nil {
			t.Fatalf("seed %s: %v", n.key, err)
		}
	}
}

func queuedFetchNodes(t *testing.T, pool *pgxpool.Pool, machine string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT payload->>'node_id' FROM graph.jobs WHERE type='fetch_body' AND status='queued' AND machine_id=$1`, machine)
	if err != nil {
		t.Fatalf("query jobs: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func tp(t time.Time) *time.Time { return &t }

func runJiraUpdates(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	deps := Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "jira-updates"}
	if err := refreshJiraUpdatesHandler(deps)(ctx, nil); err != nil {
		t.Fatalf("handler returned %v, want nil", err)
	}
}

// jiraUpdatesDB opens the scratch DB, clears graph tables, and seeds settings.
func jiraUpdatesDB(t *testing.T, kv map[string]string) *pgxpool.Pool {
	t.Helper()
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	t.Cleanup(func() { truncateGraphHandlerTables(t, pool) })
	setJiraUpdatesSettings(t, pool, kv)
	return pool
}

func assertRunFailed(t *testing.T, pool *pgxpool.Pool, cursor, lastOK string) {
	t.Helper()
	if v, _ := setting(t, pool, jiraUpdatesKeyCursor); v != cursor {
		t.Errorf("cursor = %q, want unchanged %q", v, cursor)
	}
	if v, _ := setting(t, pool, jiraUpdatesKeyLastOK); v != lastOK {
		t.Errorf("last_ok_at = %q, want unchanged %q", v, lastOK)
	}
	if v, _ := setting(t, pool, jiraUpdatesKeyLastErr); v == "" {
		t.Errorf("last_error empty, want set")
	}
}

func TestJiraUpdates_Candidates(t *testing.T) {
	pool := jiraUpdatesDB(t, map[string]string{jiraUpdatesKeyCursor: "2026-09-29T04:00:00Z"})
	older := jiraUpdT.Add(-time.Hour)
	seedJiraNodes(t, pool,
		seedNode{key: "PAY-1", body: "b", bodyTS: tp(older), updatedAt: older},
		seedNode{key: "PAY-2", body: "b", bodyTS: tp(older), updatedAt: jiraUpdT.Add(time.Hour)}, // updated_at > updated > body_ts
		seedNode{key: "PAY-3", body: "b", bodyTS: tp(jiraUpdT), updatedAt: older},                // equal body_ts
		seedNode{key: "PAY-4", body: "", bodyTS: tp(older), updatedAt: older},                    // empty body
		seedNode{key: "PAY-5", body: "b", bodyTS: tp(older), updatedAt: older, deleted: true},    // deleted
		seedNode{key: "PAY-6", body: "b", bodyTS: tp(older), updatedAt: older},                   // queued fetch
		seedNode{key: "PAY-7", body: "b", bodyTS: tp(older), updatedAt: older},                   // running fetch
		seedNode{key: "PAY-8", body: "b", updatedAt: older},                                      // null body_ts
	)
	ctx := context.Background()
	for id, status := range map[string]string{"jira:PAY-6": "queued", "jira:PAY-7": "running"} {
		if _, err := pool.Exec(ctx, `INSERT INTO graph.jobs (type, payload, status, machine_id)
			VALUES ('fetch_body', jsonb_build_object('node_id', $1::text), $2, 'other')`, id, status); err != nil {
			t.Fatalf("seed job: %v", err)
		}
	}
	f := &fakeJira{pages: []string{issuesJSON(
		"PAY-1", jiraUpd, "PAY-2", jiraUpd, "PAY-3", jiraUpd, "PAY-4", jiraUpd,
		"PAY-5", jiraUpd, "PAY-6", jiraUpd, "PAY-7", jiraUpd, "PAY-8", jiraUpd, "PAY-99", jiraUpd,
	)}}
	f.start(t)
	runJiraUpdates(t, ctx, pool)

	got := queuedFetchNodes(t, pool, "jira-updates")
	want := []string{"jira:PAY-1", "jira:PAY-2", "jira:PAY-8"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("queued = %v, want %v", got, want)
	}
	if v, _ := setting(t, pool, jiraUpdatesKeyQueued); v != "3" {
		t.Errorf("last_queued = %q, want 3", v)
	}
}

func TestJiraUpdates_Pagination(t *testing.T) {
	pool := jiraUpdatesDB(t, map[string]string{jiraUpdatesKeyCursor: "2026-09-29T04:00:00Z"})
	older := jiraUpdT.Add(-time.Hour)
	seedJiraNodes(t, pool,
		seedNode{key: "PAY-1", body: "b", bodyTS: tp(older), updatedAt: older},
		seedNode{key: "PAY-2", body: "b", bodyTS: tp(older), updatedAt: older},
	)
	f := &fakeJira{pages: []string{issuesJSON("PAY-1", jiraUpd), issuesJSON("PAY-2", jiraUpd)}}
	f.start(t)
	runJiraUpdates(t, context.Background(), pool)

	if f.callCount() != 2 {
		t.Fatalf("calls = %d, want 2", f.callCount())
	}
	if f.bodies[1]["nextPageToken"] != "tok1" {
		t.Errorf("page 2 token = %v", f.bodies[1]["nextPageToken"])
	}
	got := queuedFetchNodes(t, pool, "jira-updates")
	if strings.Join(got, ",") != "jira:PAY-1,jira:PAY-2" {
		t.Fatalf("queued = %v", got)
	}
}

func TestJiraUpdates_HTTPErrorKeepsCursor(t *testing.T) {
	for _, page := range []int{1, 2} {
		t.Run("page"+string(rune('0'+page)), func(t *testing.T) {
			const cursor, lastOK = "2026-09-29T04:00:00Z", "2026-09-29T03:00:00Z"
			pool := jiraUpdatesDB(t, map[string]string{jiraUpdatesKeyCursor: cursor, jiraUpdatesKeyLastOK: lastOK})
			f := &fakeJira{pages: []string{issuesJSON("PAY-1", jiraUpd), issuesJSON("PAY-2", jiraUpd)}, failPage: page}
			f.start(t)
			runJiraUpdates(t, context.Background(), pool)
			if f.callCount() != page {
				t.Errorf("calls = %d, want %d", f.callCount(), page)
			}
			assertRunFailed(t, pool, cursor, lastOK)
		})
	}
}

func TestJiraUpdates_BadTimestampAborts(t *testing.T) {
	const cursor, lastOK = "2026-09-29T04:00:00Z", "2026-09-29T03:00:00Z"
	pool := jiraUpdatesDB(t, map[string]string{jiraUpdatesKeyCursor: cursor, jiraUpdatesKeyLastOK: lastOK})
	older := jiraUpdT.Add(-time.Hour)
	seedJiraNodes(t, pool, seedNode{key: "PAY-1", body: "b", bodyTS: tp(older), updatedAt: older})
	f := &fakeJira{pages: []string{issuesJSON("PAY-1", jiraUpd, "PAY-2", "not a time")}}
	f.start(t)
	runJiraUpdates(t, context.Background(), pool)
	assertRunFailed(t, pool, cursor, lastOK)
	if got := queuedFetchNodes(t, pool, "jira-updates"); len(got) != 0 {
		t.Errorf("queued = %v, want none", got)
	}
}

func TestJiraUpdates_SuccessAdvances(t *testing.T) {
	pool := jiraUpdatesDB(t, map[string]string{
		jiraUpdatesKeyCursor:  "2026-09-29T04:00:00Z",
		jiraUpdatesKeyLastErr: "old failure",
	})
	f := &fakeJira{pages: []string{issuesJSON()}}
	f.start(t)
	before := time.Now().UTC().Truncate(time.Second)
	runJiraUpdates(t, context.Background(), pool)
	after := time.Now().UTC()

	cur, _ := setting(t, pool, jiraUpdatesKeyCursor)
	ct, err := time.Parse(time.RFC3339, cur)
	if err != nil || ct.Before(before) || ct.After(after) {
		t.Fatalf("cursor = %q, want start in [%s, %s]", cur, before, after)
	}
	if v, _ := setting(t, pool, jiraUpdatesKeyLastOK); v != cur {
		t.Errorf("last_ok_at = %q, want %q", v, cur)
	}
	if v, _ := setting(t, pool, jiraUpdatesKeyLastRun); v != cur {
		t.Errorf("last_run_at = %q, want %q", v, cur)
	}
	if v, ok := setting(t, pool, jiraUpdatesKeyQueued); !ok || v != "0" {
		t.Errorf("last_queued = %q (%v), want 0", v, ok)
	}
	if v, _ := setting(t, pool, jiraUpdatesKeyLastErr); v != "" {
		t.Errorf("last_error = %q, want empty", v)
	}
}

func TestJiraUpdates_NoCredentials(t *testing.T) {
	pool := jiraUpdatesDB(t, map[string]string{jiraUpdatesKeyCursor: "2026-09-29T04:00:00Z"})
	f := &fakeJira{}
	f.start(t)
	t.Setenv("AGENT_MEM_JIRA_TOKEN", "")
	runJiraUpdates(t, context.Background(), pool)
	if f.callCount() != 0 {
		t.Errorf("calls = %d, want 0", f.callCount())
	}
	if v, _ := setting(t, pool, jiraUpdatesKeyLastErr); v != "jira credentials not set" {
		t.Errorf("last_error = %q", v)
	}
}

func TestJiraUpdates_CursorMissing(t *testing.T) {
	pool := jiraUpdatesDB(t, map[string]string{})
	f := &fakeJira{}
	f.start(t)
	runJiraUpdates(t, context.Background(), pool)
	if f.callCount() != 0 {
		t.Errorf("calls = %d, want 0", f.callCount())
	}
	if v, _ := setting(t, pool, jiraUpdatesKeyLastErr); v != "cursor missing" {
		t.Errorf("last_error = %q, want \"cursor missing\"", v)
	}
	if v, _ := setting(t, pool, jiraUpdatesKeyLastRun); v == "" {
		t.Errorf("last_run_at not written")
	}
}

func TestJiraUpdates_FutureCursorClamped(t *testing.T) {
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	pool := jiraUpdatesDB(t, map[string]string{jiraUpdatesKeyCursor: future})
	f := &fakeJira{pages: []string{issuesJSON()}}
	f.start(t)
	runJiraUpdates(t, context.Background(), pool)
	if f.callCount() != 1 {
		t.Fatalf("calls = %d, want 1", f.callCount())
	}
	if got := f.bodies[0]["jql"]; got != "updated >= -20m ORDER BY updated ASC" {
		t.Errorf("jql = %v, want 20 minute window", got)
	}
}

func TestJiraUpdates_ExpiredContextRecordsError(t *testing.T) {
	const cursor, lastOK = "2026-09-29T04:00:00Z", "2026-09-29T03:00:00Z"
	pool := jiraUpdatesDB(t, map[string]string{jiraUpdatesKeyCursor: cursor, jiraUpdatesKeyLastOK: lastOK})
	f := &fakeJira{pages: []string{issuesJSON()}, sleep: 50 * time.Millisecond}
	f.start(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	runJiraUpdates(t, ctx, pool)
	assertRunFailed(t, pool, cursor, lastOK)
}

// --- ticker ---

var tickEnv = jiraUpdatesEnv{MachineID: "ticker-test", Runner: "any"}

func updatesJobCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM graph.jobs WHERE type='refresh_jira_updates'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func tick(t *testing.T, pool *pgxpool.Pool, now time.Time) {
	t.Helper()
	if err := jiraUpdatesTick(context.Background(), pool, tickEnv, now); err != nil {
		t.Fatalf("tick: %v", err)
	}
}

var tickNow = time.Date(2026, 9, 29, 6, 0, 0, 0, time.UTC)

func TestJiraTicker_Disabled(t *testing.T) {
	pool := jiraUpdatesDB(t, map[string]string{jiraUpdatesKeyEnabled: "false"})
	tick(t, pool, tickNow)
	if n := updatesJobCount(t, pool); n != 0 {
		t.Fatalf("jobs = %d, want 0", n)
	}
}

func TestJiraTicker_EnqueueOnce(t *testing.T) {
	pool := jiraUpdatesDB(t, map[string]string{})
	tick(t, pool, tickNow)
	if n := updatesJobCount(t, pool); n != 1 {
		t.Fatalf("first tick: jobs = %d, want 1", n)
	}
	tick(t, pool, tickNow)
	if n := updatesJobCount(t, pool); n != 1 {
		t.Fatalf("tick with job queued: jobs = %d, want 1", n)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE graph.jobs SET status='running' WHERE type='refresh_jira_updates'`); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	tick(t, pool, tickNow)
	if n := updatesJobCount(t, pool); n != 1 {
		t.Fatalf("tick with job running: jobs = %d, want 1", n)
	}
}

func TestJiraTicker_IntervalBoundary(t *testing.T) {
	for _, tc := range []struct {
		ago  time.Duration
		want int
	}{{14 * time.Minute, 0}, {15 * time.Minute, 1}} {
		t.Run(tc.ago.String(), func(t *testing.T) {
			pool := jiraUpdatesDB(t, map[string]string{
				jiraUpdatesKeyInterval: "15",
				jiraUpdatesKeyLastRun:  tickNow.Add(-tc.ago).Format(time.RFC3339),
			})
			tick(t, pool, tickNow)
			if n := updatesJobCount(t, pool); n != tc.want {
				t.Fatalf("jobs = %d, want %d", n, tc.want)
			}
		})
	}
}

func TestJiraTicker_CorruptInterval(t *testing.T) {
	for _, tc := range []struct {
		interval string
		ago      time.Duration
		want     int
	}{
		{"abc", 14 * time.Minute, 0}, // default 15
		{"abc", 15 * time.Minute, 1},
		{"1", 4 * time.Minute, 0}, // clamped up to 5
		{"1", 5 * time.Minute, 1},
		{"99999", 1439 * time.Minute, 0}, // clamped down to 1440
		{"99999", 1440 * time.Minute, 1},
	} {
		t.Run(tc.interval+"/"+tc.ago.String(), func(t *testing.T) {
			pool := jiraUpdatesDB(t, map[string]string{
				jiraUpdatesKeyInterval: tc.interval,
				jiraUpdatesKeyLastRun:  tickNow.Add(-tc.ago).Format(time.RFC3339),
			})
			tick(t, pool, tickNow)
			if n := updatesJobCount(t, pool); n != tc.want {
				t.Fatalf("jobs = %d, want %d", n, tc.want)
			}
		})
	}
	t.Run("unparsable last_run", func(t *testing.T) {
		pool := jiraUpdatesDB(t, map[string]string{jiraUpdatesKeyLastRun: "yesterday"})
		tick(t, pool, tickNow)
		if n := updatesJobCount(t, pool); n != 1 {
			t.Fatalf("jobs = %d, want 1", n)
		}
	})
}

func TestJiraTicker_LoopCancels(t *testing.T) {
	pool := jiraUpdatesDB(t, map[string]string{jiraUpdatesKeyEnabled: "false"})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runJiraUpdatesTicker(ctx, pool, tickEnv, 10*time.Millisecond, zerolog.Nop())
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runJiraUpdatesTicker did not return within 1s of cancel")
	}
}

// --- endpoint ---

func TestJiraUpdatesConfig_Endpoint(t *testing.T) {
	pool := openTestDB(t)
	setJiraUpdatesSettings(t, pool, map[string]string{})
	h := NewJiraUpdatesHandler(Deps{DB: pool, Logger: zerolog.Nop()})
	do := func(method, body string) (int, jiraUpdatesStatus, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/api/graph/jira-updates", strings.NewReader(body)))
		var st jiraUpdatesStatus
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
				t.Fatalf("decode: %v", err)
			}
		}
		return rec.Code, st, rec.Body.String()
	}

	code, st, raw := do(http.MethodGet, "")
	if code != 200 || !st.Enabled || st.IntervalMinutes != 15 {
		t.Fatalf("defaults: %d %+v", code, st)
	}
	for _, k := range []string{`"last_ok_at":null`, `"last_run_at":null`, `"last_queued":null`} {
		if !strings.Contains(raw, k) {
			t.Errorf("defaults body %s missing %s", raw, k)
		}
	}

	if err := putSetting(context.Background(), pool, jiraUpdatesKeyInterval, "abc"); err != nil {
		t.Fatal(err)
	}
	if _, st, _ = do(http.MethodGet, ""); st.IntervalMinutes != 15 {
		t.Errorf("interval 'abc' -> %d, want 15", st.IntervalMinutes)
	}

	code, st, _ = do(http.MethodPut, `{"enabled":false,"interval_minutes":30}`)
	if code != 200 || st.Enabled || st.IntervalMinutes != 30 {
		t.Fatalf("PUT: %d %+v", code, st)
	}
	if _, st, _ = do(http.MethodGet, ""); st.Enabled || st.IntervalMinutes != 30 {
		t.Fatalf("GET after PUT: %+v", st)
	}

	for name, body := range map[string]string{
		"interval 4":    `{"enabled":true,"interval_minutes":4}`,
		"interval 1441": `{"enabled":true,"interval_minutes":1441}`,
		"bad json":      `{"enabled":`,
		"no enabled":    `{"interval_minutes":30}`,
		"null interval": `{"enabled":true,"interval_minutes":null}`,
	} {
		if code, _, _ := do(http.MethodPut, body); code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, code)
		}
	}
	if _, st, _ = do(http.MethodGet, ""); st.Enabled || st.IntervalMinutes != 30 {
		t.Errorf("rejected PUTs changed config: %+v", st)
	}
}
