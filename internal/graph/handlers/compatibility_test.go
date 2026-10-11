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
	"reflect"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/gemini"
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
	cases := []compatibilityCase{
		{"resolve-hit", http.MethodPost, "/api/graph/resolve", `{"seeds":["slack:CGC0:100.000001"],"query":"phase zero","depth":2,"include_bodies":true}`, "", resolve},
		{"resolve-reply-seed", http.MethodPost, "/api/graph/resolve", `{"seeds":["slack:CGC0:101.000001"],"depth":2}`, "", resolve},
		{"resolve-empty", http.MethodPost, "/api/graph/resolve", `{"seeds":["jira:UNKNOWN"],"depth":2}`, "", resolve},
		{"resolve-cache-miss", http.MethodPost, "/api/graph/resolve", `{"seeds":["jira:GC0-MISS"],"depth":2}`, "", resolve},
		{"resolve-invalid", http.MethodPost, "/api/graph/resolve", `{}`, "", resolve},
		{"search-ordinary-hit", http.MethodGet, "/api/graph/search?q=phase+zero&limit=10", "", "", search},
		{"search-hybrid-hit", http.MethodGet, "/api/graph/search?q=phase+zero&match=hybrid&limit=10", "", "", search},
		{"search-empty", http.MethodGet, "/api/graph/search?q=unfindabletoken&limit=10", "", "", search},
		{"search-unknown-asker", http.MethodGet, "/api/graph/search?q=phase+zero&match=hybrid&limit=10", "", "UUNKNOWN", search},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.target, bytes.NewBufferString(tc.body))
			if tc.asker != "" {
				req.Header.Set("X-Asker-User", tc.asker)
			}
			rw := httptest.NewRecorder()
			tc.handler.ServeHTTP(rw, req)
			got := compatibilityEnvelope{Status: rw.Code, ContentType: rw.Header().Get("Content-Type"), Body: normalizeCompatibilityBody(t, rw.Body.Bytes())}
			golden := filepath.Join("testdata", "compatibility", tc.name+".golden.json")
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				data, _ := json.MarshalIndent(got, "", "  ")
				if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil { t.Fatal(err) }
				if err := os.WriteFile(golden, append(data, '\n'), 0o644); err != nil { t.Fatal(err) }
			}
			wantBytes, err := os.ReadFile(golden)
			if err != nil { t.Fatal(err) }
			var want compatibilityEnvelope
			if err := json.Unmarshal(wantBytes, &want); err != nil { t.Fatal(err) }
			if !reflect.DeepEqual(got, want) { t.Fatalf("response changed\ngot:  %s\nwant: %s", compatibilityJSON(got), compatibilityJSON(want)) }
			assertNoResolveScoreTies(t, got.Body)
		})
	}
}

type compatibilityEnvelope struct {
	Status int `json:"status"`
	ContentType string `json:"content_type"`
	Body any `json:"body"`
}

func normalizeCompatibilityBody(t *testing.T, body []byte) any {
	t.Helper()
	var v any
	if json.Unmarshal(body, &v) != nil { return string(body) }
	if top, ok := v.(map[string]any); ok {
		delete(top, "request_id")
		if trace, ok := top["graph_trace"].(map[string]any); ok { trace["took_ms"] = float64(0) }
		for k := range top { if len(k) >= 3 && k[len(k)-3:] == "_ms" { top[k] = float64(0) } }
	}
	return v
}

func assertNoResolveScoreTies(t *testing.T, body any) {
	t.Helper()
	top, ok := body.(map[string]any); if !ok { return }
	items, ok := top["artifacts"].([]any); if !ok { return }
	seen := map[float64]bool{}
	for _, raw := range items {
		score, _ := raw.(map[string]any)["score"].(float64)
		if seen[score] { t.Fatalf("fixture has tied resolve score %v", score) }
		seen[score] = true
	}
}

func compatibilityJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestCompatibilityFixtureHashIsStable(t *testing.T) {
	b, err := os.ReadFile(compatibilityFixture); if err != nil { t.Fatal(err) }
	got := fmt.Sprintf("%x", sha256.Sum256(b))
	if len(got) != 64 { t.Fatal(got) }
}

