package database

import (
	"context"
	"os"
	"testing"
	"time"
)

// Smoke-tests the llm_key_blocks queries against a scratch DB. Set
// AGENT_MEM_TEST_DATABASE_URL to run; skipped otherwise (never point it at the
// dev DB used by the dashboard).
func TestLLMKeyBlockRoundTrip(t *testing.T) {
	url := os.Getenv("AGENT_MEM_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AGENT_MEM_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	db := NewDB(pool)

	until := time.Now().Add(24 * time.Hour)
	if err := db.BlockLLMKey(ctx, "fp-quota", "aB3x", "google", "quota exhausted (429)", until); err != nil {
		t.Fatalf("block quota key: %v", err)
	}
	if err := db.BlockLLMKey(ctx, "fp-dead", "zZ9y", "google", "key rejected (403)", time.Time{}); err != nil {
		t.Fatalf("block rejected key: %v", err)
	}

	active, err := db.ActiveLLMKeyBlocks(ctx)
	if err != nil {
		t.Fatalf("active blocks: %v", err)
	}
	if len(active) != 2 {
		t.Errorf("active blocks = %v, want both fingerprints", active)
	}

	blocks, err := db.ListLLMKeyBlocks(ctx)
	if err != nil {
		t.Fatalf("list blocks: %v", err)
	}
	var permanent, temporary int
	for _, b := range blocks {
		if b.ExpiresAt == nil {
			permanent++
		} else {
			temporary++
		}
	}
	if permanent != 1 || temporary != 1 {
		t.Errorf("blocks = %d permanent / %d temporary, want 1/1", permanent, temporary)
	}

	if err := db.UnblockLLMKey(ctx, "fp-quota"); err != nil {
		t.Fatalf("unblock: %v", err)
	}
	active, _ = db.ActiveLLMKeyBlocks(ctx)
	if len(active) != 1 || active[0] != "fp-dead" {
		t.Errorf("active after unblock = %v, want [fp-dead]", active)
	}
	if err := db.UnblockLLMKey(ctx, "fp-dead"); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}
