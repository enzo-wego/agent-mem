package handlers

// Fixture eval for the topic judge: no database. Fully-formed judge inputs and
// human labels come from a JSON fixture; each pair is voted several times.
// This file must compile against older commits (it uses only long-standing
// identifiers) so the old judge can be measured with the identical harness.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/agent-mem/agent-mem/internal/llmgateway"
)

type judgeFixtureNode struct {
	NodeID     string   `json:"node_id"`
	Type       string   `json:"type"`
	Summary    string   `json:"summary"`
	Department string   `json:"department"`
	Cosine     float64  `json:"cosine"`
	SharedIDs  []string `json:"shared_ids"`
	CaseIDs    []string `json:"case_ids"`
}

type judgeFixtureCtx struct {
	SourceWindow string `json:"source_window"`
	CandWindow   string `json:"cand_window"`
	TimeDesc     string `json:"time_desc"`
}

type judgeFixtureEntry struct {
	EdgeID int64            `json:"edge_id"`
	Label  string           `json:"label"`
	Cause  string           `json:"cause"`
	Source judgeFixtureNode `json:"source"`
	Cand   judgeFixtureNode `json:"cand"`
	Ctx    judgeFixtureCtx  `json:"ctx"`
}

type judgeFixtureCounts struct {
	Correct, KeptCorrect int
	Wrong, RefusedWrong  int
	Unsure               int
	// RefusedByCause maps a WRONG entry's cause to {refused, total}.
	RefusedByCause map[string][2]int
	// EvidenceRejected counts pairs whose final verdict is DIFFERENT and where
	// a strict majority of all votes had a Why starting "evidence not found:".
	EvidenceRejected int
	// Log has one line per processed entry listing every vote.
	Log []string
}

// tierGemini routes the production GenerateCheap call to the main tier when
// tier == "main". Test-only: production keeps GenerateCheap.
type tierGemini struct {
	GeminiClient
	tier string
}

func (g tierGemini) GenerateCheap(ctx context.Context, sys, user string) (string, error) {
	if g.tier == "main" {
		return g.GeminiClient.Generate(ctx, sys, user)
	}
	return g.GeminiClient.GenerateCheap(ctx, sys, user)
}

func withJudgeTier(inner GeminiClient, tier string) (GeminiClient, error) {
	if tier != "cheap" && tier != "main" {
		return nil, fmt.Errorf("tier %q: want cheap or main", tier)
	}
	return tierGemini{GeminiClient: inner, tier: tier}, nil
}

func loadJudgeFixture(path string) ([]judgeFixtureEntry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []judgeFixtureEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("parse fixture: %w", err)
	}
	decided := 0
	for _, e := range entries {
		switch e.Label {
		case "CORRECT", "WRONG":
			decided++
		case "UNSURE":
		default:
			return nil, fmt.Errorf("edge %d: label %q not CORRECT|WRONG|UNSURE", e.EdgeID, e.Label)
		}
		if strings.TrimSpace(e.Source.Summary) == "" || strings.TrimSpace(e.Cand.Summary) == "" {
			return nil, fmt.Errorf("edge %d: empty summary", e.EdgeID)
		}
	}
	if decided == 0 {
		return nil, fmt.Errorf("fixture has no CORRECT or WRONG entries")
	}
	return entries, nil
}

func runJudgeFixture(ctx context.Context, deps Deps, entries []judgeFixtureEntry, runs int) (judgeFixtureCounts, error) {
	counts := judgeFixtureCounts{RefusedByCause: map[string][2]int{}}
	if runs <= 0 || runs%2 == 0 {
		return counts, fmt.Errorf("runs must be a positive odd integer, got %d", runs)
	}
	for _, e := range entries {
		if e.Label == "UNSURE" {
			counts.Unsure++
			continue
		}
		src := topicLinkNode{NodeID: e.Source.NodeID, Type: e.Source.Type, Summary: e.Source.Summary, Department: e.Source.Department}
		cand := topicLinkCandidate{
			topicLinkNode: topicLinkNode{NodeID: e.Cand.NodeID, Type: e.Cand.Type, Summary: e.Cand.Summary, Department: e.Cand.Department},
			Cosine:        e.Cand.Cosine,
			SharedIDs:     e.Cand.SharedIDs,
			CaseIDs:       e.Cand.CaseIDs,
		}
		tc := topicLinkContext{SourceWindow: e.Ctx.SourceWindow, CandWindow: e.Ctx.CandWindow, TimeDesc: e.Ctx.TimeDesc}
		same, evidenceVotes := 0, 0
		var votes []string
		for i := range runs {
			j, err := confirmTopicLink(ctx, deps, src, cand, tc)
			if err != nil {
				return counts, fmt.Errorf("edge %d vote %d: %w", e.EdgeID, i+1, err)
			}
			if j.SameTopic {
				same++
			}
			if strings.HasPrefix(j.Why, "evidence not found:") {
				evidenceVotes++
			}
			verdict := "DIFFERENT"
			if j.SameTopic {
				verdict = "SAME"
			}
			votes = append(votes, fmt.Sprintf("%s(%s)", verdict, j.Why))
		}
		kept := same*2 > runs
		if !kept && evidenceVotes*2 > runs {
			counts.EvidenceRejected++
		}
		switch e.Label {
		case "CORRECT":
			counts.Correct++
			if kept {
				counts.KeptCorrect++
			}
		case "WRONG":
			counts.Wrong++
			rc := counts.RefusedByCause[e.Cause]
			rc[1]++
			if !kept {
				counts.RefusedWrong++
				rc[0]++
			}
			counts.RefusedByCause[e.Cause] = rc
		}
		final := "DIFFERENT"
		if kept {
			final = "SAME"
		}
		counts.Log = append(counts.Log, fmt.Sprintf("%d %s %s %s votes=%s", e.EdgeID, e.Label, e.Cause, final, strings.Join(votes, " | ")))
	}
	return counts, nil
}

func evalEnvRuns() (int, error) {
	runs := 3
	if v := os.Getenv("AGENT_MEM_EVAL_RUNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n%2 == 0 {
			return 0, fmt.Errorf("AGENT_MEM_EVAL_RUNS=%q: want a positive odd integer", v)
		}
		runs = n
	}
	return runs, nil
}

func TestTopicJudgeFixture(t *testing.T) {
	if os.Getenv("AGENT_MEM_EVAL") != "1" || os.Getenv("AGENT_MEM_EVAL_FIXTURE") == "" {
		t.Skip("set AGENT_MEM_EVAL=1 and AGENT_MEM_EVAL_FIXTURE to run the judge fixture eval")
	}
	url, key := os.Getenv("AGENT_MEM_LLM_GATEWAY_URL"), os.Getenv("AGENT_MEM_LLM_GATEWAY_API_KEY")
	if url == "" || key == "" {
		t.Fatal("AGENT_MEM_LLM_GATEWAY_URL and AGENT_MEM_LLM_GATEWAY_API_KEY are required")
	}
	runs, err := evalEnvRuns()
	if err != nil {
		t.Fatal(err)
	}
	tier := os.Getenv("AGENT_MEM_EVAL_TIER")
	if tier == "" {
		tier = "cheap"
	}
	entries, err := loadJudgeFixture(os.Getenv("AGENT_MEM_EVAL_FIXTURE"))
	if err != nil {
		t.Fatal(err)
	}
	gem, err := withJudgeTier(NewGeminiAdapter(llmgateway.New(url, key, GraphEmbeddingDims)), tier)
	if err != nil {
		t.Fatal(err)
	}
	counts, err := runJudgeFixture(context.Background(), Deps{Gemini: gem}, entries, runs)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("tier=%s runs=%d", tier, runs)
	t.Log("edge_id label cause final votes")
	for _, l := range counts.Log {
		t.Log(l)
	}
	t.Logf("| metric | value |\n|---|---|\n| CORRECT kept | %d/%d |\n| WRONG refused | %d/%d |\n| UNSURE skipped | %d |\n| evidence rejected | %d |",
		counts.KeptCorrect, counts.Correct, counts.RefusedWrong, counts.Wrong, counts.Unsure, counts.EvidenceRejected)
	causes := make([]string, 0, len(counts.RefusedByCause))
	for c := range counts.RefusedByCause {
		causes = append(causes, c)
	}
	sort.Strings(causes)
	for _, c := range causes {
		rc := counts.RefusedByCause[c]
		t.Logf("cause %q: refused %d/%d", c, rc[0], rc[1])
	}
}

// judgeFixtureGateway speaks the llm-gateway /generate protocol ({system,user,tier} in,
// {backend,text} out) with replies scripted in request order.
type judgeFixtureGateway struct {
	srv     *httptest.Server
	mu      sync.Mutex
	replies []string
	tiers   []string
	users   []string
	fail    bool
}

func newJudgeFixtureGateway(t *testing.T) *judgeFixtureGateway {
	t.Helper()
	g := &judgeFixtureGateway{}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/generate" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			System string `json:"system"`
			User   string `json:"user"`
			Tier   string `json:"tier"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		n := len(g.users)
		g.users = append(g.users, req.User)
		g.tiers = append(g.tiers, req.Tier)
		reply := `{"same_topic":false,"confidence":0.5,"why":"script exhausted"}`
		if n < len(g.replies) {
			reply = g.replies[n]
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"backend": "fake", "text": reply})
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *judgeFixtureGateway) deps(t *testing.T, tier string) Deps {
	t.Helper()
	gem, err := withJudgeTier(NewGeminiAdapter(llmgateway.New(g.srv.URL, "k", GraphEmbeddingDims)), tier)
	if err != nil {
		t.Fatal(err)
	}
	return Deps{Gemini: gem}
}

func (g *judgeFixtureGateway) script(replies ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.replies, g.tiers, g.users = replies, nil, nil
}

func judgeFixtureReply(same bool, evA, evB string) string {
	b, _ := json.Marshal(map[string]any{
		"tag": "bug_incident", "same_topic": same, "confidence": 0.9,
		"topic": "t", "why": "w", "evidence_a": evA, "evidence_b": evB,
	})
	return string(b)
}

func newJudgeFixtureEntry(id int64, label, cause, sumA, sumB, timeDesc string) judgeFixtureEntry {
	return judgeFixtureEntry{
		EdgeID: id, Label: label, Cause: cause,
		Source: judgeFixtureNode{NodeID: fmt.Sprintf("jira:S%d", id), Type: "jira", Summary: sumA},
		Cand:   judgeFixtureNode{NodeID: fmt.Sprintf("jira:C%d", id), Type: "jira", Summary: sumB, Cosine: 0.8},
		Ctx:    judgeFixtureCtx{SourceWindow: "2026-01-01", CandWindow: "2026-01-02", TimeDesc: timeDesc},
	}
}

func TestTopicJudgeFixture_FakeGateway(t *testing.T) {
	const (
		sA = "Refund webhook retries duplicated the payout record for partner Acme."
		sB = "PR 123 deduplicates payout records written by the refund webhook retry path."
		qA = "duplicated the payout record"
		qB = "deduplicates payout records written"
		// E3 summaries never contain the quoted passages.
		s3A = "Quarterly planning notes for the hotels team roadmap review."
		s3B = "Hotels roadmap review: planning notes for the next quarter."
		bad = "a passage the artifacts never contain"
	)
	same := judgeFixtureReply(true, qA, qB)
	diff := `{"same_topic":false,"confidence":0.6,"why":"different work"}`
	missing := judgeFixtureReply(true, bad, bad)

	e1 := newJudgeFixtureEntry(1, "CORRECT", "", sA, sB, "TIME-E1")
	e2 := newJudgeFixtureEntry(2, "WRONG", "same-area", sA, sB, "TIME-E2")
	e3 := newJudgeFixtureEntry(3, "WRONG", "hallucinated-why", s3A, s3B, "TIME-E3")
	entries := []judgeFixtureEntry{e1, e2, e3}

	dir := t.TempDir()
	raw, _ := json.Marshal(entries)
	path := filepath.Join(dir, "fixture.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadJudgeFixture(path)
	if err != nil {
		t.Fatal(err)
	}

	gw := newJudgeFixtureGateway(t)
	gw.script(
		same, same, diff, // E1: kept by majority, not last vote
		diff, same, diff, // E2: refused
		missing, missing, missing, // E3
	)
	counts, err := runJudgeFixture(context.Background(), gw.deps(t, "cheap"), loaded, 3)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("majority", func(t *testing.T) {
		if counts.Correct != 1 || counts.KeptCorrect != 1 {
			t.Fatalf("counts = %+v", counts)
		}
		if rc := counts.RefusedByCause["same-area"]; rc != [2]int{1, 1} {
			t.Fatalf("same-area = %v, want {1,1}", rc)
		}
	})
	t.Run("time_desc reaches the judge", func(t *testing.T) {
		gw.mu.Lock()
		defer gw.mu.Unlock()
		if len(gw.users) != 9 {
			t.Fatalf("requests = %d, want 9", len(gw.users))
		}
		for i, u := range gw.users {
			if want := entries[i/3].Ctx.TimeDesc; !strings.Contains(u, want) {
				t.Fatalf("request %d lacks %q", i, want)
			}
		}
	})
	t.Run("tier switch", func(t *testing.T) {
		for tier, want := range map[string]string{"main": "summary", "cheap": "cheap"} {
			g := newJudgeFixtureGateway(t)
			g.script(same)
			if _, err := runJudgeFixture(context.Background(), g.deps(t, tier), []judgeFixtureEntry{e1}, 1); err != nil {
				t.Fatal(err)
			}
			g.mu.Lock()
			got := g.tiers
			g.mu.Unlock()
			if len(got) != 1 || got[0] != want {
				t.Fatalf("tier %s sent %v, want [%s]", tier, got, want)
			}
		}
	})
	t.Run("validation", func(t *testing.T) {
		g := newJudgeFixtureGateway(t)
		if _, err := runJudgeFixture(context.Background(), g.deps(t, "cheap"), []judgeFixtureEntry{e1}, 2); err == nil {
			t.Fatal("runs=2 accepted")
		}
		if _, err := withJudgeTier(nil, "x"); err == nil {
			t.Fatal("tier x accepted")
		}
		g.mu.Lock()
		g.fail = true
		g.mu.Unlock()
		if _, err := runJudgeFixture(context.Background(), g.deps(t, "cheap"), []judgeFixtureEntry{e1}, 1); err == nil {
			t.Fatal("gateway 500 did not return an error")
		}
	})
	t.Run("evidence_gate", func(t *testing.T) {
		if counts.Wrong != 2 || counts.RefusedWrong != 2 {
			t.Fatalf("Wrong=%d RefusedWrong=%d, want 2/2", counts.Wrong, counts.RefusedWrong)
		}
		if rc := counts.RefusedByCause["hallucinated-why"]; rc != [2]int{1, 1} {
			t.Fatalf("hallucinated-why = %v, want {1,1}", rc)
		}
		if counts.EvidenceRejected != 1 {
			t.Fatalf("EvidenceRejected = %d, want 1", counts.EvidenceRejected)
		}

		e4 := newJudgeFixtureEntry(4, "WRONG", "x", sA, sB, "TIME-E4")
		e5 := newJudgeFixtureEntry(5, "WRONG", "x", sA, sB, "TIME-E5")
		e6 := newJudgeFixtureEntry(6, "WRONG", "x", sA, sB, "TIME-E6")
		g := newJudgeFixtureGateway(t)
		g.script(
			missing, diff, same, // E4
			diff, same, missing, // E5
			missing, missing, same, // E6
		)
		c, err := runJudgeFixture(context.Background(), g.deps(t, "cheap"), []judgeFixtureEntry{e4, e5, e6}, 3)
		if err != nil {
			t.Fatal(err)
		}
		// E4/E5 end DIFFERENT (1 of 3 evidence votes, not a majority); E6 counts (2 of 3).
		if c.Wrong != 3 || c.RefusedWrong != 3 || c.EvidenceRejected != 1 {
			t.Fatalf("counts = %+v", c)
		}
	})
}
