package gemini

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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

	c := NewClient(ProviderOpenRouter, "test-key", "google/gemini-2.5-flash", "google/gemini-embedding-001", 768)
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

	c := NewClient(ProviderOpenRouter, "test-key", "m", "google/gemini-embedding-001", 768)
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

	c := NewClient(ProviderOpenRouter, "k", "m", "google/gemini-embedding-001", 768)
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

	c := NewClient(ProviderOpenRouter, "k", "m", "google/gemini-embedding-001", 768)
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

// TestEmbedBatchChunks verifies EmbedBatch splits inputs larger than
// maxEmbedBatch into multiple API calls (Google caps at 100/call) and returns
// one vector per input in order.
func TestEmbedBatchChunks(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body struct {
			Input []string `json:"input"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		if len(body.Input) > maxEmbedBatch {
			t.Errorf("chunk of %d exceeds maxEmbedBatch %d", len(body.Input), maxEmbedBatch)
		}
		data := make([]string, 0, len(body.Input))
		for i := range body.Input {
			data = append(data, `{"index":`+strconv.Itoa(i)+`,"embedding":[1.0]}`)
		}
		_, _ = w.Write([]byte(`{"data":[` + strings.Join(data, ",") + `]}`))
	}))
	defer srv.Close()

	c := NewClient(ProviderOpenRouter, "k", "m", "google/gemini-embedding-001", 768)
	c.baseURL = srv.URL

	texts := make([]string, 200)
	for i := range texts {
		texts[i] = "t"
	}
	vecs, err := c.EmbedBatch(context.Background(), texts)
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 200 {
		t.Fatalf("got %d vectors, want 200", len(vecs))
	}
	if want := 3; calls != want { // ceil(200/96) = 3
		t.Errorf("made %d API calls, want %d", calls, want)
	}
}

func TestGenerateGoogle(t *testing.T) {
	var gotBody map[string]any
	var gotPath, gotQueryKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQueryKey = r.URL.Query().Get("key")
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("google mode must NOT send Authorization header, got %q", got)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"{\"ok\":true}"}]}}]}`))
	}))
	defer srv.Close()

	c := NewClient(ProviderGoogle, "AIza-test", "google/gemini-2.5-flash", "google/gemini-embedding-001", 768)
	c.baseURL = srv.URL

	out, err := c.Generate(context.Background(), "sys", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if out != `{"ok":true}` {
		t.Errorf("Generate returned %q", out)
	}
	// Model id must be bare (google/ prefix stripped) in the REST path.
	if gotPath != "/gemini-2.5-flash:generateContent" {
		t.Errorf("path = %q, want /gemini-2.5-flash:generateContent (bare model id)", gotPath)
	}
	if gotQueryKey != "AIza-test" {
		t.Errorf("key query = %q, want AIza-test", gotQueryKey)
	}
	if _, ok := gotBody["contents"]; !ok {
		t.Error("google request must use contents[] (not OpenAI messages[])")
	}
}

func TestEmbedGoogleNoTaskType(t *testing.T) {
	var gotBody map[string]any
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = w.Write([]byte(`{"embedding":{"values":[0.1,0.2,0.3]}}`))
	}))
	defer srv.Close()

	c := NewClient(ProviderGoogle, "AIza-test", "m", "google/gemini-embedding-001", 768)
	c.baseURL = srv.URL

	vec, err := c.EmbedWithOptions(context.Background(), "hello", EmbedOptions{OutputDimensionality: 3072})
	if err != nil {
		t.Fatal(err)
	}
	if len(vec) != 3 {
		t.Fatalf("embedding len = %d, want 3", len(vec))
	}
	if gotPath != "/gemini-embedding-001:embedContent" {
		t.Errorf("path = %q, want /gemini-embedding-001:embedContent (bare model id)", gotPath)
	}
	// CRITICAL: no task_type — keeps vectors in the same space as what's stored.
	if _, ok := gotBody["taskType"]; ok {
		t.Error("google embed must NOT send taskType (vector-space compatibility)")
	}
	if gotBody["outputDimensionality"].(float64) != 3072 {
		t.Errorf("outputDimensionality = %v, want 3072", gotBody["outputDimensionality"])
	}
}

func TestModelIDNormalization(t *testing.T) {
	or := NewClient(ProviderOpenRouter, "k", "gemini-2.5-flash", "gemini-embedding-001", 768)
	if got := or.modelID("gemini-2.5-flash"); got != "google/gemini-2.5-flash" {
		t.Errorf("openrouter modelID(bare) = %q, want google/gemini-2.5-flash", got)
	}
	if got := or.modelID("google/gemini-2.5-flash"); got != "google/gemini-2.5-flash" {
		t.Errorf("openrouter modelID(prefixed) = %q, want unchanged", got)
	}
	// Non-Google OpenRouter namespaces pass through untouched (must NOT get google/).
	if got := or.modelID("anthropic/claude-haiku-4.5"); got != "anthropic/claude-haiku-4.5" {
		t.Errorf("openrouter modelID(anthropic) = %q, want anthropic/claude-haiku-4.5 (passthrough)", got)
	}
	if got := or.modelID("openai/gpt-5.6-luna"); got != "openai/gpt-5.6-luna" {
		t.Errorf("openrouter modelID(openai) = %q, want openai/gpt-5.6-luna (passthrough)", got)
	}
	g := NewClient(ProviderGoogle, "k", "google/gemini-2.5-flash", "google/gemini-embedding-001", 768)
	if got := g.modelID("google/gemini-2.5-flash"); got != "gemini-2.5-flash" {
		t.Errorf("google modelID(prefixed) = %q, want bare gemini-2.5-flash", got)
	}
}

func TestNewClientDefaultsProvider(t *testing.T) {
	if c := NewClient("", "k", "m", "e", 768); c.Provider() != ProviderOpenRouter {
		t.Errorf("empty provider = %q, want openrouter default", c.Provider())
	}
	if c := NewClient("bogus", "k", "m", "e", 768); c.Provider() != ProviderOpenRouter {
		t.Errorf("unknown provider = %q, want openrouter default", c.Provider())
	}
	if c := NewClient(ProviderGoogle, "k", "m", "e", 768); c.baseURL != googleBaseURL {
		t.Errorf("google baseURL = %q, want %q", c.baseURL, googleBaseURL)
	}
}
