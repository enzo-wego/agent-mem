package handlers_test

import (
	"math"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/handlers"
)

// testDB validates the scratch target before connecting or truncating any tables.
func semanticFloorDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return testDB(t)
}

func semanticFloorBelowVec() []float32 {
	c := handlers.SemanticMinCosine - 0.02
	v := spUnitVec()
	v[0], v[1] = float32(c), float32(math.Sqrt(1-c*c))
	return v
}

func semanticFloorSearch(t *testing.T, h *handlers.Search, mode string) (map[string]map[string]any, map[string]any) {
	t.Helper()
	query := "q=floorprobe&limit=50"
	if mode == "hybrid" {
		query += "&match=hybrid"
	}
	code, top := shRaw(t, h, query, "")
	if code != http.StatusOK {
		t.Fatalf("%s search status = %d, response = %v", mode, code, top)
	}
	if errs, ok := top["arm_errors"].(map[string]any); ok {
		for arm, reason := range errs {
			if mode == "default" && arm == "temporal" && reason == "no time window in query or since/until" {
				continue
			}
			t.Fatalf("%s search arm %s failed: %v", mode, arm, reason)
		}
	}
	results := make(map[string]map[string]any)
	for _, row := range shResults(top) {
		id := row["node_id"].(string)
		if _, exists := results[id]; exists {
			t.Fatalf("duplicate search result %s", id)
		}
		results[id] = row
	}
	return results, top
}

// Case a: semantic-only matches need to clear the floor in both modes.
func TestSearch_SemanticFloorFiltersBothModes(t *testing.T) {
	for _, mode := range []string{"default", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			pool := semanticFloorDB(t)
			const above, below = "jira:FLOOR-ABOVE", "jira:FLOOR-BELOW"
			spNode(t, pool, above, "jira", "Relevant payment", "payment detail", "", "", "{}", 0, false)
			spNode(t, pool, below, "jira", "Unrelated ledger", "ledger detail", "", "", "{}", 0, false)
			spEmbed(t, pool, above, spUnitVec())
			spEmbed(t, pool, below, semanticFloorBelowVec())
			h, err := handlers.NewSearchWithEmbedder(pool, fixedSearchEmbedder{vector: spUnitVec()})
			if err != nil {
				t.Fatal(err)
			}
			results, _ := semanticFloorSearch(t, h, mode)
			if len(results) != 1 || results[above] == nil {
				t.Fatalf("results = %v, want only cosine-1 node %s", results, above)
			}
			row := results[above]
			if shRank(row, "semantic") != 1 {
				t.Fatalf("cosine-1 node semantic rank = %d, want 1", shRank(row, "semantic"))
			}
			if sem := row["score_breakdown"].(map[string]any)["sem"].(float64); math.Abs(sem-1) > 1e-6 {
				t.Fatalf("cosine-1 node sem = %v, want 1", sem)
			}
		})
	}
}

// Case b: a keyword match survives without borrowing semantic contribution
// from either the below-floor root or its below-floor reply.
func TestSearch_SemanticFloorKeywordSurvivesBothModes(t *testing.T) {
	for _, mode := range []string{"default", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			pool := semanticFloorDB(t)
			const root = "slack:CFLKB:100.000001"
			spNode(t, pool, root, "slack", "floorprobe incident", "unrelated root", "slack:CFLKB", "",
				`{"ts":"100.000001","thread_ts":"100.000001"}`, 0, false)
			reply := spMsg(t, pool, "CFLKB", "101.000001", "100.000001", "unrelated reply", 0, "", false)
			spEmbed(t, pool, root, semanticFloorBelowVec())
			spEmbed(t, pool, reply, semanticFloorBelowVec())
			h, err := handlers.NewSearchWithEmbedder(pool, fixedSearchEmbedder{vector: spUnitVec()})
			if err != nil {
				t.Fatal(err)
			}
			results, _ := semanticFloorSearch(t, h, mode)
			if len(results) != 1 || results[root] == nil {
				t.Fatalf("results = %v, want only keyword root %s", results, root)
			}
			row := results[root]
			bd := row["score_breakdown"].(map[string]any)
			ranks := bd["ranks"].(map[string]any)
			if _, ok := ranks["keyword"]; !ok || shRank(row, "keyword") < 1 {
				t.Fatalf("keyword rank missing: %v", ranks)
			}
			if _, ok := ranks["semantic"]; ok {
				t.Fatalf("below-floor keyword root has semantic rank: %v", ranks)
			}
			if sem := bd["sem"].(float64); sem != 0 {
				t.Fatalf("below-floor keyword root sem = %v, want 0", sem)
			}
		})
	}
}

// Case c: only qualifying semantic hits seed default-mode graph expansion.
// Hybrid must exclude the graph arm even when the seed qualifies.
func TestSearch_SemanticFloorGraphSeeds(t *testing.T) {
	for _, qualifies := range []bool{false, true} {
		name := "below_floor"
		if qualifies {
			name = "cosine_one_control"
		}
		t.Run(name, func(t *testing.T) {
			pool := semanticFloorDB(t)
			const seed, neighbor = "jira:FLOOR-SEED", "jira:FLOOR-NEIGHBOR"
			spNode(t, pool, seed, "jira", "Seed", "unrelated seed", "", "", "{}", 0, false)
			spNode(t, pool, neighbor, "jira", "Neighbor", "unrelated neighbor", "", "", "{}", 0, false)
			vec := semanticFloorBelowVec()
			if qualifies {
				vec = spUnitVec()
			}
			spEmbed(t, pool, seed, vec)
			seedEdge(t, pool, seed, neighbor, "REFERENCES")
			h, err := handlers.NewSearchWithEmbedder(pool, fixedSearchEmbedder{vector: spUnitVec()})
			if err != nil {
				t.Fatal(err)
			}
			results, _ := semanticFloorSearch(t, h, "default")
			if qualifies {
				if len(results) != 2 || results[seed] == nil || results[neighbor] == nil {
					t.Fatalf("control results = %v, want seed and graph neighbor", results)
				}
				if shRank(results[seed], "semantic") != 1 || shRank(results[neighbor], "graph") < 1 {
					t.Fatalf("control lacks semantic seed or graph neighbor rank: %v", results)
				}
			} else if len(results) != 0 {
				t.Fatalf("below-floor seed expanded: %v, want no results", results)
			}
			hybrid, top := semanticFloorSearch(t, h, "hybrid")
			for _, arm := range top["arms"].([]any) {
				if arm == "graph" {
					t.Fatalf("hybrid includes graph arm: %v", top["arms"])
				}
			}
			if hybrid[neighbor] != nil {
				t.Fatalf("hybrid returned graph-only neighbor: %v", hybrid)
			}
			if qualifies && (len(hybrid) != 1 || hybrid[seed] == nil) {
				t.Fatalf("hybrid control results = %v, want semantic seed only", hybrid)
			}
		})
	}
}

// Case d: filtering replies happens before hybrid thread folding.
func TestSearch_SemanticFloorFoldedReplies(t *testing.T) {
	pool := semanticFloorDB(t)
	belowRoot := spMsg(t, pool, "CFLFD", "200.000001", "200.000001", "unrelated first root", 0, "", false)
	belowReply := spMsg(t, pool, "CFLFD", "201.000001", "200.000001", "unrelated first reply", 0, "", false)
	aboveRoot := spMsg(t, pool, "CFLFD", "300.000001", "300.000001", "unrelated second root", 0, "", false)
	aboveReply := spMsg(t, pool, "CFLFD", "301.000001", "300.000001", "unrelated second reply", 0, "", false)
	spEmbed(t, pool, belowReply, semanticFloorBelowVec())
	spEmbed(t, pool, aboveReply, spUnitVec())
	h, err := handlers.NewSearchWithEmbedder(pool, fixedSearchEmbedder{vector: spUnitVec()})
	if err != nil {
		t.Fatal(err)
	}
	results, _ := semanticFloorSearch(t, h, "hybrid")
	if results[belowRoot] != nil || results[belowReply] != nil {
		t.Fatalf("below-floor reply admitted its thread: %v", results)
	}
	if len(results) != 1 || results[aboveRoot] == nil {
		t.Fatalf("results = %v, want above-floor reply folded to root %s", results, aboveRoot)
	}
	if shRank(results[aboveRoot], "semantic") != 1 {
		t.Fatalf("folded root lacks semantic rank: %v", results[aboveRoot])
	}
}
