package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/gemini"
	graphtrace "github.com/agent-mem/agent-mem/internal/graph/trace"
	"github.com/jackc/pgx/v5/pgxpool"
)

const compatibilityFixture = "../../../testdata/eval/graph_context/fixture.sql"

type compatibilityEmbedder struct{}

func (compatibilityEmbedder) Embed(context.Context, string) ([]float32, error) {
	return make([]float32, GraphEmbeddingDims), nil
}
func (compatibilityEmbedder) EmbedWithOptions(context.Context, string, gemini.EmbedOptions) ([]float32, error) {
	return make([]float32, GraphEmbeddingDims), nil
}

type compatibilityCase struct {
	name, method, target, body, asker string
	handler                           http.Handler
}

func TestResolveSearchCompatibility(t *testing.T) {
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
	fixture, err := os.ReadFile(compatibilityFixture)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), string(fixture)); err != nil {
		t.Fatalf("load compatibility fixture: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM public.settings WHERE key LIKE 'graph.weights.%' OR key LIKE 'graph.boost.alpha.%' OR key = 'graph.search.graph_arm_weight'`); err != nil {
		t.Fatal(err)
	}

	oldLocal := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = oldLocal })
	fixed := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	oldNow := nowFunc
	nowFunc = func() time.Time { return fixed }
	t.Cleanup(func() { nowFunc = oldNow })
	resolve, err := NewResolve(pool)
	if err != nil {
		t.Fatal(err)
	}
	search, err := NewSearchWithEmbedder(pool, compatibilityEmbedder{})
	if err != nil {
		t.Fatal(err)
	}
	recorder := graphtrace.NewRecorder(discardTraceStore{}, graphtrace.Options{})
	t.Cleanup(func() { recorder.Close(time.Second) })
	cases := []compatibilityCase{
		{"resolve-hit", http.MethodPost, "/api/graph/resolve", `{"seeds":["slack:CGC0:100.000001"],"query":"phase zero","depth":2,"include_bodies":true}`, "", resolve},
		{"resolve-reply-seed", http.MethodPost, "/api/graph/resolve", `{"seeds":["slack:CGC0:101.000001"],"depth":2}`, "", resolve},
		{"resolve-empty", http.MethodPost, "/api/graph/resolve", `{"seeds":["jira:UNKNOWN"],"depth":2}`, "", resolve},
		{"resolve-cache-miss", http.MethodPost, "/api/graph/resolve", `{"seeds":["jira:GC0-MISS"],"depth":2}`, "", resolve},
		{"resolve-invalid", http.MethodPost, "/api/graph/resolve", `{}`, "", resolve},
		{"search-ordinary-hit", http.MethodGet, "/api/graph/search?q=phase+zero&limit=10", "", "", search},
		{"search-hybrid-hit", http.MethodGet, "/api/graph/search?q=phase+zero&match=hybrid&limit=10", "", "", search},
		{"search-empty", http.MethodGet, "/api/graph/search?q=unfindabletoken&limit=10", "", "", search},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := adminRequest(tc.method, tc.target, bytes.NewBufferString(tc.body))
			if tc.asker != "" {
				setAsker(req, tc.asker)
			}
			rw := httptest.NewRecorder()
			route := "search"
			if tc.method == http.MethodPost {
				route = "resolve"
			}
			withContextTrace(tc.handler, route, "compatibility", recorder).ServeHTTP(rw, req)
			got := compatibilityEnvelope{Status: rw.Code, ContentType: rw.Header().Get("Content-Type"), Body: normalizeCompatibilityBody(t, rw.Body.Bytes())}
			golden := filepath.Join("testdata", "compatibility", tc.name+".golden.json")
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				data, _ := json.MarshalIndent(got, "", "  ")
				if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, append(data, '\n'), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			wantBytes, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			var want compatibilityEnvelope
			if err := json.Unmarshal(wantBytes, &want); err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("response changed\ngot:  %s\nwant: %s", compatibilityJSON(got), compatibilityJSON(want))
			}
			assertNoResolveScoreTies(t, got.Body)
		})
	}
}

type compatibilityEnvelope struct {
	Status      int    `json:"status"`
	ContentType string `json:"content_type"`
	Body        string `json:"body"`
}

var compatibilityTiming = regexp.MustCompile(`("took_ms"\s*:\s*)-?\d+`)

func normalizeCompatibilityBody(t *testing.T, body []byte) string {
	t.Helper()
	if !json.Valid(body) {
		return string(body)
	}
	open := bytes.IndexByte(body, '{')
	const prefix = `"request_id":"`
	if open < 0 || !bytes.HasPrefix(body[open+1:], []byte(prefix)) {
		t.Fatalf("successful JSON response missing leading request_id: %s", body)
	}
	end := bytes.Index(body[open+1+len(prefix):], []byte(`",`))
	if end < 0 {
		t.Fatalf("malformed request_id member: %s", body)
	}
	memberEnd := open + 1 + len(prefix) + end + 2
	normalized := make([]byte, 0, len(body))
	normalized = append(normalized, body[:open+1]...)
	normalized = append(normalized, body[memberEnd:]...)
	normalized = compatibilityTiming.ReplaceAll(normalized, []byte(`${1}0`))
	return string(normalized)
}

func assertNoResolveScoreTies(t *testing.T, body string) {
	t.Helper()
	var top map[string]any
	if json.Unmarshal([]byte(body), &top) != nil {
		return
	}
	items, ok := top["artifacts"].([]any)
	if !ok {
		return
	}
	seen := map[float64]bool{}
	for _, raw := range items {
		score, _ := raw.(map[string]any)["score"].(float64)
		if seen[score] {
			t.Fatalf("fixture has tied resolve score %v", score)
		}
		seen[score] = true
	}
}

func compatibilityJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestCompatibilityFixtureHashIsStable(t *testing.T) {
	b, err := os.ReadFile(compatibilityFixture)
	if err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprintf("%x", sha256.Sum256(b))
	const want = "69fcc93fb2829413325eb397f745eeadc310c882be61f77990475f565161cc1f"
	if got != want {
		t.Fatalf("fixture sha256=%s want=%s", got, want)
	}
}
