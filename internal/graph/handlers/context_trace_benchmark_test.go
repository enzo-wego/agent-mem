package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"testing"
	"time"

	graphtrace "github.com/agent-mem/agent-mem/internal/graph/trace"
)

type discardTraceStore struct{}

func (discardTraceStore) Write(context.Context, graphtrace.Record) error  { return nil }
func (discardTraceStore) Prune(context.Context, time.Time) (int64, error) { return 0, nil }

func TestContextTraceP95Overhead(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	pool := compatibilityPool(t)
	fixture, err := os.ReadFile(compatibilityFixture)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(context.Background(), string(fixture)); err != nil {
		t.Fatal(err)
	}
	resolve, _ := NewResolve(pool)
	search, _ := NewSearchWithEmbedder(pool, compatibilityEmbedder{})
	recorder := graphtrace.NewRecorder(discardTraceStore{}, graphtrace.Options{})
	defer recorder.Close(time.Second)
	cases := []struct {
		name, method, target, body, route string
		handler                           http.Handler
	}{{"resolve-no-query", "POST", "/api/graph/resolve", `{"seeds":["slack:CGC0:100.000001"]}`, "resolve", resolve}, {"resolve-query", "POST", "/api/graph/resolve", `{"seeds":["slack:CGC0:100.000001"],"query":"phase zero"}`, "resolve", resolve}, {"search-ordinary", "GET", "/api/graph/search?q=phase+zero", "", "search", search}, {"search-hybrid", "GET", "/api/graph/search?q=phase+zero&match=hybrid", "", "search", search}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deltas := make([]time.Duration, 0, 200)
			for range 200 {
				off := timedRequest(tc.handler, tc.method, tc.target, tc.body)
				on := timedRequest(withContextTrace(tc.handler, tc.route, "bench", recorder), tc.method, tc.target, tc.body)
				if on > off {
					deltas = append(deltas, on-off)
				} else {
					deltas = append(deltas, 0)
				}
			}
			sort.Slice(deltas, func(i, j int) bool { return deltas[i] < deltas[j] })
			p95 := deltas[189]
			t.Logf("p95 trace overhead %s", p95)
			if p95 > 5*time.Millisecond {
				t.Fatalf("p95 trace overhead=%s, limit=5ms", p95)
			}
		})
	}
}
func timedRequest(h http.Handler, method, target, body string) time.Duration {
	r := adminRequest(method, target, bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(w, r)
	return time.Since(start)
}
