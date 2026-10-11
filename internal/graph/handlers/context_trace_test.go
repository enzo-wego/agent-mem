package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/gemini"
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
	}{{"resolve-ok", "resolve", "POST", "/api/graph/resolve", `{"seeds":["slack:CGC0:100.000001"]}`, resolve, 200}, {"resolve-decoded-asker", "resolve", "POST", "/api/graph/resolve", `{"seeds":["slack:CGC0:100.000001"],"Asker_EEID":5}`, resolve, 200}, {"resolve-error", "resolve", "POST", "/api/graph/resolve", `{}`, resolve, 400}, {"search-ordinary", "search", "GET", "/api/graph/search?q=phase+zero", "", search, 200}, {"search-hybrid", "search", "GET", "/api/graph/search?q=phase+zero&match=hybrid", "", search, 200}, {"search-error", "search", "GET", "/api/graph/search", "", search, 400}}
	ids := map[string]string{}
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
		ids[tc.name] = id
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
	var aclMode string
	var asker, request []byte
	if err := pool.QueryRow(context.Background(), `SELECT acl_mode,asker_input,request FROM graph.context_traces WHERE id=$1`, ids["resolve-decoded-asker"]).Scan(&aclMode, &asker, &request); err != nil {
		t.Fatal(err)
	}
	if aclMode != "filtered" || !bytes.Contains(asker, []byte(`"body_asker_eeid": 5`)) {
		t.Fatalf("resolve trace acl=%q asker=%s", aclMode, asker)
	}
	if !bytes.Contains(request, []byte(`"canonical_seeds"`)) {
		t.Fatalf("resolve request missing canonical seeds: %s", request)
	}
	for name, want := range map[string]string{"resolve-ok": "unfiltered", "resolve-decoded-asker": "filtered", "resolve-error": "not-evaluated", "search-ordinary": "unfiltered", "search-hybrid": "unfiltered", "search-error": "not-evaluated"} {
		if err := pool.QueryRow(context.Background(), `SELECT acl_mode FROM graph.context_traces WHERE id=$1`, ids[name]).Scan(&aclMode); err != nil {
			t.Fatal(err)
		}
		if aclMode != want {
			t.Fatalf("%s acl_mode=%q want=%q", name, aclMode, want)
		}
	}
}

type failingEmbedder struct{}

func (failingEmbedder) Embed(context.Context, string) ([]float32, error) {
	return nil, errors.New("embedder down")
}
func (failingEmbedder) EmbedWithOptions(context.Context, string, gemini.EmbedOptions) ([]float32, error) {
	return nil, errors.New("embedder down")
}

func TestContextTraceStoresSearchArmErrors(t *testing.T) {
	pool := compatibilityPool(t)
	fixture, err := os.ReadFile(compatibilityFixture)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), string(fixture)); err != nil {
		t.Fatal(err)
	}
	search, err := NewSearchWithEmbedder(pool, failingEmbedder{})
	if err != nil {
		t.Fatal(err)
	}
	store := &captureTraceStore{}
	recorder := graphtrace.NewRecorder(store, graphtrace.Options{})
	withContextTrace(search, "search", "test", recorder).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/graph/search?q=phase+zero&match=hybrid", nil))
	recorder.Close(time.Second)
	request := store.one(t).Request.(map[string]any)
	armErrors, ok := request["arm_errors"].(map[string]string)
	if !ok || !strings.Contains(armErrors["semantic"], "embedder down") {
		t.Fatalf("stored arm_errors=%v", request["arm_errors"])
	}
}

func TestContextTraceRecordsMembershipFailureACLMode(t *testing.T) {
	pool := compatibilityPool(t)
	fixture, err := os.ReadFile(compatibilityFixture)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), string(fixture)); err != nil {
		t.Fatal(err)
	}
	aclPerson(t, pool, 503, "trace-failure@example.com")
	search, err := NewSearchWithEmbedder(pool, compatibilityEmbedder{})
	if err != nil {
		t.Fatal(err)
	}
	search.aclBld = fixedScopeBuilder{err: context.Canceled}
	store := &captureTraceStore{}
	recorder := graphtrace.NewRecorder(store, graphtrace.Options{})
	r := httptest.NewRequest(http.MethodGet, "/api/graph/search?q=phase+zero", nil)
	r.Header.Set("X-Asker-User", "trace-failure@example.com")
	withContextTrace(search, "search", "test", recorder).ServeHTTP(httptest.NewRecorder(), r)
	recorder.Close(time.Second)
	if got := store.one(t).ACLMode; got != "filtered-membership-error" {
		t.Fatalf("acl_mode=%q", got)
	}
}
