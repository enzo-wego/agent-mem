package search_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/agent-mem/agent-mem/internal/config"
	"github.com/agent-mem/agent-mem/internal/database"
	"github.com/agent-mem/agent-mem/internal/gemini"
	"github.com/agent-mem/agent-mem/internal/search"
)

// evalCase is one golden query and a substring expected in a top-k result.
type evalCase struct {
	Query   string `json:"query"`
	Project string `json:"project"`
	Want    string `json:"want"`
}

// TestRetrievalRecall is a read-only recall@k / MRR harness over real data. It
// is a measurement tool, not a CI gate: skipped unless AGENT_MEM_EVAL=1, and it
// only runs SELECTs so it is safe against the live/dev DB. Curate the golden set
// in testdata/eval_golden.json (query -> want substring) and run:
//
//	AGENT_MEM_EVAL=1 go test ./internal/search -run Recall -v
//
// A query is a hit if `want` appears (case-insensitive) in any of the top-k
// results' title/subtitle/narrative. Set AGENT_MEM_EVAL_PROJECT to scope, or per
// case via "project". ponytail: prints numbers, does not fail on low recall.
func TestRetrievalRecall(t *testing.T) {
	if os.Getenv("AGENT_MEM_EVAL") != "1" {
		t.Skip("set AGENT_MEM_EVAL=1 to run the retrieval recall eval (needs real DB + API key)")
	}

	const k = 10

	raw, err := os.ReadFile("testdata/eval_golden.json")
	if err != nil {
		t.Fatalf("read golden set: %v", err)
	}
	var cases []evalCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("parse golden set: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("golden set is empty")
	}

	cfg := config.Load()
	config.ApplyEnv(cfg)
	if cfg.DatabaseURL == "" {
		t.Fatal("DATABASE_URL not set")
	}
	if cfg.ActiveLLMKey() == "" {
		t.Fatal("no LLM API key configured")
	}

	ctx := context.Background()
	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("connect db: %v", err)
	}
	defer pool.Close()

	gc := gemini.NewClient(cfg.LLMProviderOrDefault(), cfg.ActiveLLMKey(),
		cfg.GeminiModel, cfg.GeminiEmbeddingModel, cfg.GeminiEmbeddingDims)
	searcher := search.NewSearcher(database.NewDB(pool), gc)

	defaultProject := os.Getenv("AGENT_MEM_EVAL_PROJECT")

	hits, mrrSum := 0, 0.0
	for _, tc := range cases {
		project := tc.Project
		if project == "" {
			project = defaultProject
		}
		results, err := searcher.Search(ctx, tc.Query, project, k)
		if err != nil {
			t.Errorf("query %q: search failed: %v", tc.Query, err)
			continue
		}
		rank := firstHitRank(results, tc.Want)
		if rank > 0 {
			hits++
			mrrSum += 1.0 / float64(rank)
			t.Logf("hit   rank=%-2d  %q", rank, tc.Query)
		} else {
			t.Logf("MISS          %q (want %q)", tc.Query, tc.Want)
		}
	}

	n := len(cases)
	t.Logf("recall@%d: %.2f (%d/%d)   MRR: %.2f", k, float64(hits)/float64(n), hits, n, mrrSum/float64(n))
}

// firstHitRank returns the 1-based rank of the first result whose
// title/subtitle/narrative contains want (case-insensitive), or 0 if none.
func firstHitRank(results []database.SearchResult, want string) int {
	want = strings.ToLower(want)
	for i, r := range results {
		hay := strings.ToLower(r.Title + " " + r.Subtitle + " " + r.Narrative)
		if strings.Contains(hay, want) {
			return i + 1
		}
	}
	return 0
}
