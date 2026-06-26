package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGenerateExtractsJSON verifies the JSON object is extracted from a response
// wrapped in markdown fences / prose, so callers get clean JSON.
func TestGenerateExtractsJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") == "" || r.Header.Get("anthropic-version") == "" {
			t.Errorf("missing required headers")
		}
		// Claude wraps the object in a ```json fence with trailing prose.
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"` + "```json\\n{\\\"overview\\\":\\\"hi\\\"}\\n```\\nDone." + `"}]}`))
	}))
	defer srv.Close()

	c := NewClient("k", "")
	c.baseURL = srv.URL

	out, err := c.Generate(context.Background(), "sys", "user")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var parsed struct {
		Overview string `json:"overview"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %q (%v)", out, err)
	}
	if parsed.Overview != "hi" {
		t.Fatalf("overview = %q, want hi", parsed.Overview)
	}
}
