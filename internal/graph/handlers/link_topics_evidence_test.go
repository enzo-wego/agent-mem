package handlers

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

func TestEvidenceSupported(t *testing.T) {
	text := "Refund webhook retries duplicated the payout record\nfor partner Acme."
	cases := []struct {
		name        string
		quote, text string
		want        bool
	}{
		{"exact substring", "duplicated the payout record", text, true},
		{"case differs", "DUPLICATED THE PAYOUT RECORD", text, true},
		{"whitespace differs", "duplicated   the\tpayout  record", text, true},
		{"multi-line quote on single-line text", "payout record\nfor partner", "the payout record for partner Acme", true},
		{"quote chars stripped", "“duplicated” the `payout` record", text, true},
		{"markup stripped from text", "payout record for partner", "payout *record* for _partner_ Acme", true},
		{"12 runes ok", "abcdefghijkl", "xx abcdefghijkl xx", true},
		{"11 runes refused", "abcdefghijk", "xx abcdefghijk xx", false},
		{"11 runes with multibyte refused", "abcdefghijé", "abcdefghijé", false},
		{"12 runes with multibyte exact", "abcdefghijké", "abcdefghijké", true},
		{"300 runes ok", strings.Repeat("a", 300), strings.Repeat("a", 400), true},
		{"301 runes refused", strings.Repeat("a", 301), strings.Repeat("a", 400), false},
		{"empty refused", "", text, false},
		{"paraphrase refused", "retries created a second payout entry", text, false},
		{"quote only in other side", "deduplicates payout records", text, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := evidenceSupported(c.quote, c.text); got != c.want {
				t.Fatalf("evidenceSupported(%q) = %v, want %v", c.quote, got, c.want)
			}
		})
	}
}

// evidenceFake scripts GenerateCheap replies and records calls.
type evidenceFake struct {
	GeminiClient
	mu      sync.Mutex
	replies []string
	calls   int
	system  string
	user    string
	cheap   int
	main    int
}

func (f *evidenceFake) GenerateCheap(_ context.Context, sys, user string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cheap++
	f.system, f.user = sys, user
	r := f.replies[f.calls%len(f.replies)]
	f.calls++
	return r, nil
}

func (f *evidenceFake) Generate(_ context.Context, _, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.main++
	return "", nil
}

func (f *evidenceFake) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func judgeReply(t *testing.T, same bool, evA, evB string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"tag": "bug_incident", "same_topic": same, "confidence": 0.9,
		"topic": "payout dup", "why": "same defect", "evidence_a": evA, "evidence_b": evB,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const (
	evSumA = "Refund webhook retries duplicated the payout record for partner Acme in the settlement job."
	evSumB = "PR 123 deduplicates payout records written by the refund webhook retry path."
	evQA   = "duplicated the payout record"
	evQB   = "deduplicates payout records written"
)

func TestConfirmTopicLinkEvidence(t *testing.T) {
	a := topicLinkNode{NodeID: "jira:A", Type: "jira", Summary: evSumA}
	b := topicLinkCandidate{topicLinkNode: topicLinkNode{NodeID: "jira:B", Type: "jira", Summary: evSumB}, SharedIDs: []string{"pxx6xgkdtl123"}}
	tc := topicLinkContext{TimeDesc: "overlap"}

	run := func(reply string, cand topicLinkCandidate) (topicLinkJudgment, *evidenceFake) {
		t.Helper()
		f := &evidenceFake{replies: []string{reply}}
		j, err := confirmTopicLink(context.Background(), Deps{Gemini: f, Logger: zerolog.Nop()}, a, cand, tc)
		if err != nil {
			t.Fatal(err)
		}
		return j, f
	}

	t.Run("both present", func(t *testing.T) {
		j, f := run(judgeReply(t, true, "  Duplicated the\npayout record ", evQB), b)
		if !j.SameTopic || j.EvidenceA != "Duplicated the payout record" || j.EvidenceB != evQB {
			t.Fatalf("judgment = %+v", j)
		}
		if f.cheap != 1 || f.main != 0 {
			t.Fatalf("cheap=%d main=%d, want 1/0", f.cheap, f.main)
		}
		if !strings.Contains(f.system, "evidence_a") || !strings.Contains(f.system, "SAME AREA IS NOT SAME TOPIC") {
			t.Fatalf("system prompt missing evidence/tie-breaker text")
		}
	})
	t.Run("quote B missing", func(t *testing.T) {
		j, _ := run(judgeReply(t, true, evQA, "this passage is not in artifact b"), b)
		if j.SameTopic || !strings.HasPrefix(j.Why, "evidence not found: ") || j.EvidenceA != "" || j.EvidenceB != "" {
			t.Fatalf("judgment = %+v", j)
		}
	})
	t.Run("quote from shared identifiers line only", func(t *testing.T) {
		j, _ := run(judgeReply(t, true, evQA, "pxx6xgkdtl123"), b)
		if j.SameTopic || !strings.HasPrefix(j.Why, "evidence not found: ") {
			t.Fatalf("judgment = %+v", j)
		}
	})
	t.Run("evidence absent", func(t *testing.T) {
		j, _ := run(`{"same_topic":true,"confidence":0.9,"topic":"x","why":"y"}`, b)
		if j.SameTopic {
			t.Fatalf("judgment = %+v", j)
		}
	})
	t.Run("different without evidence unchanged", func(t *testing.T) {
		j, _ := run(`{"same_topic":false,"confidence":0.7,"topic":"x","why":"unrelated"}`, b)
		if j.SameTopic || j.Why != "unrelated" {
			t.Fatalf("judgment = %+v", j)
		}
	})
}

// --- DB-backed: evidence persists against the stored endpoints ---

func TestLinkTopicsEvidencePersistence(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var dbName string
	if err := pool.QueryRow(ctx, `SELECT current_database()`).Scan(&dbName); err != nil || dbName != "agentmem_test" {
		t.Fatalf("refusing to run: database = %q (err %v), want agentmem_test", dbName, err)
	}

	const from, to = "jira:EVID-1", "jira:EVID-2" // canonical order: from < to
	ids := []string{from, to}
	cleanup := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM graph.edges WHERE from_node_id=ANY($1) OR to_node_id=ANY($1)`, ids)
		_, _ = pool.Exec(ctx, `DELETE FROM graph.topic_link_judgments WHERE source_node_id=ANY($1) OR target_node_id=ANY($1)`, ids)
		_, _ = pool.Exec(ctx, `DELETE FROM graph.artifact_index WHERE node_id=ANY($1)`, ids)
		_, _ = pool.Exec(ctx, `DELETE FROM graph.nodes WHERE id=ANY($1)`, ids)
	}
	cleanup()
	t.Cleanup(cleanup)

	for id, sum := range map[string]string{from: evSumA, to: evSumB} {
		if _, err := pool.Exec(ctx, `INSERT INTO graph.nodes (id,type,natural_key,title,metadata,machine_id,created_at,first_seen_at)
 VALUES ($1,'jira',$1,'t','{}','test','2026-01-01','2026-01-01')`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO graph.artifact_index (node_id,summary,summary_kind,embedding,machine_id)
 VALUES ($1,$2,'heuristic',array_fill(0.1::real, ARRAY[$3::int])::halfvec,'test')`, id, sum, GraphEmbeddingDims); err != nil {
			t.Fatal(err)
		}
	}

	fake := &evidenceFake{}
	deps := Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "test", Gemini: fake}
	handler := linkTopicsHandler(deps)
	runOne := func(node, other string, force bool, reply string) {
		t.Helper()
		if reply != "" {
			fake.mu.Lock()
			fake.replies = []string{reply}
			fake.mu.Unlock()
		}
		payload, _ := json.Marshal(linkTopicsPayload{NodeID: node, Force: force, ExtraCandidates: []string{other}})
		if err := handler(ctx, payload); err != nil {
			t.Fatal(err)
		}
	}
	checkEdge := func(label string) {
		t.Helper()
		var ef, et string
		if err := pool.QueryRow(ctx, `SELECT COALESCE(metadata->>'evidence_from',''), COALESCE(metadata->>'evidence_to','')
 FROM graph.edges WHERE from_node_id=$1 AND to_node_id=$2 AND kind='SAME_TOPIC'`, from, to).Scan(&ef, &et); err != nil {
			t.Fatalf("%s: edge: %v", label, err)
		}
		if ef != evQA || et != evQB {
			t.Fatalf("%s: evidence_from=%q evidence_to=%q, want %q / %q", label, ef, et, evQA, evQB)
		}
	}

	// Run 1: source is canonical from; A = from-artifact.
	runOne(from, to, false, judgeReply(t, true, evQA, evQB))
	if n := fake.callCount(); n != 1 {
		t.Fatalf("run 1 model calls = %d, want 1", n)
	}
	checkEdge("run 1")

	// Run 2: source is canonical to, forced; A = to-artifact so quotes are reversed.
	runOne(to, from, true, judgeReply(t, true, evQB, evQA))
	if n := fake.callCount(); n != 2 {
		t.Fatalf("run 2 total model calls = %d, want 2", n)
	}
	checkEdge("run 2")

	// Run 3: cache hit from either node; no model call, evidence preserved.
	runOne(from, to, false, "")
	runOne(to, from, false, "")
	if n := fake.callCount(); n != 2 {
		t.Fatalf("run 3 total model calls = %d, want 2 (cache hit)", n)
	}
	checkEdge("run 3")
}
