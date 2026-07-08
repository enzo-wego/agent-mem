package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

type captureRoundTripper struct {
	body []byte
}

func (rt *captureRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.body, _ = io.ReadAll(req.Body)
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewBufferString(`{"embedding":{"values":[0.1,0.2]}}`)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func TestEmbedWithOptionsUsesEmbedContentConfig(t *testing.T) {
	rt := &captureRoundTripper{}
	c := NewClient("key", "gemini", "gemini-embedding-001", 768)
	c.httpClient = &http.Client{Transport: rt}

	got, err := c.EmbedWithOptions(context.Background(), "same topic text", EmbedOptions{
		OutputDimensionality: 3072,
		TaskType:             "SEMANTIC_SIMILARITY",
	})
	if err != nil {
		t.Fatalf("EmbedWithOptions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("embedding len = %d, want 2", len(got))
	}

	var req map[string]any
	if err := json.Unmarshal(rt.body, &req); err != nil {
		t.Fatalf("request JSON: %v", err)
	}
	if _, ok := req["outputDimensionality"]; ok {
		t.Fatalf("outputDimensionality must be under embedContentConfig, got top-level request %s", rt.body)
	}
	cfg, ok := req["embedContentConfig"].(map[string]any)
	if !ok {
		t.Fatalf("missing embedContentConfig in request %s", rt.body)
	}
	if got := cfg["taskType"]; got != "SEMANTIC_SIMILARITY" {
		t.Fatalf("taskType = %v, want SEMANTIC_SIMILARITY", got)
	}
	if got := cfg["outputDimensionality"]; got != float64(3072) {
		t.Fatalf("outputDimensionality = %v, want 3072", got)
	}
}
