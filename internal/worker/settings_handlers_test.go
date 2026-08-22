package worker

import (
	"context"
	"encoding/json"
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

func TestGetSettingsIncludesCapAndProcessingPaused(t *testing.T) {
	s := &Server{config: &config.Config{
		LLMHourlyCallCap: 73,
		ProcessingPaused: true,
	}}
	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	rec := httptest.NewRecorder()

	s.handleGetSettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/settings status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode settings response: %v", err)
	}
	if got, ok := body["llm_hourly_call_cap"]; !ok || got != float64(73) {
		t.Fatalf("llm_hourly_call_cap = %#v (present=%v), want 73", got, ok)
	}
	if got, ok := body["processing_paused"]; !ok || got != true {
		t.Fatalf("processing_paused = %#v (present=%v), want true", got, ok)
	}
}
