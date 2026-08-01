package llmgateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/gemini"
	"github.com/agent-mem/agent-mem/internal/graph/handlers"
)

// The client must satisfy the whole surface it replaces. A compile-time
// assertion is cheaper than discovering a missing method at wiring time.
var _ handlers.GeminiClient = (*Client)(nil)

// capture records what the gateway received, so tests can assert on the wire
// format rather than on our own request structs.
type capture struct {
	path string
	body map[string]any
}

func serve(t *testing.T, status int, respBody string) (*httptest.Server, *capture) {
	t.Helper()
	got := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got.body)
		if r.Header.Get("X-API-Key") != "secret" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"detail":"unauthorized"}`))
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

// Tier is the cost control: "summary" buys Sonnet, "cheap" buys Haiku. Sending
// the wrong one is a silent bill, not an error — so pin both.
func TestGenerateTiers(t *testing.T) {
	for _, tc := range []struct {
		name, wantTier string
		call           func(*Client) (string, error)
	}{
		{"Generate uses summary", "summary", func(c *Client) (string, error) {
			return c.Generate(context.Background(), "sys", "usr")
		}},
		{"GenerateCheap uses cheap", "cheap", func(c *Client) (string, error) {
			return c.GenerateCheap(context.Background(), "sys", "usr")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, got := serve(t, 200, `{"backend":"claude","text":"hello"}`)
			out, err := tc.call(New(srv.URL, "secret", 3072))
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if out != "hello" {
				t.Errorf("text = %q", out)
			}
			if got.path != "/generate" {
				t.Errorf("path = %q", got.path)
			}
			if got.body["tier"] != tc.wantTier {
				t.Errorf("tier = %v, want %v", got.body["tier"], tc.wantTier)
			}
			if got.body["system"] != "sys" || got.body["user"] != "usr" {
				t.Errorf("prompts not carried: %#v", got.body)
			}
		})
	}
}

func TestClientBypassesAmbientProxy(t *testing.T) {
	var proxyHits int
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyHits++
		http.Error(w, "gateway traffic must not reach the ambient proxy", http.StatusBadGateway)
	}))
	defer proxy.Close()

	gatewayHits := 0
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gatewayHits++
		if r.URL.Path != "/generate" {
			t.Errorf("path = %q, want /generate", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"backend":"claude","text":"direct"}`))
	}))
	defer gateway.Close()

	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")

	c := New(gateway.URL, "secret", 3072)
	transport, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("gateway client transport = %T, want *http.Transport", c.http.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("gateway client inherits ambient proxy configuration")
	}

	got, err := c.Generate(context.Background(), "system", "user")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got != "direct" {
		t.Fatalf("Generate = %q, want direct", got)
	}
	if gatewayHits != 1 {
		t.Fatalf("gateway hits = %d, want 1", gatewayHits)
	}
	if proxyHits != 0 {
		t.Fatalf("proxy hits = %d, want 0", proxyHits)
	}
}

func TestEmbedSendsDimsAndValidatesWidth(t *testing.T) {
	// A 768-dim client must ask for 768 — observations.embedding is vector(768)
	// and a 3072 vector fails the insert far from the cause.
	srv, got := serve(t, 200, `{"embeddings":[[0.1,0.2,0.3]],"dims":3}`)
	c := New(srv.URL, "secret", 3)
	v, err := c.Embed(context.Background(), "text")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(v) != 3 {
		t.Fatalf("len = %d", len(v))
	}
	if got.path != "/embed" {
		t.Errorf("path = %q", got.path)
	}
	if got.body["dims"] != float64(3) {
		t.Errorf("dims = %v, want 3", got.body["dims"])
	}
	if texts, _ := got.body["texts"].([]any); len(texts) != 1 || texts[0] != "text" {
		t.Errorf("texts = %v", got.body["texts"])
	}
}

func TestEmbedWithOptionsOverridesDims(t *testing.T) {
	srv, got := serve(t, 200, `{"embeddings":[[1,2]],"dims":2}`)
	c := New(srv.URL, "secret", 3072)
	if _, err := c.EmbedWithOptions(context.Background(), "t", gemini.EmbedOptions{OutputDimensionality: 2}); err != nil {
		t.Fatalf("EmbedWithOptions: %v", err)
	}
	if got.body["dims"] != float64(2) {
		t.Errorf("dims = %v, want the override 2", got.body["dims"])
	}
	// task_type must never be sent: stored vectors were produced without one, and
	// adding it moves queries into a different vector space — search silently
	// degrades instead of failing.
	if _, ok := got.body["task_type"]; ok {
		t.Error("task_type was sent; queries would no longer match the stored corpus")
	}
}

// A width mismatch must be caught here, not by pgvector. The DB error surfaces
// far from the cause and reads like a schema problem.
func TestEmbedRejectsWrongWidth(t *testing.T) {
	srv, _ := serve(t, 200, `{"embeddings":[[0.1,0.2]],"dims":768}`)
	_, err := New(srv.URL, "secret", 768).Embed(context.Background(), "t")
	if err == nil || !strings.Contains(err.Error(), "want 768") {
		t.Fatalf("want a dimension error, got %v", err)
	}
}

func TestDescribeParsesAndBase64s(t *testing.T) {
	inner := `{"description":"a chart","ocr":"Q1 revenue","entities":["Q1"]}`
	payload, _ := json.Marshal(map[string]string{"backend": "claude", "text": inner})
	srv, got := serve(t, 200, string(payload))

	desc, ocr, ents, err := New(srv.URL, "secret", 3072).
		Describe(context.Background(), "image/png", []byte{0x89, 0x50}, "Describe it")
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if desc != "a chart" || ocr != "Q1 revenue" || len(ents) != 1 || ents[0] != "Q1" {
		t.Errorf("parsed = %q / %q / %v", desc, ocr, ents)
	}
	if got.path != "/describe" {
		t.Errorf("path = %q", got.path)
	}
	if got.body["mime"] != "image/png" {
		t.Errorf("mime = %v", got.body["mime"])
	}
	if want := base64.StdEncoding.EncodeToString([]byte{0x89, 0x50}); got.body["data_b64"] != want {
		t.Errorf("data_b64 = %v, want %v", got.body["data_b64"], want)
	}
	// The JSON instruction must be appended, or the model returns prose and the
	// parse fails on every attachment.
	if p, _ := got.body["prompt"].(string); !strings.Contains(p, "Respond as JSON only") {
		t.Errorf("prompt missing the JSON instruction: %q", p)
	}
}

// Every non-200 and every unusable body must surface as an error. Callers cache
// "" as a real answer, so a silent empty return poisons the cache.
func TestFailuresAreErrorsNotEmptyStrings(t *testing.T) {
	for _, tc := range []struct {
		name, body  string
		status      int
		wantErrPart string
	}{
		{"seat spent", `{"detail":"seat quota exhausted, resets_at 1785534600"}`, 503, "503"},
		{"upstream broke", `{"detail":"openrouter 502"}`, 502, "502"},
		{"malformed json", `not json`, 200, "decode"},
		{"200 but blank", `{"backend":"claude","text":"   "}`, 200, "no text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := serve(t, tc.status, tc.body)
			got, err := New(srv.URL, "secret", 3072).Generate(context.Background(), "s", "u")
			if err == nil {
				t.Fatalf("want error, got %q — a blank would be cached as a real summary", got)
			}
			if got != "" {
				t.Errorf("text should be empty on error, got %q", got)
			}
			if !strings.Contains(err.Error(), tc.wantErrPart) {
				t.Errorf("error %q not diagnosable, want mention of %q", err, tc.wantErrPart)
			}
		})
	}
}

// The seat reset time is the one detail that makes a 503 actionable.
func TestQuotaErrorKeepsResetTime(t *testing.T) {
	srv, _ := serve(t, 503, `{"detail":"seat quota exhausted (resets_at=1785534600)"}`)
	_, err := New(srv.URL, "secret", 3072).Generate(context.Background(), "s", "u")
	if err == nil || !strings.Contains(err.Error(), "1785534600") {
		t.Errorf("reset time lost: %v", err)
	}
}

// The distinction that keeps flat memory from losing data: a gateway that is
// DOWN is retryable, a gateway that ANSWERS an error is not. Only the former
// may be wrapped in ErrUnreachable, or a permanent 400 would requeue forever.
func TestUnreachableIsDistinguishedFromAnswered(t *testing.T) {
	// Nothing listening: closed immediately so the port is dead.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := dead.URL
	dead.Close()

	_, err := New(url, "secret", 3072).Generate(context.Background(), "s", "u")
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("a dead gateway must be ErrUnreachable so work requeues, got %v", err)
	}

	srv, _ := serve(t, 400, `{"detail":"bad request"}`)
	_, err = New(srv.URL, "secret", 3072).Generate(context.Background(), "s", "u")
	if errors.Is(err, ErrUnreachable) {
		t.Error("an answered 400 must NOT be ErrUnreachable; it would requeue forever")
	}
}

func TestMissingAuthSurfaces(t *testing.T) {
	srv, _ := serve(t, 200, `{"text":"never reached"}`)
	_, err := New(srv.URL, "wrong-key", 3072).Generate(context.Background(), "s", "u")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("bad auth must surface, got %v", err)
	}
}

func TestContextCancellationIsHonored(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := New(srv.URL, "secret", 3072).Generate(ctx, "s", "u"); err == nil {
		t.Error("a hung gateway must not hold a job lease")
	}
}

// gateway 180s < client 200s < lease 240s. Editing one must trip on the others.
func TestTimeoutOrderingHolds(t *testing.T) {
	const gatewayClaudeTimeout = 180 * time.Second
	if RequestTimeout <= gatewayClaudeTimeout {
		t.Errorf("RequestTimeout %v must exceed the gateway's %v", RequestTimeout, gatewayClaudeTimeout)
	}
	if handlers.SummaryLease <= RequestTimeout {
		t.Errorf("SummaryLease %v must exceed RequestTimeout %v, else leases expire mid-call",
			handlers.SummaryLease, RequestTimeout)
	}
}

func TestBaseURLTrailingSlashAndDimsDefault(t *testing.T) {
	if got := New("http://x:8750/", "k", 768).baseURL; got != "http://x:8750" {
		t.Errorf("baseURL = %q, want no trailing slash", got)
	}
	if got := New("http://x", "k", 0).dims; got != 3072 {
		t.Errorf("dims default = %d, want 3072", got)
	}
}
