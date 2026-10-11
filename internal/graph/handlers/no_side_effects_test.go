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
	fixture, err := os.ReadFile(compatibilityFixture)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = normal.Exec(context.Background(), string(fixture)); err != nil {
		t.Fatal(err)
	}
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
	cases := []struct {
		method, target, body string
		handler              http.Handler
		status               int
	}{{"POST", "/api/graph/resolve", `{"seeds":["jira:GC0-MISS"],"no_side_effects":true}`, resolve, 200}, {"POST", "/api/graph/resolve", `{}`, resolve, 400}, {"GET", "/api/graph/search?q=phase+zero", "", search, 200}, {"GET", "/api/graph/search?q=phase+zero&match=hybrid", "", search, 200}, {"GET", "/api/graph/search", "", search, 400}}
	for _, tc := range cases {
		r := httptest.NewRequest(tc.method, tc.target, bytes.NewBufferString(tc.body))
		r.Header.Set("X-Graph-No-Side-Effects", "1")
		w := httptest.NewRecorder()
		tc.handler.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s %s status=%d body=%s", tc.method, tc.target, w.Code, w.Body.String())
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
	r := httptest.NewRequest("POST", "/api/graph/resolve", bytes.NewBufferString(`{"seeds":["jira:GC0-MISS"]}`))
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
