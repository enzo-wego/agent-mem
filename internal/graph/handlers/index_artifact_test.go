package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/agent-mem/agent-mem/internal/gemini"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/pgvector/pgvector-go"
)

func TestIndexArtifactHandler_BadPayload(t *testing.T) {
	deps := Deps{Logger: zerolog.Nop(), MachineID: "test"}
	h := NewIndexArtifactHandler(deps)

	err := h.Handler(context.Background(), []byte("not json"))
	if err == nil {
		t.Fatal("expected error for bad JSON")
	}
}

func TestIndexArtifactHandler_MissingNodeID(t *testing.T) {
	deps := Deps{Logger: zerolog.Nop(), MachineID: "test"}
	h := NewIndexArtifactHandler(deps)

	payload, _ := json.Marshal(indexArtifactPayload{Force: false})
	err := h.Handler(context.Background(), payload)
	if err == nil {
		t.Fatal("expected error when node_id is empty")
	}
}

func TestIndexArtifactHandler_SkipsWithDB(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
	// Integration test placeholder — covered by DB-backed tests.
}

func TestIndexArtifactHandler_DuplicateHeuristicSkipsEmbedding(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	ctx := context.Background()

	const (
		representativeNodeID = "jira:PAY-1"
		targetNodeID         = "jira:PAY-2"
		body                 = "Repeated payment error"
	)
	if _, err := pool.Exec(ctx, `
INSERT INTO graph.nodes (id, type, natural_key, body, machine_id)
VALUES ($1, 'jira', $1, $3, 'test'),
       ($2, 'jira', $2, $3, 'test')`,
		representativeNodeID, targetNodeID, body); err != nil {
		t.Fatalf("seed nodes: %v", err)
	}

	vector := make([]float32, GraphEmbeddingDims)
	vector[0] = 1
	if _, err := pool.Exec(ctx, `
INSERT INTO graph.artifact_index
  (node_id, summary, summary_kind, embedding, refreshed_at, machine_id)
VALUES ($1, $2, 'heuristic', $3, NOW(), 'test')`,
		representativeNodeID, heuristicSummary(targetNodeID, body), pgvector.NewVector(vector)); err != nil {
		t.Fatalf("seed representative artifact: %v", err)
	}

	gemini := &mockGemini{embedResult: func() ([]float32, error) {
		return vector, nil
	}}
	deps := Deps{
		DB:        pool,
		Gemini:    gemini,
		Logger:    zerolog.Nop(),
		MachineID: "test",
	}
	payload, err := json.Marshal(indexArtifactPayload{NodeID: targetNodeID, Force: true})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := NewIndexArtifactHandler(deps).Handler(ctx, payload); err != nil {
		t.Fatalf("index artifact: %v", err)
	}

	if calls := gemini.embedCalls.Load(); calls != 0 {
		t.Fatalf("embedding calls = %d, want 0", calls)
	}
	var representativeEmbeddingIsNull bool
	if err := pool.QueryRow(ctx, `
SELECT embedding IS NULL
FROM graph.artifact_index
WHERE node_id = $1`, representativeNodeID).Scan(&representativeEmbeddingIsNull); err != nil {
		t.Fatalf("read representative artifact: %v", err)
	}
	if representativeEmbeddingIsNull {
		t.Fatal("representative heuristic embedding is NULL")
	}
	var summaryKind string
	var embeddingIsNull bool
	if err := pool.QueryRow(ctx, `
SELECT summary_kind, embedding IS NULL
FROM graph.artifact_index
WHERE node_id = $1`, targetNodeID).Scan(&summaryKind, &embeddingIsNull); err != nil {
		t.Fatalf("read indexed artifact: %v", err)
	}
	if summaryKind != "heuristic" {
		t.Fatalf("summary kind = %q, want heuristic", summaryKind)
	}
	if !embeddingIsNull {
		t.Fatal("duplicate heuristic embedding is non-NULL")
	}
}

func TestIndexArtifactHandler_ConcurrentDuplicateHeuristicsKeepOneEmbedding(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	ctx := context.Background()

	const body = "Concurrent repeated payment error"
	nodeIDs := []string{"jira:PAY-3", "jira:PAY-4"}
	if _, err := pool.Exec(ctx, `
INSERT INTO graph.nodes (id, type, natural_key, body, machine_id)
VALUES ($1, 'jira', $1, $3, 'test'),
       ($2, 'jira', $2, $3, 'test')`,
		nodeIDs[0], nodeIDs[1], body); err != nil {
		t.Fatalf("seed nodes: %v", err)
	}
	summary := heuristicSummary(nodeIDs[0], body)

	lockTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin advisory-lock transaction: %v", err)
	}
	if _, err := lockTx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))`,
		"heuristic", summary); err != nil {
		_ = lockTx.Rollback(ctx)
		t.Fatalf("acquire advisory lock: %v", err)
	}

	vector := make([]float32, GraphEmbeddingDims)
	vector[0] = 1
	releaseEmbedding := make(chan struct{})
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { close(releaseEmbedding) })
	}
	t.Cleanup(func() {
		release()
		_ = lockTx.Rollback(context.Background())
	})

	gemini := &mockGemini{embedResult: func() ([]float32, error) {
		<-releaseEmbedding
		return vector, nil
	}}
	handler := NewIndexArtifactHandler(Deps{
		DB:        pool,
		Gemini:    gemini,
		Logger:    zerolog.Nop(),
		MachineID: "test",
	}).Handler

	results := make(chan error, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		payload, err := json.Marshal(indexArtifactPayload{NodeID: nodeID, Force: true})
		if err != nil {
			t.Fatalf("marshal payload: %v", err)
		}
		go func(payload []byte) {
			results <- handler(ctx, payload)
		}(payload)
	}

	readyDeadline := time.NewTimer(5 * time.Second)
	defer readyDeadline.Stop()
	readyTicker := time.NewTicker(10 * time.Millisecond)
	defer readyTicker.Stop()
readyLoop:
	for {
		select {
		case <-readyTicker.C:
			var advisoryWaiters int
			if err := pool.QueryRow(ctx, `
SELECT count(*)
FROM pg_stat_activity
WHERE datname = current_database()
  AND wait_event = 'advisory'`).Scan(&advisoryWaiters); err != nil {
				t.Fatalf("count advisory waiters: %v", err)
			}
			if advisoryWaiters >= len(nodeIDs) || gemini.embedCalls.Load() >= int32(len(nodeIDs)) {
				break readyLoop
			}
		case <-readyDeadline.C:
			t.Fatal("handlers did not reach the guarded embedding decision")
		}
	}

	if err := lockTx.Commit(ctx); err != nil {
		t.Fatalf("release advisory lock: %v", err)
	}

	embedDeadline := time.NewTimer(5 * time.Second)
	defer embedDeadline.Stop()
	embedTicker := time.NewTicker(10 * time.Millisecond)
	defer embedTicker.Stop()
	for gemini.embedCalls.Load() == 0 {
		select {
		case <-embedTicker.C:
		case <-embedDeadline.C:
			t.Fatal("representative embedding call did not start")
		}
	}
	release()

	for range nodeIDs {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("index artifact: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent index artifact handler did not finish")
		}
	}

	if calls := gemini.embedCalls.Load(); calls != 1 {
		t.Fatalf("embedding calls = %d, want 1", calls)
	}
	var total, nonNull int
	if err := pool.QueryRow(ctx, `
SELECT count(*), count(embedding)
FROM graph.artifact_index
WHERE node_id = ANY($1)`, nodeIDs).Scan(&total, &nonNull); err != nil {
		t.Fatalf("read concurrent artifacts: %v", err)
	}
	if total != len(nodeIDs) || nonNull != 1 {
		t.Fatalf("artifacts total/non-NULL = %d/%d, want %d/1", total, nonNull, len(nodeIDs))
	}
}

func TestHeuristicSummary(t *testing.T) {
	cases := []struct {
		nodeID string
		body   string
		wantIn string // the result should contain or start with this
	}{
		{
			nodeID: "jira:PAY-1234",
			body:   "First paragraph.\n\nSecond paragraph.",
			wantIn: "First paragraph.",
		},
		{
			nodeID: "gh_pr:wego/payments#42",
			body:   "PR title\nline2\nline3\nline4",
			wantIn: "PR title",
		},
		{
			nodeID: "slack:C123:1234.5678",
			body:   "Short message",
			wantIn: "Short message",
		},
	}

	for _, c := range cases {
		got := heuristicSummary(c.nodeID, c.body)
		if got == "" {
			t.Errorf("heuristicSummary(%q, ...) returned empty string", c.nodeID)
		}
		if len(got) > 200 {
			t.Errorf("heuristicSummary result exceeds 200 chars: %d", len(got))
		}
	}
}

// Regression: truncation must not split a multi-byte rune — a byte slice
// produced invalid UTF-8 that Postgres rejected (SQLSTATE 22021).
func TestHeuristicSummary_TruncatesOnRuneBoundary(t *testing.T) {
	body := strings.Repeat("é", 300) // 2 bytes each, > 200 runes
	for _, nodeID := range []string{"jira:PAY-1", "gh_pr:wego/x#1", "slack:C1:1.2"} {
		got := heuristicSummary(nodeID, body)
		if !utf8.ValidString(got) {
			t.Errorf("%s: result is not valid UTF-8: %q", nodeID, got)
		}
		if n := utf8.RuneCountInString(got); n > 200 {
			t.Errorf("%s: result exceeds 200 runes: %d", nodeID, n)
		}
	}
}

func TestIndexSummaryForSlackRootPrefersThreadSummary(t *testing.T) {
	got, kind := indexSummaryForSlackRoot("Email blacklist", "Checkout payment links are blocking specific emails.")
	if got != "Email blacklist\n\nCheckout payment links are blocking specific emails." {
		t.Fatalf("summary = %q", got)
	}
	if kind != "thread_summary" {
		t.Fatalf("summary kind = %q, want thread_summary", kind)
	}
}

func TestIndexSummaryForSlackRootFallsBackWhenSummaryMissing(t *testing.T) {
	got, kind := indexSummaryForSlackRoot("", "")
	if got != "" {
		t.Fatalf("summary = %q, want empty", got)
	}
	if kind != "" {
		t.Fatalf("summary kind = %q, want empty", kind)
	}
}

func TestDecisionsBlock(t *testing.T) {
	for _, tc := range []struct {
		name string
		ds   []threadDecision
		want string
	}{
		{"text", []threadDecision{{Text: "Use ledger A"}, {Text: "  Ship\n v2  "}, {Text: "   "}}, "- Use ledger A\n- Ship v2"},
		{"nil", nil, ""},
		{"blank", []threadDecision{{Text: " "}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := decisionsBlock(tc.ds); got != tc.want {
				t.Fatalf("decisionsBlock = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDecisionsBlock_Caps(t *testing.T) {
	ds := make([]threadDecision, 10)
	for i := range ds {
		ds[i].Text = fmt.Sprintf("D%02d", i+1)
	}
	want := "- D03\n- D04\n- D05\n- D06\n- D07\n- D08\n- D09\n- D10"
	if got := decisionsBlock(ds); got != want {
		t.Fatalf("latest decisions = %q, want %q", got, want)
	}
	got := decisionsBlock([]threadDecision{{Text: strings.Repeat("é", 300)}})
	if want := "- " + strings.Repeat("é", 200); got != want || !utf8.ValidString(got) {
		t.Fatalf("rune truncation = %q, want %q and valid UTF-8", got, want)
	}
	ds = make([]threadDecision, 20)
	for i := range ds {
		ds[i].Text = strings.Repeat("x", 500)
	}
	if got := decisionsBlock(ds); utf8.RuneCountInString(got) != 1623 {
		t.Fatalf("block has %d runes, want 1623", utf8.RuneCountInString(got))
	}
}

// recordingGemini records every embedding input; the rest is mockGemini.
type recordingGemini struct {
	*mockGemini
	inputs []string
}

func (r *recordingGemini) EmbedWithOptions(ctx context.Context, text string, o gemini.EmbedOptions) ([]float32, error) {
	r.inputs = append(r.inputs, text)
	return r.mockGemini.EmbedWithOptions(ctx, text, o)
}

func decisionIndexTestDB(t *testing.T) (*pgxpool.Pool, *recordingGemini) {
	t.Helper()
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	cleanup := func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM graph.thread_summaries WHERE channel_id = 'CDKW'`); err != nil {
			t.Fatalf("clean thread summaries: %v", err)
		}
	}
	cleanup()
	t.Cleanup(cleanup)
	vector := make([]float32, GraphEmbeddingDims)
	vector[0] = 1
	return pool, &recordingGemini{mockGemini: &mockGemini{embedResult: func() ([]float32, error) {
		return vector, nil
	}}}
}

func seedDecisionIndexThread(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
INSERT INTO graph.nodes (id, type, natural_key, body, scope, metadata, machine_id)
VALUES ('slack:CDKW:900.000001', 'slack', 'slack:CDKW:900.000001', 'kickoff about refunds', 'slack:CDKW',
        '{"ts":"900.000001","thread_ts":"900.000001"}', 'test'),
       ('slack:CDKW:900.000002', 'slack', 'slack:CDKW:900.000002', 'ok', 'slack:CDKW',
        '{"ts":"900.000002","thread_ts":"900.000001"}', 'test')`); err != nil {
		t.Fatalf("seed nodes: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO graph.thread_summaries (channel_id, thread_ts, signature, summary, overview, decisions)
VALUES ('CDKW', '900.000001', 'sig', 'WOMBATTOPIC flow', 'Team discussed refunds.',
        '[{"text":"Use the QUOKKAPAY ledger for partial refunds","by":"Lan","date":"1970-01-01","ts":"900.000002"}]')`); err != nil {
		t.Fatalf("seed summary: %v", err)
	}
}

func runDecisionIndex(t *testing.T, pool *pgxpool.Pool, rec *recordingGemini, nodeID string) {
	t.Helper()
	payload, err := json.Marshal(indexArtifactPayload{NodeID: nodeID, Force: true, SkipJudging: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := NewIndexArtifactHandler(Deps{DB: pool, Gemini: rec, Logger: zerolog.Nop(), MachineID: "test"}).
		Handler(context.Background(), payload); err != nil {
		t.Fatalf("index artifact: %v", err)
	}
}

func decisionIndexSearch(t *testing.T, s *Search, query string) []struct {
	ID        string           `json:"id"`
	Summary   string           `json:"summary"`
	Match     []string         `json:"match"`
	Decisions []threadDecision `json:"decisions"`
} {
	t.Helper()
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/graph/search?"+query, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("search status %d: %s", w.Code, w.Body.String())
	}
	var response struct {
		Results []struct {
			ID        string           `json:"id"`
			Summary   string           `json:"summary"`
			Match     []string         `json:"match"`
			Decisions []threadDecision `json:"decisions"`
		} `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode search: %v", err)
	}
	return response.Results
}

func TestIndexArtifact_DecisionKeywordFindsThread(t *testing.T) {
	pool, rec := decisionIndexTestDB(t)
	seedDecisionIndexThread(t, pool)
	s, err := NewSearch(pool)
	if err != nil {
		t.Fatal(err)
	}
	if got := decisionIndexSearch(t, s, "q=QUOKKAPAY&match=hybrid"); len(got) != 0 {
		t.Fatalf("before indexing: %v, want no results", got)
	}
	runDecisionIndex(t, pool, rec, "slack:CDKW:900.000001")
	var summary, kind, decisions string
	var embeddingNull bool
	if err := pool.QueryRow(context.Background(), `
SELECT summary, summary_kind, decisions_text, embedding IS NULL
FROM graph.artifact_index WHERE node_id = 'slack:CDKW:900.000001'`).
		Scan(&summary, &kind, &decisions, &embeddingNull); err != nil {
		t.Fatal(err)
	}
	if summary != "WOMBATTOPIC flow\n\nTeam discussed refunds." || kind != "thread_summary" ||
		decisions != "- Use the QUOKKAPAY ledger for partial refunds" || embeddingNull {
		t.Fatalf("index row: summary=%q kind=%q decisions=%q embeddingNull=%v", summary, kind, decisions, embeddingNull)
	}
	wantInputs := []string{"WOMBATTOPIC flow\n\nTeam discussed refunds.\n\nDecisions:\n- Use the QUOKKAPAY ledger for partial refunds"}
	if !reflect.DeepEqual(rec.inputs, wantInputs) {
		t.Fatalf("embedding inputs = %q, want %q", rec.inputs, wantInputs)
	}
	var jobs, skipJobs int
	if err := pool.QueryRow(context.Background(), `
SELECT count(*), count(*) FILTER (WHERE payload->>'skip_judging' = 'true')
FROM graph.jobs WHERE type = 'link_topics'`).Scan(&jobs, &skipJobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 || skipJobs != 1 {
		t.Fatalf("link_topics jobs=%d skip_judging jobs=%d, want 1 each", jobs, skipJobs)
	}
	got := decisionIndexSearch(t, s, "q=QUOKKAPAY&match=hybrid")
	if len(got) != 1 {
		t.Fatalf("decision search = %v, want one result", got)
	}
	if got[0].ID != "slack:CDKW:900.000001" || !reflect.DeepEqual(got[0].Match, []string{"keyword"}) ||
		got[0].Summary != "Team discussed refunds." || len(got[0].Decisions) != 1 ||
		got[0].Decisions[0].Text != "Use the QUOKKAPAY ledger for partial refunds" {
		t.Fatalf("decision result = %+v", got[0])
	}
	// The summary is indexed by design: a summary-only word finds the thread too.
	if got := decisionIndexSearch(t, s, "q=WOMBATTOPIC&match=hybrid"); len(got) != 1 || got[0].ID != "slack:CDKW:900.000001" {
		t.Fatalf("topic search = %v, want the thread", got)
	}
}

func TestIndexArtifact_DefaultSearchSummaryHasNoDecisions(t *testing.T) {
	pool, rec := decisionIndexTestDB(t)
	seedDecisionIndexThread(t, pool)
	runDecisionIndex(t, pool, rec, "slack:CDKW:900.000001")
	s, err := NewSearch(pool)
	if err != nil {
		t.Fatal(err)
	}
	got := decisionIndexSearch(t, s, "q=kickoff")
	if len(got) != 1 {
		t.Fatalf("default search = %v, want one result", got)
	}
	if got[0].ID != "slack:CDKW:900.000001" || got[0].Summary != "WOMBATTOPIC flow\n\nTeam discussed refunds." ||
		strings.Contains(got[0].Summary, "QUOKKAPAY") || strings.Contains(got[0].Summary, "Decisions:") {
		t.Fatalf("default result = %+v", got[0])
	}
}

func TestIndexArtifact_ThreadRootWithoutDecisionsWritesEmpty(t *testing.T) {
	pool, rec := decisionIndexTestDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
INSERT INTO graph.nodes (id, type, natural_key, body, scope, metadata, machine_id)
VALUES ('slack:CDKW:910.000001', 'slack', 'slack:CDKW:910.000001', 'hello', 'slack:CDKW',
        '{"ts":"910.000001","thread_ts":"910.000001"}', 'test'),
       ('jira:PAY-77', 'jira', 'jira:PAY-77', 'Some jira text', NULL, '{}', 'test')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO graph.thread_summaries (channel_id, thread_ts, signature, summary, overview, decisions)
VALUES ('CDKW', '910.000001', 'sig', 'T', 'O', NULL)`); err != nil {
		t.Fatal(err)
	}
	runDecisionIndex(t, pool, rec, "slack:CDKW:910.000001")
	var summary string
	var decisions *string
	if err := pool.QueryRow(ctx, `
SELECT summary, decisions_text FROM graph.artifact_index WHERE node_id = 'slack:CDKW:910.000001'`).
		Scan(&summary, &decisions); err != nil {
		t.Fatal(err)
	}
	if summary != "T\n\nO" || decisions == nil || *decisions != "" {
		t.Fatalf("root summary=%q decisions=%v, want T\\n\\nO and non-NULL empty", summary, decisions)
	}
	if want := []string{"T\n\nO"}; !reflect.DeepEqual(rec.inputs, want) {
		t.Fatalf("embedding inputs = %q, want %q", rec.inputs, want)
	}
	runDecisionIndex(t, pool, rec, "jira:PAY-77")
	var decisionsNull bool
	if err := pool.QueryRow(ctx, `
SELECT decisions_text IS NULL FROM graph.artifact_index WHERE node_id = 'jira:PAY-77'`).
		Scan(&decisionsNull); err != nil {
		t.Fatal(err)
	}
	if !decisionsNull {
		t.Fatal("Jira decisions_text must be NULL")
	}
}
