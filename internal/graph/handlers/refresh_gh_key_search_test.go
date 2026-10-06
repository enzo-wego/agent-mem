package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/extractor"
	"github.com/agent-mem/agent-mem/internal/graph/ids"
)

var (
	ghStart = time.Date(2026, 10, 6, 6, 0, 0, 0, time.UTC)
	ghBase  = time.Date(2026, 10, 6, 4, 0, 0, 0, time.UTC)
)

func ghTime(base time.Time, secs int) string {
	return base.Add(time.Duration(secs) * time.Second).Format(ghKeyTimeFmt)
}

// ghKeyReset clears graph tables and gh_key_search_* settings.
func ghKeyReset(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	truncateGraphHandlerTables(t, pool)
	if _, err := pool.Exec(context.Background(), `DELETE FROM settings WHERE key LIKE 'gh\_key\_search\_%'`); err != nil {
		t.Fatalf("clear settings: %v", err)
	}
}

func ghKeySetup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := openTestDB(t)
	ghKeyReset(t, pool)
	t.Cleanup(func() { ghKeyReset(t, pool) })
	return pool
}

func seedJira(t *testing.T, pool *pgxpool.Pool, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO graph.nodes (id, type, natural_key, machine_id) VALUES ($1,'jira',$2,'m') ON CONFLICT DO NOTHING`, "jira:"+k, k); err != nil {
			t.Fatal(err)
		}
	}
}

func edgeCount(t *testing.T, pool *pgxpool.Pool, where string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM graph.edges WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

type ghClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *ghClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *ghClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}
func (c *ghClock) sleep(_ context.Context, d time.Duration) error {
	c.advance(d)
	return nil
}

func newGHTestRunner(pool *pgxpool.Pool, baseURL string) (*ghKeyRunner, *ghClock) {
	clk := &ghClock{t: ghStart}
	r := newGHKeyRunner(Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "m", GHToken: "tok", GHBaseURL: baseURL})
	r.now = clk.now
	r.sleep = clk.sleep
	return r, clk
}

func ghPR(n int, title, body, updated string) map[string]any {
	return map[string]any{
		"number": n, "title": title, "body": body,
		"html_url":       fmt.Sprintf("https://github.com/wego/payments/pull/%d", n),
		"repository_url": "https://api.github.com/repos/wego/payments",
		"updated_at":     updated,
		"pull_request":   map[string]any{},
	}
}

func ghJSON(total int, incomplete bool, items ...map[string]any) string {
	if items == nil {
		items = []map[string]any{}
	}
	b, _ := json.Marshal(map[string]any{"total_count": total, "incomplete_results": incomplete, "items": items})
	return string(b)
}

// ghFake records every request's q and page and answers via respond.
type ghFake struct {
	mu      sync.Mutex
	qs      []string
	pages   []string
	respond func(n int, q string, page int, w http.ResponseWriter)
}

func (f *ghFake) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.qs = append(f.qs, r.URL.Query().Get("q"))
		f.pages = append(f.pages, r.URL.Query().Get("page"))
		n := len(f.qs)
		f.mu.Unlock()
		page := 1
		fmt.Sscanf(r.URL.Query().Get("page"), "%d", &page)
		f.respond(n, r.URL.Query().Get("q"), page, w)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *ghFake) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.qs)
}

func (f *ghFake) q(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.qs[i]
}

// ghStatic answers every request with the same JSON.
func ghStatic(body string) *ghFake {
	return &ghFake{respond: func(_ int, _ string, _ int, w http.ResponseWriter) { _, _ = w.Write([]byte(body)) }}
}

// ghPaged answers page p with pages[p-1] (empty beyond).
func ghPaged(total int, pages ...[]map[string]any) *ghFake {
	return &ghFake{respond: func(_ int, _ string, page int, w http.ResponseWriter) {
		var items []map[string]any
		if page-1 < len(pages) {
			items = pages[page-1]
		}
		_, _ = w.Write([]byte(ghJSON(total, false, items...)))
	}}
}

func ghRun(t *testing.T, r *ghKeyRunner) { t.Helper(); r.run(context.Background()) }

func TestGHKeySearch_KeyMatching(t *testing.T) {
	pool := ghKeySetup(t)
	cases := []struct {
		name, title, body string
		linked            bool
	}{
		{"title", "PAY-2414: x", "", true},
		{"lowercase body", "x", "fixes pay-2414 in y", true},
		{"longer number", "PAY-24145", "", false},
		{"letter prefix", "XPAY-2414", "", false},
		{"digit prefix", "1PAY-2414", "", false},
		{"dash prefix", "X-PAY-2414", "", false},
		{"suffix dash", "PAY-2414-1", "", true},
		{"hash only", "#2414", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ghKeyReset(t, pool)
			seedJira(t, pool, "PAY-2414")
			srv := ghStatic(ghJSON(1, false, ghPR(5, c.title, c.body, ghTime(ghBase, 0)))).start(t)
			r, _ := newGHTestRunner(pool, srv.URL)
			ghRun(t, r)
			want := 0
			if c.linked {
				want = 1
			}
			if got := edgeCount(t, pool, `from_node_id='gh_pr:wego/payments#5' AND to_node_id='jira:PAY-2414'`); got != want {
				t.Fatalf("edges = %d, want %d", got, want)
			}
		})
	}
}

func TestGHKeySearch_MultipleAndDuplicateKeys(t *testing.T) {
	pool := ghKeySetup(t)
	seedJira(t, pool, "PAY-2414", "PAY-2415")
	srv := ghStatic(ghJSON(1, false, ghPR(5, "PAY-2414: x", "again PAY-2414 and PAY-2415", ghTime(ghBase, 0)))).start(t)
	r, _ := newGHTestRunner(pool, srv.URL)
	ghRun(t, r)
	if got := edgeCount(t, pool, `from_node_id='gh_pr:wego/payments#5'`); got != 2 {
		t.Fatalf("edges = %d, want 2", got)
	}
	if v, _ := setting(t, pool, ghKeyKeyLinked); v != "2" {
		t.Fatalf("last_linked = %q, want 2", v)
	}
}

func TestGHKeySearch_Filters(t *testing.T) {
	pool := ghKeySetup(t)
	seedJira(t, pool, "PAY-2414", "PAY-2416")
	if _, err := pool.Exec(context.Background(), `UPDATE graph.nodes SET deleted_at=now() WHERE id='jira:PAY-2416'`); err != nil {
		t.Fatal(err)
	}
	issue := ghPR(1, "PAY-2414 issue", "", ghTime(ghBase, 0))
	delete(issue, "pull_request")
	other := ghPR(2, "PAY-2414 other repo", "", ghTime(ghBase, 1))
	other["repository_url"] = "https://api.github.com/repos/other/thing"
	srv := ghStatic(ghJSON(4, false,
		issue, other,
		ghPR(3, "PAY-9999 PAY-2416 ADS-1", "", ghTime(ghBase, 2)),
		ghPR(4, "ADS-1 nothing", "", ghTime(ghBase, 3)),
	)).start(t)
	r, _ := newGHTestRunner(pool, srv.URL)
	ghRun(t, r)
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM graph.nodes WHERE type='gh_pr'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("gh_pr nodes = %d err=%v, want 0", n, err)
	}
	if got := edgeCount(t, pool, `true`); got != 0 {
		t.Fatalf("edges = %d, want 0", got)
	}
	if v, _ := setting(t, pool, ghKeyKeyUnmatched); v != "2" {
		t.Fatalf("last_unmatched = %q, want 2", v)
	}
	if v, _ := setting(t, pool, ghKeyKeyCursor); v != ghTime(ghBase, 3) {
		t.Fatalf("cursor = %q, want %q", v, ghTime(ghBase, 3))
	}
}

func ghTemplateRun(t *testing.T, seeded int) (*pgxpool.Pool, *ghKeyRunner) {
	pool := ghKeySetup(t)
	seedPRBoilerplateBodies(t, pool, seeded)
	seedJira(t, pool, "PAY-2142", "PAY-9001")
	srv := ghStatic(ghJSON(1, false, ghPR(11, "Fix thing", prTemplateTestLine+"\nFixes PAY-9001", ghTime(ghBase, 0)))).start(t)
	r, _ := newGHTestRunner(pool, srv.URL)
	return pool, r
}

func TestGHKeySearch_TemplateKeyStripped(t *testing.T) {
	pool, r := ghTemplateRun(t, 10)
	ghRun(t, r)
	if got := edgeCount(t, pool, `from_node_id='gh_pr:wego/payments#11' AND to_node_id='jira:PAY-9001'`); got != 1 {
		t.Fatalf("PAY-9001 edges = %d, want 1", got)
	}
	if got := edgeCount(t, pool, `from_node_id='gh_pr:wego/payments#11' AND to_node_id='jira:PAY-2142'`); got != 0 {
		t.Fatalf("PAY-2142 edges = %d, want 0", got)
	}
}

func TestGHKeySearch_TemplateBelowThreshold(t *testing.T) {
	pool, r := ghTemplateRun(t, 9)
	ghRun(t, r)
	if got := edgeCount(t, pool, `from_node_id='gh_pr:wego/payments#11' AND to_node_id='jira:PAY-2142'`); got != 1 {
		t.Fatalf("PAY-2142 edges = %d, want 1 (accepted limit)", got)
	}
}

func TestGHKeySearch_TemplateLookupError(t *testing.T) {
	pool := ghKeySetup(t)
	seedJira(t, pool, "PAY-2414", "PAY-9001")
	srv := ghStatic(ghJSON(2, false,
		ghPR(1, "PAY-2414: plain", "", ghTime(ghBase, 0)),
		ghPR(2, "PAY-9001", prTemplateTestLine, ghTime(ghBase, 1)),
	)).start(t)
	r, _ := newGHTestRunner(pool, srv.URL)
	broken := openTestDB(t)
	r.stripDB = broken
	n := 0
	r.afterItem = func() {
		if n++; n == 1 {
			broken.Close()
		}
	}
	ghRun(t, r)
	if got := edgeCount(t, pool, `from_node_id='gh_pr:wego/payments#1'`); got != 1 {
		t.Fatalf("item 1 edges = %d, want 1", got)
	}
	if got := edgeCount(t, pool, `from_node_id='gh_pr:wego/payments#2'`); got != 0 {
		t.Fatalf("item 2 edges = %d, want 0", got)
	}
	if v, _ := setting(t, pool, ghKeyKeyCursor); v != ghTime(ghBase, 0) {
		t.Fatalf("cursor = %q, want item 1's time", v)
	}
	if v, _ := setting(t, pool, ghKeyKeyLastErr); v == "" {
		t.Fatal("last_error empty, want failure")
	}
}

func TestStripPRBoilerplate_WrapperUnchanged(t *testing.T) {
	pool := openTestDB(t)
	pool.Close()
	body := prTemplateTestLine + "\nFixes PAY-9001"
	if got := stripPRBoilerplate(context.Background(), pool, "gh_pr:wego/payments#11", body); got != body {
		t.Fatalf("body = %q, want unchanged", got)
	}
	if _, err := stripPRBoilerplateErr(context.Background(), pool, "gh_pr:wego/payments#11", body); err == nil {
		t.Fatal("stripPRBoilerplateErr: want lookup error")
	}
}

func TestGHKeySearch_NewPRForOldTicket(t *testing.T) {
	pool := ghKeySetup(t)
	seedJira(t, pool, "PAY-2414")
	if _, err := pool.Exec(context.Background(), `UPDATE graph.nodes SET body_ts='2020-01-01T00:00:00Z' WHERE id='jira:PAY-2414'`); err != nil {
		t.Fatal(err)
	}
	if err := putSetting(context.Background(), pool, ghKeyKeyCursor, "2026-10-06T04:00:00Z"); err != nil {
		t.Fatal(err)
	}
	f := ghStatic(ghJSON(1, false, ghPR(9, "PAY-2414: new work", "", "2026-10-06T05:00:00Z")))
	srv := f.start(t)
	r, _ := newGHTestRunner(pool, srv.URL)
	ghRun(t, r)
	if got := edgeCount(t, pool, `from_node_id='gh_pr:wego/payments#9' AND to_node_id='jira:PAY-2414'`); got != 1 {
		t.Fatalf("edges = %d, want 1", got)
	}
	if !strings.Contains(f.q(0), "updated:2026-10-06T03:50:00Z..2026-10-06T05:58:00Z") {
		t.Fatalf("q = %q", f.q(0))
	}
}

func ghRange(from, n int, base time.Time, title func(i int) string) []map[string]any {
	var out []map[string]any
	for i := from; i < from+n; i++ {
		out = append(out, ghPR(i, title(i), "", ghTime(base, i)))
	}
	return out
}

func TestGHKeySearch_Pagination(t *testing.T) {
	pool := ghKeySetup(t)
	seedJira(t, pool, "PAY-2414")
	plain := func(int) string { return "chore" }
	p2 := ghRange(101, 50, ghBase, plain)
	p2[19] = ghPR(120, "PAY-2414: match", "", ghTime(ghBase, 120))
	f := ghPaged(150, ghRange(1, 100, ghBase, plain), p2)
	srv := f.start(t)
	r, _ := newGHTestRunner(pool, srv.URL)
	ghRun(t, r)
	if got := edgeCount(t, pool, `from_node_id='gh_pr:wego/payments#120'`); got != 1 {
		t.Fatalf("edges = %d, want 1", got)
	}
	if f.requests() != 2 {
		t.Fatalf("requests = %d, want 2", f.requests())
	}
	if v, _ := setting(t, pool, ghKeyKeyCursor); v != ghTime(ghBase, 150) {
		t.Fatalf("cursor = %q, want %q", v, ghTime(ghBase, 150))
	}
}

func TestGHKeySearch_Requery(t *testing.T) {
	t.Run("second query starts at cursor", func(t *testing.T) {
		pool := ghKeySetup(t)
		f := &ghFake{respond: func(n int, _ string, _ int, w http.ResponseWriter) {
			if n <= 10 {
				_, _ = w.Write([]byte(ghJSON(1500, false, ghRange((n-1)*100+1, 100, ghBase, func(int) string { return "chore" })...)))
				return
			}
			_, _ = w.Write([]byte(ghJSON(5, false, ghRange(1001, 5, ghBase, func(int) string { return "chore" })...)))
		}}
		srv := f.start(t)
		r, _ := newGHTestRunner(pool, srv.URL)
		ghRun(t, r)
		if f.requests() != 11 {
			t.Fatalf("requests = %d, want 11", f.requests())
		}
		if want := "updated:" + ghTime(ghBase, 1000) + ".."; !strings.Contains(f.q(10), want) {
			t.Fatalf("11th q = %q, want it to contain %q", f.q(10), want)
		}
		if v, _ := setting(t, pool, ghKeyKeyCursor); v != ghTime(ghBase, 1005) {
			t.Fatalf("cursor = %q", v)
		}
		if v, _ := setting(t, pool, ghKeyKeyLastErr); v != "" {
			t.Fatalf("last_error = %q", v)
		}
	})
	t.Run("stalled", func(t *testing.T) {
		pool := ghKeySetup(t)
		same := ghTime(ghBase, 0)
		f := &ghFake{respond: func(n int, _ string, _ int, w http.ResponseWriter) {
			var items []map[string]any
			for i := range 100 {
				items = append(items, ghPR(n*1000+i, "chore", "", same))
			}
			_, _ = w.Write([]byte(ghJSON(1500, false, items...)))
		}}
		srv := f.start(t)
		r, _ := newGHTestRunner(pool, srv.URL)
		ghRun(t, r)
		if v, _ := setting(t, pool, ghKeyKeyLastErr); v != "stalled at "+same {
			t.Fatalf("last_error = %q", v)
		}
	})
}

func TestGHKeySearch_IncompleteResults(t *testing.T) {
	pool := ghKeySetup(t)
	seedJira(t, pool, "PAY-2414")
	p1 := ghRange(1, 100, ghBase, func(int) string { return "chore" })
	p1[5] = ghPR(6, "PAY-2414", "", ghTime(ghBase, 6))
	f := &ghFake{respond: func(_ int, _ string, page int, w http.ResponseWriter) {
		if page == 1 {
			_, _ = w.Write([]byte(ghJSON(150, false, p1...)))
			return
		}
		_, _ = w.Write([]byte(ghJSON(150, true, ghRange(101, 50, ghBase, func(int) string { return "chore" })...)))
	}}
	srv := f.start(t)
	r, _ := newGHTestRunner(pool, srv.URL)
	ghRun(t, r)
	if v, _ := setting(t, pool, ghKeyKeyLastErr); v == "" {
		t.Fatal("last_error empty")
	}
	if v, _ := setting(t, pool, ghKeyKeyCursor); v != ghTime(ghBase, 100) {
		t.Fatalf("cursor = %q, want page 1's last item", v)
	}
	if got := edgeCount(t, pool, `from_node_id='gh_pr:wego/payments#6'`); got != 1 {
		t.Fatalf("page-1 edges = %d, want 1", got)
	}
}

func TestGHKeySearch_Failures(t *testing.T) {
	const preset = "2026-10-06T04:00:00Z"
	status := func(code int) func(w http.ResponseWriter) {
		return func(w http.ResponseWriter) { w.WriteHeader(code) }
	}
	cases := []struct {
		name   string
		handle func(w http.ResponseWriter)
	}{
		{"401", status(401)}, {"403", status(403)}, {"429", status(429)}, {"500", status(500)},
		{"invalid json", func(w http.ResponseWriter) { _, _ = w.Write([]byte("{")) }},
		{"timeout", func(w http.ResponseWriter) { time.Sleep(300 * time.Millisecond) }},
		{"transport", nil},
		{"cancelled", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool := ghKeySetup(t)
			if err := putSetting(context.Background(), pool, ghKeyKeyCursor, preset); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := &ghFake{respond: func(_ int, _ string, _ int, w http.ResponseWriter) {
				if c.name == "cancelled" {
					cancel()
					time.Sleep(200 * time.Millisecond)
					return
				}
				c.handle(w)
			}}
			srv := f.start(t)
			r, _ := newGHTestRunner(pool, srv.URL)
			switch c.name {
			case "timeout":
				r.reqTimeout = 50 * time.Millisecond
			case "transport":
				srv.Close()
			}
			r.run(ctx)
			if v, _ := setting(t, pool, ghKeyKeyCursor); v != preset {
				t.Fatalf("cursor = %q, want unchanged", v)
			}
			if v, _ := setting(t, pool, ghKeyKeyLastErr); v == "" {
				t.Fatal("last_error empty")
			}
			if v, _ := setting(t, pool, ghKeyKeyLastRun); v != "2026-10-06T06:00:00Z" {
				t.Fatalf("last_run_at = %q", v)
			}
		})
	}
}

func ghCountJobs(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM graph.jobs WHERE type='refresh_gh_key_search'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestGHKeySearch_LastRunAtLifecycle(t *testing.T) {
	pool := ghKeySetup(t)
	srv := (&ghFake{respond: func(_ int, _ string, _ int, w http.ResponseWriter) { w.WriteHeader(401) }}).start(t)
	r, _ := newGHTestRunner(pool, srv.URL)
	ghRun(t, r)
	if err := putSetting(context.Background(), pool, ghKeyKeyEnabled, "true"); err != nil {
		t.Fatal(err)
	}
	env := ghKeyEnv{MachineID: "m"}
	if err := ghKeySearchTick(context.Background(), pool, env, ghStart.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n := ghCountJobs(t, pool); n != 0 {
		t.Fatalf("jobs within interval = %d, want 0", n)
	}
	if err := ghKeySearchTick(context.Background(), pool, env, ghStart.Add(61*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n := ghCountJobs(t, pool); n != 1 {
		t.Fatalf("jobs after interval = %d, want 1", n)
	}
}

func TestGHKeySearch_NoToken(t *testing.T) {
	pool := ghKeySetup(t)
	f := ghStatic(ghJSON(0, false))
	srv := f.start(t)
	r, _ := newGHTestRunner(pool, srv.URL)
	r.deps.GHToken = ""
	ghRun(t, r)
	if v, _ := setting(t, pool, ghKeyKeyLastErr); v != "github token not set" {
		t.Fatalf("last_error = %q", v)
	}
	if _, ok := setting(t, pool, ghKeyKeyLastRun); !ok {
		t.Fatal("last_run_at not set")
	}
	if f.requests() != 0 {
		t.Fatalf("requests = %d, want 0", f.requests())
	}
}

func TestGHKeySearch_CursorStates(t *testing.T) {
	cases := []struct {
		name         string
		cursor, date string
		wantReq      int
		wantQ        string
		wantErr      string
	}{
		{name: "no cursor default start", date: "2025-01-01", wantReq: 1, wantQ: "updated:2024-12-31T23:50:00Z.."},
		{name: "no cursor future start", date: "2027-01-01", wantReq: 0},
		{name: "cursor after end", cursor: "2026-10-06T05:59:00Z", wantReq: 0},
		{name: "malformed cursor", cursor: "garbage", wantReq: 0, wantErr: "cursor unparsable: garbage"},
		{name: "cursor before end", cursor: "2026-10-06T05:57:00Z", wantReq: 1, wantQ: "updated:2026-10-06T05:47:00Z.."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool := ghKeySetup(t)
			ctx := context.Background()
			if c.cursor != "" {
				_ = putSetting(ctx, pool, ghKeyKeyCursor, c.cursor)
			}
			if c.date != "" {
				_ = putSetting(ctx, pool, ghKeyKeyStartDate, c.date)
			}
			f := ghStatic(ghJSON(0, false))
			srv := f.start(t)
			r, _ := newGHTestRunner(pool, srv.URL)
			ghRun(t, r)
			if f.requests() != c.wantReq {
				t.Fatalf("requests = %d, want %d", f.requests(), c.wantReq)
			}
			if c.wantQ != "" && !strings.Contains(f.q(0), c.wantQ) {
				t.Fatalf("q = %q, want %q", f.q(0), c.wantQ)
			}
			if c.cursor != "" {
				if v, _ := setting(t, pool, ghKeyKeyCursor); v != c.cursor {
					t.Fatalf("cursor = %q, want unchanged %q", v, c.cursor)
				}
			}
			errv, _ := setting(t, pool, ghKeyKeyLastErr)
			if errv != c.wantErr {
				t.Fatalf("last_error = %q, want %q", errv, c.wantErr)
			}
			if c.wantErr == "" {
				if _, ok := setting(t, pool, ghKeyKeyLastOK); !ok {
					t.Fatal("success write missing")
				}
			}
		})
	}
}

func ghSixItems(t *testing.T, pool *pgxpool.Pool) []map[string]any {
	var items []map[string]any
	for i := 1; i <= 6; i++ {
		seedJira(t, pool, fmt.Sprintf("PAY-%d", 2500+i))
		items = append(items, ghPR(i, fmt.Sprintf("PAY-%d", 2500+i), "", ghTime(ghBase, i*60)))
	}
	return items
}

func TestGHKeySearch_BudgetStopsBetweenItems(t *testing.T) {
	pool := ghKeySetup(t)
	items := ghSixItems(t, pool)
	f := ghStatic(ghJSON(6, false, items...))
	srv := f.start(t)
	r, clk := newGHTestRunner(pool, srv.URL)
	n := 0
	r.afterItem = func() {
		if n++; n == 3 {
			clk.advance(241 * time.Second)
		}
	}
	ghRun(t, r)
	if v, _ := setting(t, pool, ghKeyKeyCursor); v != ghTime(ghBase, 180) {
		t.Fatalf("cursor = %q, want item 3's time %q", v, ghTime(ghBase, 180))
	}
	if v, _ := setting(t, pool, ghKeyKeyLastErr); v != "" {
		t.Fatalf("last_error = %q", v)
	}
	if v, _ := setting(t, pool, ghKeyKeyLinked); v != "3" {
		t.Fatalf("run 1 last_linked = %q, want 3", v)
	}

	r2, _ := newGHTestRunner(pool, srv.URL)
	ghRun(t, r2)
	if want := "updated:" + ghTime(ghBase, 180-600) + ".."; !strings.Contains(f.q(1), want) {
		t.Fatalf("run 2 q = %q, want %q", f.q(1), want)
	}
	if v, _ := setting(t, pool, ghKeyKeyLinked); v != "3" {
		t.Fatalf("run 2 last_linked = %q, want 3 (items 4-6 only)", v)
	}
	if got := edgeCount(t, pool, `source_msg_id='gh_key_search'`); got != 6 {
		t.Fatalf("edges = %d, want 6", got)
	}
}

func TestGHKeySearch_SlowItemsAdvance(t *testing.T) {
	pool := ghKeySetup(t)
	base := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	var items []map[string]any
	for i := range 7 {
		seedJira(t, pool, fmt.Sprintf("PAY-%d", 2600+i))
		items = append(items, ghPR(i+1, fmt.Sprintf("PAY-%d", 2600+i), "", ghTime(base, i*20*60)))
	}
	re := regexp.MustCompile(`updated:(\S+)\.\.(\S+)`)
	f := &ghFake{respond: func(_ int, q string, _ int, w http.ResponseWriter) {
		from, _ := time.Parse(time.RFC3339, re.FindStringSubmatch(q)[1])
		var out []map[string]any
		for _, it := range items {
			if ut, _ := time.Parse(time.RFC3339, it["updated_at"].(string)); !ut.Before(from) {
				out = append(out, it)
			}
		}
		_, _ = w.Write([]byte(ghJSON(len(out), false, out...)))
	}}
	srv := f.start(t)
	prev := ""
	for run := 1; run <= 3; run++ {
		r, clk := newGHTestRunner(pool, srv.URL)
		r.afterItem = func() { clk.advance(90 * time.Second) }
		ghRun(t, r)
		cur, _ := setting(t, pool, ghKeyKeyCursor)
		if cur <= prev {
			t.Fatalf("run %d: cursor %q did not move past %q", run, cur, prev)
		}
		prev = cur
	}
	if got := edgeCount(t, pool, `source_msg_id='gh_key_search'`); got != 7 {
		t.Fatalf("edges after 3 runs = %d, want 7", got)
	}
}

func TestGHKeySearch_CursorMonotonicChronological(t *testing.T) {
	cases := []struct{ name, stored, item, want string }{
		{"offset cursor moves", "2026-10-06T01:00:00+01:00", "2026-10-06T00:30:00Z", "2026-10-06T00:30:00Z"},
		{"fractional cursor stays", "2026-10-06T00:45:00.500Z", "2026-10-06T00:30:00Z", "2026-10-06T00:45:00.500Z"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool := ghKeySetup(t)
			_ = putSetting(context.Background(), pool, ghKeyKeyCursor, c.stored)
			srv := ghStatic(ghJSON(1, false, ghPR(1, "chore", "", c.item))).start(t)
			r, _ := newGHTestRunner(pool, srv.URL)
			ghRun(t, r)
			if v, _ := setting(t, pool, ghKeyKeyCursor); v != c.want {
				t.Fatalf("cursor = %q, want %q", v, c.want)
			}
		})
	}
}

func TestGHKeySearch_SuccessWriteFails(t *testing.T) {
	pool := ghKeySetup(t)
	seedJira(t, pool, "PAY-2414")
	srv := ghStatic(ghJSON(1, false, ghPR(1, "PAY-2414", "", ghTime(ghBase, 0)))).start(t)
	r, _ := newGHTestRunner(pool, srv.URL)
	r.successWrite = func(context.Context, int, int, time.Time) error { return fmt.Errorf("injected") }
	ghRun(t, r)
	if v, _ := setting(t, pool, ghKeyKeyCursor); v != ghTime(ghBase, 0) {
		t.Fatalf("cursor = %q", v)
	}
	if _, ok := setting(t, pool, ghKeyKeyLastOK); ok {
		t.Fatal("last_ok_at written despite failing success write")
	}
}

func TestGHKeySearch_AtomicItem(t *testing.T) {
	pool := ghKeySetup(t)
	seedJira(t, pool, "PAY-2414")
	const preset = "2026-10-06T03:00:00Z"
	_ = putSetting(context.Background(), pool, ghKeyKeyCursor, preset)
	srv := ghStatic(ghJSON(1, false, ghPR(1, "PAY-2414", "", ghTime(ghBase, 0)))).start(t)
	r, _ := newGHTestRunner(pool, srv.URL)
	r.beforeFetchJob = func() error { return fmt.Errorf("injected job failure") }
	ghRun(t, r)
	var nodes int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM graph.nodes WHERE type='gh_pr'`).Scan(&nodes); err != nil || nodes != 0 {
		t.Fatalf("gh_pr nodes = %d err=%v, want 0", nodes, err)
	}
	if got := edgeCount(t, pool, `true`); got != 0 {
		t.Fatalf("edges = %d, want 0", got)
	}
	if v, _ := setting(t, pool, ghKeyKeyCursor); v != preset {
		t.Fatalf("cursor = %q, want %q", v, preset)
	}
	if v, _ := setting(t, pool, ghKeyKeyLastErr); v == "" {
		t.Fatal("last_error empty")
	}
}

func TestGHKeySearch_FetchQueuing(t *testing.T) {
	pool := ghKeySetup(t)
	ctx := context.Background()
	seedJira(t, pool, "PAY-2414")
	if _, err := pool.Exec(ctx, `INSERT INTO graph.nodes (id,type,natural_key,body,machine_id) VALUES ('gh_pr:wego/payments#2','gh_pr','wego/payments#2','has a body','m')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO graph.nodes (id,type,natural_key,machine_id) VALUES ('gh_pr:wego/payments#3','gh_pr','wego/payments#3','m')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO graph.jobs (type,payload,machine_id,available_at) VALUES ('fetch_body','{"node_id":"gh_pr:wego/payments#3"}','m', now() + interval '2 days')`); err != nil {
		t.Fatal(err)
	}
	srv := ghStatic(ghJSON(3, false,
		ghPR(1, "PAY-2414 new", "", ghTime(ghBase, 0)),
		ghPR(2, "PAY-2414 has body", "", ghTime(ghBase, 1)),
		ghPR(3, "PAY-2414 parked fetch", "", ghTime(ghBase, 2)),
	)).start(t)
	jobs := func(n int) int {
		var c int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph.jobs WHERE type='fetch_body' AND payload->>'node_id'=$1`,
			fmt.Sprintf("gh_pr:wego/payments#%d", n)).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	for run := 1; run <= 2; run++ {
		r, _ := newGHTestRunner(pool, srv.URL)
		ghRun(t, r)
		if jobs(1) != 1 || jobs(2) != 0 || jobs(3) != 1 {
			t.Fatalf("run %d jobs #1=%d #2=%d #3=%d, want 1/0/1", run, jobs(1), jobs(2), jobs(3))
		}
	}
	var depth *string
	if err := pool.QueryRow(ctx, `SELECT payload->>'depth' FROM graph.jobs WHERE payload->>'node_id'='gh_pr:wego/payments#1'`).Scan(&depth); err != nil || depth != nil {
		t.Fatalf("depth = %v err=%v, want absent (0)", depth, err)
	}
}

func TestGHKeySearch_Pacing(t *testing.T) {
	pool := ghKeySetup(t)
	r, clk := newGHTestRunner(pool, "http://unused")
	var times []time.Time
	for range 30 {
		if err := r.pace(context.Background()); err != nil {
			t.Fatal(err)
		}
		times = append(times, clk.now())
		clk.advance(time.Second)
	}
	for i := range times {
		n := 0
		for j := i; j < len(times) && times[j].Sub(times[i]) < time.Minute; j++ {
			n++
		}
		if n > ghKeyMaxPerMinute {
			t.Fatalf("%d requests within 60 s starting at request %d", n, i)
		}
	}
	if times[29].Sub(times[0]) < time.Minute {
		t.Fatalf("30 requests spanned %v, want pacing to push past 60 s", times[29].Sub(times[0]))
	}
}

func TestGHKeySearch_EdgeOwnership(t *testing.T) {
	pool := ghKeySetup(t)
	ctx := context.Background()
	seedJira(t, pool, "PAY-1")
	srv := ghStatic(ghJSON(1, false, ghPR(7, "PAY-1", "", ghTime(ghBase, 0)))).start(t)
	r, _ := newGHTestRunner(pool, srv.URL)
	ghRun(t, r)
	const pr = "gh_pr:wego/payments#7"
	deps := Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "m"}
	owner := func(to string) string {
		var s *string
		if err := pool.QueryRow(ctx, `SELECT source_msg_id FROM graph.edges WHERE from_node_id=$1 AND to_node_id=$2`, pr, to).Scan(&s); err != nil || s == nil {
			return ""
		}
		return *s
	}
	f1 := extractor.Finding{NodeID: "jira:PAY-1", Type: ids.NodeType("jira"), EdgeKind: "REFERENCES"}
	if _, err := reconcileEdges(ctx, deps, pr, []extractor.Finding{f1}); err != nil {
		t.Fatal(err)
	}
	if o := owner("jira:PAY-1"); o != "gh_key_search" {
		t.Fatalf("owner after reconcile = %q, want gh_key_search", o)
	}
	f2 := extractor.Finding{NodeID: "jira:PAY-2", Type: ids.NodeType("jira"), EdgeKind: "REFERENCES"}
	if _, err := reconcileEdges(ctx, deps, pr, []extractor.Finding{f2}); err != nil {
		t.Fatal(err)
	}
	if o := owner("jira:PAY-2"); o != pr {
		t.Fatalf("ordinary edge owner = %q, want %q", o, pr)
	}
	keep, err := reconcileEdges(ctx, deps, pr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := pruneStaleEdges(ctx, deps, pr, keep); err != nil {
		t.Fatal(err)
	}
	if o := owner("jira:PAY-1"); o != "gh_key_search" {
		t.Fatalf("search edge after prune: owner %q, want it kept", o)
	}
	if got := edgeCount(t, pool, `from_node_id=$1 AND to_node_id='jira:PAY-2'`, pr); got != 0 {
		t.Fatalf("ordinary edge survived prune (%d)", got)
	}
}

func TestGHKeySearch_Endpoint(t *testing.T) {
	pool := ghKeySetup(t)
	h := NewGHKeySearchHandler(Deps{DB: pool})
	do := func(method, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/graph/gh-key-search", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	const fresh = `{"enabled":false,"interval_minutes":60,"start_date":"2025-01-01","cursor":null,"last_run_at":null,"last_ok_at":null,"last_error":"","last_linked":null,"last_unmatched":null}`
	if rec := do("GET", ""); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != fresh {
		t.Fatalf("fresh GET = %d %s", rec.Code, rec.Body.String())
	}
	rec := do("PUT", `{"enabled":true,"interval_minutes":120,"start_date":"2025-03-01"}`)
	want := `{"enabled":true,"interval_minutes":120,"start_date":"2025-03-01","cursor":null,"last_run_at":null,"last_ok_at":null,"last_error":"","last_linked":null,"last_unmatched":null}`
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do("GET", ""); strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("GET after PUT = %s", rec.Body.String())
	}
	for name, body := range map[string]string{
		"interval 10":   `{"enabled":true,"interval_minutes":10,"start_date":"2025-03-01"}`,
		"missing field": `{"enabled":true,"interval_minutes":60}`,
		"bad date":      `{"enabled":true,"interval_minutes":60,"start_date":"2025-13-01"}`,
	} {
		if rec := do("PUT", body); rec.Code != 400 {
			t.Fatalf("%s: code = %d, want 400", name, rec.Code)
		}
	}
	if rec := do("GET", ""); strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("rejected PUT changed state: %s", rec.Body.String())
	}
}

func TestGHKeySearch_Ticker(t *testing.T) {
	pool := ghKeySetup(t)
	ctx := context.Background()
	env := ghKeyEnv{MachineID: "m"}
	now := ghStart
	tick := func() {
		if err := ghKeySearchTick(ctx, pool, env, now); err != nil {
			t.Fatal(err)
		}
	}
	tick()
	if n := ghCountJobs(t, pool); n != 0 {
		t.Fatalf("disabled: jobs = %d", n)
	}
	_ = putSetting(ctx, pool, ghKeyKeyEnabled, "true")
	_ = putSetting(ctx, pool, ghKeyKeyLastRun, ghStart.Add(-10*time.Minute).Format(time.RFC3339))
	tick()
	if n := ghCountJobs(t, pool); n != 0 {
		t.Fatalf("recent run: jobs = %d", n)
	}
	_ = putSetting(ctx, pool, ghKeyKeyLastRun, ghStart.Add(-2*time.Hour).Format(time.RFC3339))
	tick()
	if n := ghCountJobs(t, pool); n != 1 {
		t.Fatalf("due: jobs = %d, want 1", n)
	}
	tick()
	if n := ghCountJobs(t, pool); n != 1 {
		t.Fatalf("queued duplicate: jobs = %d", n)
	}
	if _, err := pool.Exec(ctx, `UPDATE graph.jobs SET status='running' WHERE type='refresh_gh_key_search'`); err != nil {
		t.Fatal(err)
	}
	tick()
	if n := ghCountJobs(t, pool); n != 1 {
		t.Fatalf("running duplicate: jobs = %d", n)
	}
}
