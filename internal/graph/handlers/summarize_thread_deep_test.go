package handlers

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

// TestSummarizeThread_DeepSummary verifies the job stores the one-line topic AND
// the deep overview + highlights for a multi-message thread.
func TestSummarizeThread_DeepSummary(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	for _, tbl := range []string{"graph.thread_summaries", "graph.nodes"} {
		if _, err := pool.Exec(ctx, "DELETE FROM "+tbl); err != nil {
			t.Fatalf("clean %s: %v", tbl, err)
		}
	}

	// Seed a 2-message thread in channel C, thread_ts 100.000001. summarize_thread
	// reads the author from metadata->'author'->>'display_name'.
	for _, n := range []struct{ id, ts, author string }{
		{"slack:C:100.000001", "100.000001", "Ross"},
		{"slack:C:100.000002", "100.000002", "IC One"},
	} {
		meta := `{"ts":"` + n.ts + `","thread_ts":"100.000001","author":{"display_name":"` + n.author + `"}}`
		_, err := pool.Exec(ctx, `
INSERT INTO graph.nodes (id, type, natural_key, body, scope, metadata, machine_id)
VALUES ($1,'slack',$1,$2,'slack:C',$3::jsonb,'test')
ON CONFLICT (id) DO NOTHING`, n.id, "msg "+n.author, meta)
		if err != nil {
			t.Fatalf("seed %s: %v", n.id, err)
		}
	}

	gem := &mockGemini{}
	gem.generateResult = func() (string, error) {
		return `{"topic":"Refund stuck on TripleA",` +
			`"overview":"Ross reported refunds returning none; the team is investigating.",` +
			`"highlights":["Ross raised the refund bug","Team began investigating"]}`, nil
	}
	deps := Deps{DB: pool, Gemini: gem, Logger: zerolog.Nop()}

	payload, _ := json.Marshal(map[string]string{"channel_id": "C", "thread_ts": "100.000001"})
	if err := NewSummarizeThreadHandler(deps).Handler(ctx, payload); err != nil {
		t.Fatalf("handler: %v", err)
	}

	var summary, overview string
	var hlRaw []byte
	err = pool.QueryRow(ctx,
		`SELECT summary, overview, highlights FROM graph.thread_summaries WHERE channel_id='C' AND thread_ts='100.000001'`).
		Scan(&summary, &overview, &hlRaw)
	if err != nil {
		t.Fatalf("read summary: %v", err)
	}
	if summary != "Refund stuck on TripleA" {
		t.Errorf("summary = %q", summary)
	}
	if overview == "" {
		t.Errorf("overview is empty")
	}
	var hl []string
	if err := json.Unmarshal(hlRaw, &hl); err != nil || len(hl) != 2 {
		t.Errorf("highlights = %v (err %v), want 2 items", hl, err)
	}
}
