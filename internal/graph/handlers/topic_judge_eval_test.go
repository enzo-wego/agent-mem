package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/config"
	"github.com/agent-mem/agent-mem/internal/database"
	"github.com/agent-mem/agent-mem/internal/gemini"
)

// goldenPair is one hand-labelled artifact pair: what the rules judge SHOULD
// answer for it.
type goldenPair struct {
	A    string `json:"a"`
	B    string `json:"b"`
	Want string `json:"want"` // "same" | "different"
	Note string `json:"note"`
}

// TestTopicJudgeGolden measures the rules judge against hand-labelled pairs
// from real data — the topic-link twin of TestRetrievalRecall. It is a
// measurement tool, not a CI gate: skipped unless AGENT_MEM_EVAL=1.
//
// Read-only by construction: it loads summaries and activity windows and calls
// confirmTopicLink directly, so it never writes graph.topic_link_judgments and
// never touches graph.edges — safe against the live DB (unlike the handler
// integration tests, which truncate).
//
//	AGENT_MEM_EVAL=1 DATABASE_URL=… go test ./internal/graph/handlers -run TopicJudgeGolden -v
//
// Curate pairs in testdata/topic_link_golden.json. Each rules-version bump
// should be validated here before a corpus-wide re-judge.
func TestTopicJudgeGolden(t *testing.T) {
	if os.Getenv("AGENT_MEM_EVAL") != "1" {
		t.Skip("set AGENT_MEM_EVAL=1 to run the topic-judge eval (needs real DB + API key)")
	}

	raw, err := os.ReadFile("testdata/topic_link_golden.json")
	if err != nil {
		t.Fatalf("read golden set: %v", err)
	}
	var pairs []goldenPair
	if err := json.Unmarshal(raw, &pairs); err != nil {
		t.Fatalf("parse golden set: %v", err)
	}
	if len(pairs) == 0 {
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

	deps := Deps{
		DB:     pool,
		Logger: zerolog.Nop(),
		Gemini: NewGeminiAdapter(gemini.NewClient(cfg.LLMProviderOrDefault(), cfg.ActiveLLMKey(),
			cfg.GeminiModel, cfg.GeminiEmbeddingModel, cfg.GeminiEmbeddingDims), nil),
	}

	// The judge is a sampled LLM: a single run mistakes sampling noise for a
	// rules improvement (verified — one v4 run scored 7/9, two more scored 6/9).
	// Vote each pair runs times and take the majority.
	runs := 3
	if v := os.Getenv("AGENT_MEM_EVAL_RUNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			runs = n
		}
	}

	t.Logf("rules version %d, %d vote(s) per pair", loadedTopicRules.Version, runs)
	var correct, falseNeg, falsePos int
	for _, p := range pairs {
		src, err := loadTopicLinkSource(ctx, deps, p.A)
		if err != nil {
			t.Errorf("load %s: %v", p.A, err)
			continue
		}
		cnd, err := loadTopicLinkSource(ctx, deps, p.B)
		if err != nil {
			t.Errorf("load %s: %v", p.B, err)
			continue
		}
		aStart, aEnd, aOK := nodeActivityWindow(ctx, deps, p.A)
		bStart, bEnd, bOK := nodeActivityWindow(ctx, deps, p.B)
		timeDesc, _ := timeRelation(aStart, aEnd, aOK, bStart, bEnd, bOK)

		sameVotes, failed := 0, false
		var last topicLinkJudgment
		for i := 0; i < runs; i++ {
			j, err := confirmTopicLink(ctx, deps, src, topicLinkCandidate{
				topicLinkNode: cnd,
				Cosine:        pairCosine(ctx, deps, p.A, p.B),
			}, topicLinkContext{
				SourceWindow: formatWindow(aStart, aEnd, aOK),
				CandWindow:   formatWindow(bStart, bEnd, bOK),
				TimeDesc:     timeDesc,
			})
			if err != nil {
				t.Errorf("judge %s ↔ %s: %v", p.A, p.B, err)
				failed = true
				break
			}
			if j.SameTopic {
				sameVotes++
			}
			last = j
		}
		if failed {
			continue
		}
		got := "different"
		if sameVotes*2 > runs {
			got = "same"
		}
		vote := fmt.Sprintf("%d/%d same", sameVotes, runs)
		switch {
		case got == p.Want:
			correct++
			t.Logf("ok    %-9s (%s) %s ↔ %s  [%s] %s", got, vote, p.A, p.B, last.Tag, last.Why)
		case p.Want == "same":
			falseNeg++
			t.Logf("MISS  want same, got different (%s)  %s ↔ %s  [%s] %s", vote, p.A, p.B, last.Tag, last.Why)
		default:
			falsePos++
			t.Logf("WRONG want different, got same (%s)  %s ↔ %s  [%s] %s", vote, p.A, p.B, last.Tag, last.Why)
		}
	}
	t.Logf("rules v%d (%d votes/pair): %d/%d correct — %d false negatives, %d false positives",
		loadedTopicRules.Version, runs, correct, len(pairs), falseNeg, falsePos)
}

// pairCosine returns the summary-embedding cosine between two indexed nodes
// (0 when either lacks an embedding) — the same number the judge sees in
// production.
func pairCosine(ctx context.Context, deps Deps, a, b string) float64 {
	var cos float64
	_ = deps.DB.QueryRow(ctx, `
SELECT 1.0 - (x.embedding <=> y.embedding)
FROM graph.artifact_index x, graph.artifact_index y
WHERE x.node_id = $1 AND y.node_id = $2
  AND x.embedding IS NOT NULL AND y.embedding IS NOT NULL`, a, b).Scan(&cos)
	return cos
}
