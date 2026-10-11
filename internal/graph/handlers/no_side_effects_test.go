package handlers

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	graphtrace "github.com/agent-mem/agent-mem/internal/graph/trace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type readOnlyErrorCollector struct {
	mu    sync.Mutex
	count int
}

func (*readOnlyErrorCollector) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}
func (c *readOnlyErrorCollector) TraceQueryEnd(_ context.Context, _ *pgx.Conn, d pgx.TraceQueryEndData) {
	var pe *pgconn.PgError
	if errors.As(d.Err, &pe) && pe.Code == "25006" {
		c.mu.Lock()
		c.count++
		c.mu.Unlock()
	}
}
func (c *readOnlyErrorCollector) Count() int { c.mu.Lock(); defer c.mu.Unlock(); return c.count }

func TestNoSideEffectsRequestsUseReadOnlyPoolAndSkipEnqueue(t *testing.T) {
	normal := compatibilityPool(t)
	resetEpicACL(t, normal)
	fixture, err := os.ReadFile(compatibilityFixture)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = normal.Exec(context.Background(), string(fixture)); err != nil {
		t.Fatal(err)
	}
	aclPerson(t, normal, 504, "ro-member@example.com", "slack:CGC0")
	cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	collector := &readOnlyErrorCollector{}
	cfg.ConnConfig.Tracer = collector
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET default_transaction_read_only = on")
		return err
	}
	ro, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ro.Close)
	resolve, err := NewResolve(ro)
	if err != nil {
		t.Fatal(err)
	}
	enqueues := 0
	resolve.enqueueFetchBodyFn = func(context.Context, string) { enqueues++ }
	search, err := NewSearchWithEmbedder(ro, compatibilityEmbedder{})
	if err != nil {
		t.Fatal(err)
	}
	// Request handlers run on the read-only pool; the trace row is written
	// through the normal pool, outside the measured path.
	recorder := graphtrace.NewRecorder(graphtrace.PostgresStore{DB: normal}, graphtrace.Options{})
	t.Cleanup(func() { recorder.Close(5 * time.Second) })
	cases := []struct {
		route, method, target, body string
		headers                     map[string]string
		handler                     http.Handler
		status                      int
		bodyContains                string
	}{
		{"resolve", "POST", "/api/graph/resolve", `{"seeds":["slack:CGC0:100.000001"],"include_bodies":true}`, map[string]string{"X-Graph-No-Side-Effects": "TrUe"}, resolve, 200, `"body":"phase zero root body"`},
		{"resolve", "POST", "/api/graph/resolve", `{"seeds":["jira:GC0-MISS"]}`, map[string]string{"X-Graph-No-Side-Effects": "TrUe"}, resolve, 200, `"cache_misses":["jira:GC0-MISS"]`},
		{"resolve", "POST", "/api/graph/resolve", `{"seeds":["jira:GC0-MISS"],"no_side_effects":true}`, nil, resolve, 200, `"cache_misses":["jira:GC0-MISS"]`},
		{"resolve", "POST", "/api/graph/resolve", `{"seeds":["slack:CGC0:100.000001"],"asker_eeid":504,"include_bodies":true}`, map[string]string{"X-Graph-No-Side-Effects": "1"}, resolve, 200, `"body":"phase zero root body"`},
		{"resolve", "POST", "/api/graph/resolve", `{}`, map[string]string{"X-Graph-No-Side-Effects": "1"}, resolve, 400, "seeds required"},
		{"search", "GET", "/api/graph/search?q=phase+zero", "", map[string]string{"X-Graph-No-Side-Effects": "1", "X-Asker-User": "UUNKNOWN"}, search, 200, `"results"`},
		{"search", "GET", "/api/graph/search?q=phase+zero", "", map[string]string{"X-Graph-No-Side-Effects": "1", "X-Asker-User": "ro-member@example.com"}, search, 200, `"node_id":"slack:CGC0:100.000001"`},
		{"search", "GET", "/api/graph/search?q=phase+zero&match=hybrid", "", map[string]string{"X-Graph-No-Side-Effects": "true"}, search, 200, `"results"`},
		{"search", "GET", "/api/graph/search", "", map[string]string{"X-Graph-No-Side-Effects": "1"}, search, 400, "q required"},
	}
	for _, tc := range cases {
		r := adminRequest(tc.method, tc.target, bytes.NewBufferString(tc.body))
		for key, value := range tc.headers {
			r.Header.Set(key, value)
		}
		w := httptest.NewRecorder()
		withContextTrace(tc.handler, tc.route, "no-side-effects", recorder).ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s %s status=%d body=%s", tc.method, tc.target, w.Code, w.Body.String())
		}
		if !bytes.Contains(w.Body.Bytes(), []byte(tc.bodyContains)) {
			t.Fatalf("%s %s body=%q missing %q", tc.method, tc.target, w.Body.String(), tc.bodyContains)
		}
	}
	if enqueues != 0 {
		t.Fatalf("enqueues with flag=%d", enqueues)
	}
	if collector.Count() != 0 {
		t.Fatalf("write errors during flagged requests=%d", collector.Count())
	}
	if _, err := ro.Exec(context.Background(), `UPDATE graph.nodes SET title=title WHERE id='jira:GC0-1'`); sqlState(err) != "25006" {
		t.Fatalf("UPDATE error=%v", err)
	}
	if _, err := ro.Exec(context.Background(), `INSERT INTO graph.nodes(id,type,natural_key,machine_id) VALUES('jira:WRITE-CONTROL','jira','jira:WRITE-CONTROL','test')`); sqlState(err) != "25006" {
		t.Fatalf("INSERT error=%v", err)
	}
	resolve.enqueueFetchBodyFn = func(context.Context, string) { enqueues++ }
	r := adminRequest("POST", "/api/graph/resolve", bytes.NewBufferString(`{"seeds":["jira:GC0-MISS"]}`))
	w := httptest.NewRecorder()
	resolve.ServeHTTP(w, r)
	if enqueues == 0 {
		t.Fatal("unflagged cache miss did not enqueue")
	}
}
func sqlState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}
