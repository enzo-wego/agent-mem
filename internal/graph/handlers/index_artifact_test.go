package handlers

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

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
