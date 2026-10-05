package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/agent-mem/agent-mem/internal/graph/handlers"
	"github.com/agent-mem/agent-mem/internal/graph/scoring"
)

func TestSearch_GraphArmWeight(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	cleanup := func() {
		exec(`DELETE FROM public.settings WHERE key = 'graph.rrf.weight.graph'`)
		exec(`DELETE FROM public.settings WHERE key = ANY($1)`, scoring.BoostAlphaKeys[:])
	}
	cleanup()
	t.Cleanup(cleanup)
	for _, key := range scoring.BoostAlphaKeys {
		exec(`INSERT INTO public.settings(key,value) VALUES($1,'0')`, key)
	}
	const a = "gh_pr:weight/a"
	const b = "gh_pr:weight/b"
	const seed = "gh_pr:weight/semantic-02"
	for rank := 1; rank <= 25; rank++ {
		id := fmt.Sprintf("gh_pr:weight/semantic-%02d", rank)
		if rank == 1 {
			id = b
		}
		if rank == 20 {
			id = a
		}
		spNode(t, pool, id, "gh_pr", "ordinary artifact", "ordinary body", "public", "", "{}", 0, false)
		// Unit vectors have strictly decreasing cosines .99 through .75, all
		// above the .65 floor. A is outside the top ten expansion seeds.
		cosine := .99 - float64(rank-1)*.01
		vector := make([]float32, handlers.GraphEmbeddingDims)
		vector[0], vector[1] = float32(cosine), float32(math.Sqrt(1-cosine*cosine))
		spEmbed(t, pool, id, vector)
	}
	// Insert A's lowest-confidence edge first to reject insertion-order ranking.
	exec(`INSERT INTO graph.edges(from_node_id,to_node_id,kind,metadata,machine_id) VALUES($1,$2,'SAME_TOPIC','{"confidence":0.1}','test')`, seed, a)
	for i := 1; i <= 19; i++ {
		id := fmt.Sprintf("gh_pr:weight/neighbor-%02d", i)
		spNode(t, pool, id, "gh_pr", "ordinary neighbor", "ordinary body", "public", "", "{}", 0, false)
		exec(`INSERT INTO graph.edges(from_node_id,to_node_id,kind,metadata,machine_id) VALUES($1,$2,'SAME_TOPIC',jsonb_build_object('confidence',$3::float8),'test')`, seed, id, .2+float64(i)*.03)
	}
	h, err := handlers.NewSearchWithEmbedder(pool, fixedSearchEmbedder{vector: spUnitVec()})
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		ID        string  `json:"node_id"`
		Score     float64 `json:"score"`
		Breakdown struct {
			Ranks map[string]int `json:"ranks"`
		} `json:"score_breakdown"`
	}
	type response struct {
		Results []result          `json:"results"`
		Errors  map[string]string `json:"arm_errors"`
	}
	search := func(weight string) response {
		t.Helper()
		exec(`INSERT INTO public.settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, scoring.GraphArmWeightKey, weight)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/graph/search?q=unmatchablequasar&limit=30", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
		var resp response
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		for _, arm := range []string{"semantic", "graph", "keyword"} {
			if err := resp.Errors[arm]; err != "" {
				t.Fatalf("%s arm error: %s", arm, err)
			}
		}
		return resp
	}
	full, quarter := search("1"), search("0.25")
	find := func(resp response, id string) (int, result) {
		for i, row := range resp.Results {
			if row.ID == id {
				return i, row
			}
		}
		t.Fatalf("%s missing from response: %+v", id, resp.Results)
		return -1, result{}
	}
	// Presence, ordering, provenance, then raw scores: ordering must be the
	// first assertion that fails if production fusion ignores the setting.
	fullA, fa := find(full, a)
	fullB, fb := find(full, b)
	quarterA, qa := find(quarter, a)
	quarterB, qb := find(quarter, b)
	if fullA >= fullB {
		t.Fatalf("weight 1 ordering: A rank %d must precede B rank %d", fullA+1, fullB+1)
	}
	if quarterB >= quarterA {
		t.Fatalf("weight 0.25 ordering: B rank %d must precede A rank %d", quarterB+1, quarterA+1)
	}
	for _, row := range []result{fa, qa} {
		if !reflect.DeepEqual(row.Breakdown.Ranks, map[string]int{"semantic": 20, "graph": 20}) {
			t.Fatalf("A ranks: %v", row.Breakdown.Ranks)
		}
	}
	for _, row := range []result{fb, qb} {
		if !reflect.DeepEqual(row.Breakdown.Ranks, map[string]int{"semantic": 1}) {
			t.Fatalf("B ranks: %v", row.Breakdown.Ranks)
		}
	}
	for _, tc := range []struct {
		row  result
		want float64
	}{
		{fa, 2.0 / 80}, {fb, 1.0 / 61}, {qa, 1.25 / 80}, {qb, 1.0 / 61},
	} {
		if math.Abs(tc.row.Score-tc.want) > 1e-9 {
			t.Fatalf("%s score %.12g, want %.12g", tc.row.ID, tc.row.Score, tc.want)
		}
	}
	t.Logf("weight 1: A rank=%d score=%.12g B rank=%d score=%.12g; weight .25: A rank=%d score=%.12g B rank=%d score=%.12g", fullA+1, fa.Score, fullB+1, fb.Score, quarterA+1, qa.Score, quarterB+1, qb.Score)
}
