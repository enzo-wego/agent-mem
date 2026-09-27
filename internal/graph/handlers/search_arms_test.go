package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"github.com/agent-mem/agent-mem/internal/gemini"
	"github.com/agent-mem/agent-mem/internal/graph/handlers"
)

type armResp struct {
	Results []struct {
		NodeID    string `json:"node_id"`
		Score     float64
		Breakdown struct {
			RRF      float64        `json:"rrf"`
			Temporal float64        `json:"temporal"`
			Ranks    map[string]int `json:"ranks"`
		} `json:"score_breakdown"`
	} `json:"results"`
	Arms      []string          `json:"arms"`
	ArmErrors map[string]string `json:"arm_errors"`
	Window    *struct {
		Start time.Time `json:"start"`
		End   time.Time `json:"end"`
	} `json:"window"`
	Query string `json:"query"`
}

func doSearch(t *testing.T, h http.Handler, rawQuery string) (int, armResp) {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/graph/search?"+rawQuery, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var resp armResp
	if w.Code == http.StatusOK {
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return w.Code, resp
}

func seedEmbedding(t *testing.T, pool *pgxpool.Pool, id, summary string, vec []float32) {
	t.Helper()
	var v any
	if vec != nil {
		v = pgvector.NewVector(vec)
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO graph.artifact_index (node_id, summary, summary_kind, embedding, machine_id)
VALUES ($1, $2, 'heuristic', $3, 'test')`, id, summary, v); err != nil {
		t.Fatalf("seedEmbedding %s: %v", id, err)
	}
}

func setCreated(t *testing.T, pool *pgxpool.Pool, id string, at time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE graph.nodes SET created_at = $2 WHERE id = $1`, id, at); err != nil {
		t.Fatalf("setCreated %s: %v", id, err)
	}
}

// Fixture: four nodes, each reachable by a different mix of arms.
//
//	slack:C:all   semantic (cos 0.9), keyword (title+summary), temporal (Aug 20)
//	slack:C:sem   semantic only (cos 1.0, unrelated title, January)
//	jira:PAY-7    keyword only (title) + graph (REFERENCED by slack:C:all)
//	slack:C:dated temporal only (Aug 15, no embedding, no keyword)
func seedArmsFixture(t *testing.T, pool *pgxpool.Pool) (query []float32) {
	t.Helper()
	dims := handlers.GraphEmbeddingDims
	query = make([]float32, dims)
	query[0] = 1
	near := make([]float32, dims)
	near[0], near[1] = 0.9, float32(math.Sqrt(1-0.81))

	jan := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	seedNode(t, pool, "slack:C:all", "slack", "rollout plan")
	seedEmbedding(t, pool, "slack:C:all", "rollout plan for the new flow", near)
	setCreated(t, pool, "slack:C:all", time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC))

	seedNode(t, pool, "slack:C:sem", "slack", "unrelated words")
	seedEmbedding(t, pool, "slack:C:sem", "nothing in common", query)
	setCreated(t, pool, "slack:C:sem", jan)

	seedNode(t, pool, "jira:PAY-7", "jira", "PAY-7 rollout checklist")
	setCreated(t, pool, "jira:PAY-7", jan)
	seedEdge(t, pool, "slack:C:all", "jira:PAY-7", "REFERENCES")

	seedNode(t, pool, "slack:C:dated", "slack", "deploy notes")
	setCreated(t, pool, "slack:C:dated", time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC))
	return query
}

func TestSearch_FourArmsFusedOrder(t *testing.T) {
	pool := testDB(t)
	query := seedArmsFixture(t, pool)
	h, err := handlers.NewSearchWithEmbedder(pool, fixedSearchEmbedder{vector: query})
	if err != nil {
		t.Fatal(err)
	}

	code, resp := doSearch(t, h, "q=rollout+in+August+2026&limit=10")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if resp.Query != "rollout" {
		t.Errorf("query rest = %q, want rollout", resp.Query)
	}
	if resp.Window == nil || !resp.Window.Start.Equal(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)) ||
		!resp.Window.End.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("window = %+v, want August 2026", resp.Window)
	}
	if len(resp.Arms) != 4 || len(resp.ArmErrors) != 0 {
		t.Errorf("arms = %v errors = %v, want all four and none", resp.Arms, resp.ArmErrors)
	}

	got := map[string]map[string]int{}
	var order []string
	for _, r := range resp.Results {
		got[r.NodeID] = r.Breakdown.Ranks
		order = append(order, r.NodeID)
		if r.Breakdown.RRF <= 0 {
			t.Errorf("%s has no rrf score", r.NodeID)
		}
	}
	wantRanks := map[string]map[string]int{
		"slack:C:all":   {"semantic": 2, "keyword": 1, "temporal": 2},
		"slack:C:sem":   {"semantic": 1},
		"jira:PAY-7":    {"keyword": 2, "graph": 1},
		"slack:C:dated": {"temporal": 1}, // Aug 15 sits in an earlier bucket than Aug 20
	}
	for id, want := range wantRanks {
		if len(got[id]) != len(want) {
			t.Errorf("%s ranks = %v, want %v", id, got[id], want)
			continue
		}
		for arm, rank := range want {
			if got[id][arm] != rank {
				t.Errorf("%s %s rank = %d, want %d (all: %v)", id, arm, got[id][arm], rank, got[id])
			}
		}
	}
	// Fusion: three arms > two arms > one arm; between the single-arm hits
	// the temporal boost lifts the in-window thread over the January one.
	wantOrder := []string{"slack:C:all", "jira:PAY-7", "slack:C:dated", "slack:C:sem"}
	if len(order) != len(wantOrder) {
		t.Fatalf("order = %v, want %v", order, wantOrder)
	}
	for i := range wantOrder {
		if order[i] != wantOrder[i] {
			t.Errorf("order = %v, want %v", order, wantOrder)
			break
		}
	}
	for _, r := range resp.Results {
		switch r.NodeID {
		case "slack:C:dated":
			if r.Breakdown.Temporal < 0.9 {
				t.Errorf("dated temporal proximity = %v, want near 1", r.Breakdown.Temporal)
			}
		case "slack:C:sem":
			if r.Breakdown.Temporal != 0 {
				t.Errorf("January thread temporal proximity = %v, want 0", r.Breakdown.Temporal)
			}
		}
	}
}

func TestSearch_ArmSelectionAndExplicitWindow(t *testing.T) {
	pool := testDB(t)
	query := seedArmsFixture(t, pool)
	h, err := handlers.NewSearchWithEmbedder(pool, fixedSearchEmbedder{vector: query})
	if err != nil {
		t.Fatal(err)
	}

	// arms=keyword: only the keyword list is fused.
	code, resp := doSearch(t, h, "q=rollout&arms=keyword")
	if code != http.StatusOK || len(resp.Arms) != 1 || resp.Arms[0] != "keyword" {
		t.Fatalf("arms=keyword → %d %v", code, resp.Arms)
	}
	if len(resp.Results) != 2 || resp.Results[0].NodeID != "slack:C:all" || resp.Results[1].NodeID != "jira:PAY-7" {
		t.Errorf("keyword-only results = %+v", resp.Results)
	}
	if resp.ArmErrors["temporal"] == "" {
		t.Errorf("temporal must report why it did not run: %v", resp.ArmErrors)
	}

	// Explicit since/until with a plain query: temporal arm runs, rest == q.
	code, resp = doSearch(t, h, "q=rollout&since=2026-08-01&until=2026-08-31&arms=temporal")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if resp.Query != "rollout" || resp.Window == nil || !resp.Window.End.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("explicit window: query=%q window=%+v", resp.Query, resp.Window)
	}
	ids := map[string]bool{}
	for _, r := range resp.Results {
		ids[r.NodeID] = true
	}
	if !ids["slack:C:dated"] || !ids["slack:C:all"] || ids["slack:C:sem"] {
		t.Errorf("temporal-only results = %v", ids)
	}

	if code, _ := doSearch(t, h, "q=rollout&arms=bogus"); code != http.StatusBadRequest {
		t.Errorf("unknown arm status = %d, want 400", code)
	}
	if code, _ := doSearch(t, h, "q=rollout&since=notadate"); code != http.StatusBadRequest {
		t.Errorf("bad since status = %d, want 400", code)
	}
}

type failingEmbedder struct{}

func (failingEmbedder) Embed(context.Context, string) ([]float32, error) {
	return nil, errors.New("gateway down")
}

func (failingEmbedder) EmbedWithOptions(context.Context, string, gemini.EmbedOptions) ([]float32, error) {
	return nil, errors.New("gateway down")
}

// An arm that fails is dropped from fusion and reported; the request still
// succeeds with whatever the other arms found.
func TestSearch_FailedArmIsReportedNotFatal(t *testing.T) {
	pool := testDB(t)
	seedArmsFixture(t, pool)
	h, err := handlers.NewSearchWithEmbedder(pool, failingEmbedder{})
	if err != nil {
		t.Fatal(err)
	}
	code, resp := doSearch(t, h, "q=rollout")
	if code != http.StatusOK {
		t.Fatalf("status %d, want 200 despite embed failure", code)
	}
	if resp.ArmErrors["semantic"] == "" || resp.ArmErrors["graph"] == "" {
		t.Errorf("arm_errors = %v, want semantic and graph reported", resp.ArmErrors)
	}
	if len(resp.Arms) != 1 || resp.Arms[0] != "keyword" {
		t.Errorf("arms = %v, want keyword only", resp.Arms)
	}
	if len(resp.Results) != 2 {
		t.Errorf("keyword results = %+v, want the two rollout nodes", resp.Results)
	}
}
