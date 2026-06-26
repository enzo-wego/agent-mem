package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGenerateReassemblesJSON verifies the "{" prefill is re-attached and any
// trailing text past the final "}" is trimmed, so callers get a clean JSON object.
func TestGenerateReassemblesJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") == "" || r.Header.Get("anthropic-version") == "" {
			t.Errorf("missing required headers")
		}
		// Model echoes the body Claude would return AFTER the prefilled "{".
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"\"overview\":\"hi\"}\n\nextra"}]}`))
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
