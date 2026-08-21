package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-mem/agent-mem/internal/config"
	"github.com/agent-mem/agent-mem/internal/llmgateway"
)

// TestNewGatewayClientHonoursCap verifies that a client produced by
// newGatewayClient carries the cap from the snapshot and refuses generate
// calls once the ceiling is reached — with no HTTP request made for the
// refused call.
func TestNewGatewayClientHonoursCap(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		// Return a valid generate response.
		w.Write([]byte(`{"backend":"test","text":"ok"}`))
	}))
	t.Cleanup(srv.Close)

	snap := config.ConfigSnapshot{
		LLMGatewayURL:       srv.URL,
		LLMGatewayAPIKey:    "key",
		LLMHourlyCallCap:    1, // cap at 1
		GeminiEmbeddingDims: 768,
	}

	c := newGatewayClient(snap, 768)
	if c == nil {
		t.Fatal("newGatewayClient returned nil with a non-empty URL")
	}

	ctx := context.Background()

	// First call: should succeed and consume the one slot.
	_, err := c.Generate(ctx, "sys", "user")
	if err != nil {
		t.Fatalf("first Generate (within cap) failed: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 HTTP call after first Generate, got %d", calls)
	}

	// Second call: cap is exhausted, must refuse without an HTTP request.
	_, err = c.Generate(ctx, "sys", "user")
	if err == nil {
		t.Fatal("second Generate (over cap) should have been refused, got nil error")
	}
	if !strings.Contains(err.Error(), "hourly cap") {
		t.Errorf("refusal error does not mention 'hourly cap': %v", err)
	}
	if !llmgateway.IsRetryable(err) {
		t.Errorf("cap refusal must be retryable (transient), got: %v", err)
	}
	if calls != 1 {
		t.Errorf("HTTP call count = %d after cap refusal, want 1 (no new request)", calls)
	}
}
