package gemini

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGenerateOpenRouter(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("auth header = %q, want Bearer test-key", got)
		}
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"ok\":true}"}}]}`))
	}))
	defer srv.Close()

	c := NewClient("test-key", "google/gemini-2.5-flash", "google/gemini-embedding-001", 768)
	c.baseURL = srv.URL

	out, err := c.Generate(context.Background(), "sys", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if out != `{"ok":true}` {
		t.Errorf("Generate returned %q", out)
	}
	if gotBody["model"] != "google/gemini-2.5-flash" {
		t.Errorf("model = %v", gotBody["model"])
	}
	if _, ok := gotBody["messages"]; !ok {
		t.Error("request must use OpenAI messages[]")
	}
}

func TestEmbedOpenRouter(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("path = %q, want /embeddings", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3]}]}`))
	}))
	defer srv.Close()

	c := NewClient("test-key", "m", "google/gemini-embedding-001", 768)
	c.baseURL = srv.URL

	vec, err := c.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if len(vec) != 3 {
		t.Fatalf("embedding len = %d, want 3", len(vec))
	}
	if gotBody["dimensions"].(float64) != 768 {
		t.Errorf("dimensions = %v, want 768", gotBody["dimensions"])
	}
	if _, ok := gotBody["task_type"]; ok {
		t.Error("task_type must NOT be sent to OpenRouter")
	}
	if gotBody["model"] != "google/gemini-embedding-001" {
		t.Errorf("model = %v", gotBody["model"])
	}
}

func TestEmbedWithOptionsDims(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1]}]}`))
	}))
	defer srv.Close()

	c := NewClient("k", "m", "google/gemini-embedding-001", 768)
	c.baseURL = srv.URL

	_, err := c.EmbedWithOptions(context.Background(), "x", EmbedOptions{OutputDimensionality: 3072})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["dimensions"].(float64) != 3072 {
		t.Errorf("dimensions = %v, want 3072 (per-call override)", gotBody["dimensions"])
	}
}

func TestEmbedBatchOrdering(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("path = %q, want /embeddings", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		// Return out of order: index 1 first, then index 0.
		_, _ = w.Write([]byte(`{"data":[{"index":1,"embedding":[2.0]},{"index":0,"embedding":[1.0]}]}`))
	}))
	defer srv.Close()

	c := NewClient("k", "m", "google/gemini-embedding-001", 768)
	c.baseURL = srv.URL

	vecs, err := c.EmbedBatch(context.Background(), []string{"first", "second"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 2 {
		t.Fatalf("got %d vectors, want 2", len(vecs))
	}
	if vecs[0][0] != 1.0 || vecs[1][0] != 2.0 {
		t.Errorf("embeddings bound to wrong inputs: got %v (ordering not honored)", vecs)
	}
	if _, ok := gotBody["input"].([]any); !ok {
		t.Errorf("input must be sent as a JSON array, got %T", gotBody["input"])
	}
}
