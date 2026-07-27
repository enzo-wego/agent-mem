package handlers

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

// caseTestDB is a deliberately narrow alternative to the handlers_test testDB
// helper (unreachable from this internal test package): it never truncates, so
// pointing DATABASE_URL at a real database cannot destroy it. Every test below
// seeds ids under the slack:CT* prefix and deletes exactly those rows.
func caseTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping case-candidate integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("DB ping: %v", err)
	}
	clean := func() {
		pool.Exec(ctx, `DELETE FROM graph.topic_link_judgments WHERE source_node_id LIKE 'slack:CT%' OR target_node_id LIKE 'slack:CT%'`)
		pool.Exec(ctx, `DELETE FROM graph.artifact_index WHERE node_id LIKE 'slack:CT%'`)
		pool.Exec(ctx, `DELETE FROM graph.edges WHERE from_node_id LIKE 'slack:CT%' OR to_node_id LIKE 'slack:CT%'`)
		pool.Exec(ctx, `DELETE FROM graph.nodes WHERE id LIKE 'slack:CT%'`)
	}
	clean()
	t.Cleanup(func() {
		clean()
		pool.Close()
	})
	return pool
}

// caseSeedThread seeds a thread root with an indexed summary, since candidate
// generators require a thread_summary of at least 40 characters.
func caseSeedThread(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
INSERT INTO graph.nodes (id, type, natural_key, title, scope, machine_id)
VALUES ($1, 'slack_thread', $1, $1, 'slack:' || split_part($1,':',2), 'test')
ON CONFLICT (id) DO NOTHING`, id); err != nil {
		t.Fatalf("seed node %s: %v", id, err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO graph.artifact_index (node_id, summary, summary_kind, identifiers, refreshed_at, machine_id)
VALUES ($1, 'A summary long enough to clear the forty character substance floor.', 'thread_summary', '{}', NOW(), 'test')
ON CONFLICT (node_id) DO NOTHING`, id); err != nil {
		t.Fatalf("seed artifact_index %s: %v", id, err)
	}
}

func seedTopicEdge(t *testing.T, pool *pgxpool.Pool, from, to, meta string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO graph.edges (from_node_id, to_node_id, kind, metadata, machine_id)
VALUES ($1, $2, 'SAME_TOPIC', $3::jsonb, 'test')
ON CONFLICT (from_node_id, to_node_id, kind) DO UPDATE SET metadata=EXCLUDED.metadata`,
		from, to, meta); err != nil {
		t.Fatalf("seedTopicEdge %s->%s: %v", from, to, err)
	}
}

func caseMateIDs(cands []topicLinkCandidate) map[string][]string {
	out := map[string][]string{}
	for _, c := range cands {
		out[c.NodeID] = c.CaseIDs
	}
	return out
}

// TestCaseMateCandidates covers the triangle the pairwise judge cannot see —
// verified on slack:C048WV1BZTK:1784600389.693489, where one run confirmed the
// #ext-wego-juspay thread for order p0yy6hmqdw and refused the #payments-team
// thread for the SAME order, while those two were linked at confidence 1.0 —
// and the identifier shapes that must NOT nominate.
func TestCaseMateCandidates(t *testing.T) {
	ctx := context.Background()
	pool := caseTestDB(t)
	deps := Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "test"}

	const (
		s    = "slack:CT1:100" // the source
		p    = "slack:CT2:200" // confirmed same topic as s
		q    = "slack:CT3:300" // same case as p, via a payment ref  → nominate
		tkt  = "slack:CT4:400" // shares a Jira key with p           → no
		uuid = "slack:CT5:500" // shares a session UUID with p       → no
		word = "slack:CT6:600" // shares "scheduler1" with p         → no
		weak = "slack:CT7:700" // same case as p but low confidence  → no
	)
	for _, id := range []string{s, p, q, tkt, uuid, word, weak} {
		caseSeedThread(t, pool, id)
	}
	seedTopicEdge(t, pool, s, p, `{"method":"cosine-shortlist + llm-confirm","confidence":0.9}`)
	seedTopicEdge(t, pool, p, q, `{"method":"shared-identifier + llm-confirm","confidence":1,"shared_ids":["p0yy6hmqdw"]}`)
	seedTopicEdge(t, pool, p, tkt, `{"method":"shared-identifier + llm-confirm","confidence":1,"shared_ids":["PAY-2255"]}`)
	seedTopicEdge(t, pool, p, uuid, `{"method":"shared-identifier + llm-confirm","confidence":1,"shared_ids":["3e9a0bee-319c-422c-ae50-3509ad253159"]}`)
	seedTopicEdge(t, pool, p, word, `{"method":"shared-identifier + llm-confirm","confidence":1,"shared_ids":["scheduler1"]}`)
	seedTopicEdge(t, pool, p, weak, `{"method":"shared-identifier + llm-confirm","confidence":0.6,"shared_ids":["p9y0yhtbd5"]}`)

	cands, err := caseMateCandidates(ctx, deps, s)
	if err != nil {
		t.Fatalf("caseMateCandidates: %v", err)
	}
	got := caseMateIDs(cands)

	if ids, ok := got[q]; !ok {
		t.Errorf("payment-ref case-mate %s not nominated; got %v", q, got)
	} else if len(ids) != 1 || ids[0] != "p0yy6hmqdw" {
		t.Errorf("case ids for %s = %v, want [p0yy6hmqdw]", q, ids)
	}
	for name, id := range map[string]string{
		"shared Jira key (tie-breaker #2)":  tkt,
		"session/artifact UUID":             uuid,
		"word with a trailing counter":      word,
		"low-confidence identifier edge":    weak,
		"the source itself":                 s,
		"the confirmed partner it came via": p,
	} {
		if _, bad := got[id]; bad {
			t.Errorf("%s nominated a case-mate (%s); it names no concrete case", name, id)
		}
	}
}

// TestCaseMateCandidatesDedupesSharedMate checks the shape that broke the
// earlier asserting implementation in production: one node reachable as the
// case-mate of two confirmed partners must be nominated once.
func TestCaseMateCandidatesDedupesSharedMate(t *testing.T) {
	ctx := context.Background()
	pool := caseTestDB(t)
	deps := Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "test"}

	const (
		s  = "slack:CT1:100"
		p1 = "slack:CT2:200"
		p2 = "slack:CT6:600"
		q  = "slack:CT3:300"
	)
	for _, id := range []string{s, p1, p2, q} {
		caseSeedThread(t, pool, id)
	}
	seedTopicEdge(t, pool, s, p1, `{"method":"cosine-shortlist + llm-confirm","confidence":0.9}`)
	seedTopicEdge(t, pool, s, p2, `{"method":"cosine-shortlist + llm-confirm","confidence":0.95}`)
	seedTopicEdge(t, pool, p1, q, `{"method":"shared-identifier + llm-confirm","confidence":1,"shared_ids":["p0yy6hmqdw"]}`)
	seedTopicEdge(t, pool, p2, q, `{"method":"shared-identifier + llm-confirm","confidence":1,"shared_ids":["p0yy6hmqdw"]}`)

	cands, err := caseMateCandidates(ctx, deps, s)
	if err != nil {
		t.Fatalf("caseMateCandidates: %v", err)
	}
	seen := 0
	for _, c := range cands {
		if c.NodeID == q {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("case-mate %s nominated %d times, want exactly 1", q, seen)
	}
}

// TestConfirmTopicLinkShowsCaseContextAsEvidence pins the prompt contract: the
// judge is told about the shared case AND reminded that a release thread or a
// passing PR mention still loses. Sampling found that identifier joining a case
// thread to "Payments service v0.48.5 deployment".
func TestConfirmTopicLinkShowsCaseContextAsEvidence(t *testing.T) {
	gem := &mockGemini{}
	gem.cheapGenerateResult = func() (string, error) {
		return `{"tag":"bug_incident","same_topic":true,"confidence":0.9,"topic":"t","why":"w"}`, nil
	}
	deps := Deps{Logger: zerolog.Nop(), Gemini: gem}
	_, err := confirmTopicLink(context.Background(), deps,
		topicLinkNode{NodeID: "a", Type: "slack_thread", Summary: "source summary"},
		topicLinkCandidate{
			topicLinkNode: topicLinkNode{NodeID: "b", Type: "slack_thread", Summary: "candidate summary"},
			CaseVia:       "slack:CX:1",
			CaseIDs:       []string{"p0yy6hmqdw"},
		},
		topicLinkContext{TimeDesc: "activity windows overlap"})
	if err != nil {
		t.Fatalf("confirmTopicLink: %v", err)
	}
	for _, want := range []string{"p0yy6hmqdw", "same concrete case", "ALREADY CONFIRMED", "tie-breakers #2 and #3"} {
		if !strings.Contains(gem.cheapGenerateUser, want) {
			t.Errorf("prompt missing %q; got:\n%s", want, gem.cheapGenerateUser)
		}
	}
}

// TestTopicLinkContentHashKeyedOnCaseIDs: gaining case context is new evidence,
// so a verdict reached without it must not be served from cache.
func TestTopicLinkContentHashKeyedOnCaseIDs(t *testing.T) {
	base := topicLinkContentHash("a", "b", "sa", "sb", nil, "overlap", nil)
	withCase := topicLinkContentHash("a", "b", "sa", "sb", nil, "overlap", []string{"p0yy6hmqdw"})
	if base == withCase {
		t.Error("content hash ignores case ids; a pre-context refusal would be served from cache")
	}
}
