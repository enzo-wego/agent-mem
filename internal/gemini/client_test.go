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
	"time"
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
	var gotPath, gotAuthKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuthKey = r.Header.Get("x-goog-api-key")
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
	// Google auth rides the x-goog-api-key header, never the URL query.
	if gotAuthKey != "AIza-test" {
		t.Errorf("x-goog-api-key = %q, want AIza-test", gotAuthKey)
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

// --- key pool: rotation + block-and-failover ---

func TestPickSpreadsAcrossWindows(t *testing.T) {
	keys := []string{"a", "b", "c"}
	seen := map[string]int{}
	for w := int64(0); w < 60; w++ {
		seen[pick(keys, w)]++
	}
	for _, k := range keys {
		if seen[k] == 0 {
			t.Errorf("key %q never selected across 60 windows: %v", k, seen)
		}
	}
	// Same window must always resolve to the same key (all processes agree).
	if pick(keys, 42) != pick(keys, 42) {
		t.Error("pick is not deterministic for a given window")
	}
}

func TestRotationPinsSingleKeyAndZeroInterval(t *testing.T) {
	if got := NewClient(ProviderGoogle, "solo", "m", "e", 768).apiKey(); got != "solo" {
		t.Errorf("single key = %q, want solo", got)
	}
	c := NewRotatingClient(ProviderGoogle, []string{"first", "second"}, 0, "m", "e", 768)
	if got := c.apiKey(); got != "first" {
		t.Errorf("rotate=0 = %q, want first (pinned)", got)
	}
}

type fakeBlockStore struct {
	blocked map[string]string // fingerprint -> reason
}

func (f *fakeBlockStore) BlockLLMKey(_ context.Context, fingerprint, _, _, reason string, _ time.Time) error {
	f.blocked[fingerprint] = reason
	return nil
}

// A 429 on one key must block that key and retry the call on the next key with
// no backoff — the failover the key pool exists for.
func TestBlockedKeyFailsOverImmediately(t *testing.T) {
	var tried []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("x-goog-api-key")
		tried = append(tried, key)
		if key == "dead" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":429,"message":"quota exceeded"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"{\"ok\":true}"}]}}]}`))
	}))
	defer srv.Close()

	store := &fakeBlockStore{blocked: map[string]string{}}
	c := NewRotatingClient(ProviderGoogle, []string{"dead", "alive"}, 0, "gemini-2.5-flash", "e", 768).
		WithKeyBlocks(store, nil)
	c.baseURL = srv.URL

	start := time.Now()
	out, err := c.Generate(context.Background(), "", "hi")
	if err != nil {
		t.Fatalf("Generate after failover: %v", err)
	}
	if out != `{"ok":true}` {
		t.Errorf("Generate = %q, want the live key's response", out)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("failover took %v, want immediate (no backoff)", elapsed)
	}
	if len(tried) != 2 || tried[0] != "dead" || tried[1] != "alive" {
		t.Errorf("keys tried = %v, want [dead alive]", tried)
	}
	if reason := store.blocked[Fingerprint("dead")]; reason == "" {
		t.Errorf("dead key not persisted as blocked: %v", store.blocked)
	}
	if store.blocked[Fingerprint("alive")] != "" {
		t.Error("live key must not be blocked")
	}
	// The block sticks: the next call skips the dead key entirely.
	if got := c.apiKey(); got != "alive" {
		t.Errorf("apiKey after block = %q, want alive", got)
	}
}

func TestLiveKeysFallsBackWhenAllBlocked(t *testing.T) {
	c := NewRotatingClient(ProviderGoogle, []string{"a", "b"}, time.Hour, "m", "e", 768)
	c.blocked[Fingerprint("a")] = true
	c.blocked[Fingerprint("b")] = true
	if got := len(c.liveKeys()); got != 2 {
		t.Errorf("liveKeys with all blocked = %d keys, want 2 (retry beats hard failure)", got)
	}
}
