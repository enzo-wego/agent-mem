package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	graphtrace "github.com/agent-mem/agent-mem/internal/graph/trace"
	"github.com/jackc/pgx/v5/pgxpool"
)

func compatibilityPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil || cfg.ConnConfig.Database != "agentmem_test" {
		t.Fatalf("DATABASE_URL must select agentmem_test: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestContextTraceResolveAndSearchResponses(t *testing.T) {
	pool := compatibilityPool(t)
	fixture, err := os.ReadFile(compatibilityFixture)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(context.Background(), string(fixture)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM graph.context_traces WHERE build_rev='rev-test'`); err != nil {
		t.Fatal(err)
	}
	recorder := graphtrace.NewRecorder(graphtrace.PostgresStore{DB: pool}, graphtrace.Options{})
	resolve, _ := NewResolve(pool)
	search, _ := NewSearchWithEmbedder(pool, compatibilityEmbedder{})
	cases := []struct {
		name, route, method, target, body string
		handler                           http.Handler
		status                            int
	}{{"resolve-ok", "resolve", "POST", "/api/graph/resolve", `{"seeds":["slack:CGC0:100.000001"]}`, resolve, 200}, {"resolve-error", "resolve", "POST", "/api/graph/resolve", `{}`, resolve, 400}, {"search-ordinary", "search", "GET", "/api/graph/search?q=phase+zero", "", search, 200}, {"search-hybrid", "search", "GET", "/api/graph/search?q=phase+zero&match=hybrid", "", search, 200}, {"search-error", "search", "GET", "/api/graph/search", "", search, 400}}
	for _, tc := range cases {
		r := httptest.NewRequest(tc.method, tc.target, bytes.NewBufferString(tc.body))
		w := httptest.NewRecorder()
		withContextTrace(tc.handler, tc.route, "rev-test", recorder).ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s status=%d body=%s", tc.name, w.Code, w.Body.String())
		}
		id := w.Header().Get("X-Graph-Request-Id")
		if !strings.HasPrefix(id, "ctx_") {
			t.Fatalf("%s id=%q", tc.name, id)
		}
		if tc.status == 200 {
			var top map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &top); err != nil || top["request_id"] != id {
				t.Fatalf("%s request_id missing: %v %v", tc.name, top, err)
			}
		} else if strings.Contains(w.Body.String(), "request_id") {
			t.Fatalf("%s error body changed: %q", tc.name, w.Body.String())
		}
	}
	recorder.Close(5 * time.Second)
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM graph.context_traces WHERE build_rev='rev-test'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != len(cases) {
		t.Fatalf("trace rows=%d want=%d", count, len(cases))
	}
}
