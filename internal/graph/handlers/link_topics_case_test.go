package handlers

import (
	"context"
	"os"
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
		t.Skip("DATABASE_URL not set; skipping case-propagation integration test")
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

func caseSeedNode(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO graph.nodes (id, type, natural_key, title, machine_id)
VALUES ($1, 'slack_thread', $1, $1, 'test')
ON CONFLICT (id) DO NOTHING`, id); err != nil {
		t.Fatalf("caseSeedNode %s: %v", id, err)
	}
}

// seedTopicEdge inserts a SAME_TOPIC edge carrying the metadata the linker
// writes (method / confidence / shared_ids), which is what propagation reads.
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

func sameTopicEdgeMethod(t *testing.T, pool *pgxpool.Pool, a, b string) (string, bool) {
	t.Helper()
	var method string
	err := pool.QueryRow(context.Background(), `
SELECT COALESCE(metadata->>'method','')
FROM graph.edges
WHERE kind='SAME_TOPIC' AND from_node_id=LEAST($1,$2) AND to_node_id=GREATEST($1,$2)`,
		a, b).Scan(&method)
	if err != nil {
		return "", false
	}
	return method, true
}

// TestPropagateCaseTopics covers the triangle the pairwise judge cannot see:
// two threads about ONE payment are one case, so a verdict against either holds
// for both. Verified against production, where the judge confirmed the
// #ext-wego-juspay thread for order p0yy6hmqdw and refused the #payments-team
// thread for the same order in the same run.
func TestPropagateCaseTopics(t *testing.T) {
	ctx := context.Background()
	pool := caseTestDB(t)
	t.Cleanup(func() {
		pool.Exec(ctx, `DELETE FROM graph.topic_link_judgments WHERE source_node_id LIKE 'slack:CT%' OR target_node_id LIKE 'slack:CT%'`)
	})
	deps := Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "test"}

	// s ── judged same ── p ── same case (p0yy6hmqdw) ── q   → propagate s~q
	//                      p ── shared PAY ticket ────── r   → must NOT propagate
	//                                                    q ── same case ── u
	//                                                        → must NOT chain from the propagated s~q
	const (
		s = "slack:CT1:100"
		p = "slack:CT2:200"
		q = "slack:CT3:300"
		r = "slack:CT4:400"
		u = "slack:CT5:500"
	)
	for _, id := range []string{s, p, q, r, u} {
		caseSeedNode(t, pool, id)
	}
	seedTopicEdge(t, pool, s, p, `{"method":"cosine-shortlist + llm-confirm","confidence":0.9,"tag":"bug_incident","topic":"Duplicate refund records"}`)
	seedTopicEdge(t, pool, p, q, `{"method":"shared-identifier + llm-confirm","confidence":1,"shared_ids":["p0yy6hmqdw"]}`)
	seedTopicEdge(t, pool, p, r, `{"method":"shared-identifier + llm-confirm","confidence":1,"shared_ids":["PAY-2255"]}`)
	seedTopicEdge(t, pool, q, u, `{"method":"shared-identifier + llm-confirm","confidence":1,"shared_ids":["p0yy6hmqdw"]}`)

	// The stale verdict the panel would otherwise render as "✕ different".
	if err := saveTopicLinkJudgment(ctx, deps, s, q, "stale-hash", topicLinkJudgment{
		SameTopic: false, Confidence: 0.9, Tag: "bug_incident", Why: "different payment IDs",
	}); err != nil {
		t.Fatalf("seed judgment: %v", err)
	}

	if err := propagateCaseTopics(ctx, deps, s); err != nil {
		t.Fatalf("propagateCaseTopics: %v", err)
	}

	if method, ok := sameTopicEdgeMethod(t, pool, s, q); !ok || method != casePropagationMethod {
		t.Errorf("s~q edge: method=%q ok=%v, want %q", method, ok, casePropagationMethod)
	}
	var same bool
	var why string
	if err := pool.QueryRow(ctx, `
SELECT same_topic, why FROM graph.topic_link_judgments
WHERE source_node_id=LEAST($1,$2) AND target_node_id=GREATEST($1,$2)`, s, q).Scan(&same, &why); err != nil {
		t.Fatalf("read s~q judgment: %v", err)
	}
	if !same {
		t.Errorf("s~q judgment still says different topic; why=%q", why)
	}

	// A shared TICKET is not a shared case (tie-breaker #2).
	if method, ok := sameTopicEdgeMethod(t, pool, s, r); ok {
		t.Errorf("s~r edge created via a shared Jira key (method=%q); tickets are not cases", method)
	}

	// One hop only: a second pass must not chain off its own output.
	if err := propagateCaseTopics(ctx, deps, s); err != nil {
		t.Fatalf("propagateCaseTopics (second pass): %v", err)
	}
	if method, ok := sameTopicEdgeMethod(t, pool, s, u); ok {
		t.Errorf("s~u edge chained through a propagated edge (method=%q)", method)
	}

	// A directly judged edge keeps its own metadata.
	if method, _ := sameTopicEdgeMethod(t, pool, s, p); method != "cosine-shortlist + llm-confirm" {
		t.Errorf("s~p method=%q, want the original judged method", method)
	}
}

// TestPropagateCaseTopicsKeepsConfirmedVerdicts checks the guard on the
// judgment upsert: propagation fills gaps and overwrites refusals, never a
// verdict the judge already confirmed with its own reasoning.
func TestPropagateCaseTopicsKeepsConfirmedVerdicts(t *testing.T) {
	ctx := context.Background()
	pool := caseTestDB(t)
	t.Cleanup(func() {
		pool.Exec(ctx, `DELETE FROM graph.topic_link_judgments WHERE source_node_id LIKE 'slack:CT%' OR target_node_id LIKE 'slack:CT%'`)
	})
	deps := Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "test"}

	const (
		s = "slack:CT1:100"
		p = "slack:CT2:200"
		q = "slack:CT3:300"
	)
	for _, id := range []string{s, p, q} {
		caseSeedNode(t, pool, id)
	}
	seedTopicEdge(t, pool, s, p, `{"method":"cosine-shortlist + llm-confirm","confidence":0.9}`)
	seedTopicEdge(t, pool, p, q, `{"method":"shared-identifier + llm-confirm","confidence":1,"shared_ids":["p0yy6hmqdw"]}`)
	if err := saveTopicLinkJudgment(ctx, deps, s, q, "real-hash", topicLinkJudgment{
		SameTopic: true, Confidence: 0.95, Tag: "bug_incident", Why: "judged directly",
	}); err != nil {
		t.Fatalf("seed judgment: %v", err)
	}

	if err := propagateCaseTopics(ctx, deps, s); err != nil {
		t.Fatalf("propagateCaseTopics: %v", err)
	}

	var hash, why string
	if err := pool.QueryRow(ctx, `
SELECT content_hash, why FROM graph.topic_link_judgments
WHERE source_node_id=LEAST($1,$2) AND target_node_id=GREATEST($1,$2)`, s, q).Scan(&hash, &why); err != nil {
		t.Fatalf("read judgment: %v", err)
	}
	if hash != "real-hash" || why != "judged directly" {
		t.Errorf("propagation overwrote a confirmed verdict: hash=%q why=%q", hash, why)
	}
}
