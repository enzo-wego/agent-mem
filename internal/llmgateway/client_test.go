package llmgateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/handlers"
)

// The client must satisfy the seam it exists for. A compile-time assertion is
// cheaper than discovering the mismatch at wiring time.
var _ handlers.TextGenerator = (*Client)(nil)

func TestGenerateSendsTierAndAuth(t *testing.T) {
	var gotPath, gotKey, gotCT string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotKey, gotCT = r.URL.Path, r.Header.Get("X-API-Key"), r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"backend":"claude","text":"a summary"}`))
	}))
	defer srv.Close()

	got, err := New(srv.URL, "secret", "summary").Generate(context.Background(), "sys prompt", "user msg")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got != "a summary" {
		t.Errorf("text = %q, want %q", got, "a summary")
	}
	if gotPath != "/generate" {
		t.Errorf("path = %q, want /generate", gotPath)
	}
	// Without the header the gateway 401s and every summary silently stops.
	if gotKey != "secret" {
		t.Errorf("X-API-Key = %q, want %q", gotKey, "secret")
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	// The tier is the cost control: "summary" buys Sonnet, "cheap" buys Flash.
	// Sending the wrong one is a silent bill, not an error.
	if gotBody["tier"] != "summary" {
		t.Errorf("tier = %v, want summary", gotBody["tier"])
	}
	if gotBody["system"] != "sys prompt" || gotBody["user"] != "user msg" {
		t.Errorf("body did not carry the prompts: %#v", gotBody)
	}
}

// An unknown tier must degrade to the cheap model. Coercing upward would let a
// typo quietly buy the expensive one on every call.
func TestUnknownTierDegradesToCheap(t *testing.T) {
	for _, in := range []string{"", "Summary", "premium", "sonnet"} {
		if got := New("http://x", "k", in).tier; got != "cheap" {
			t.Errorf("tier %q → %q, want cheap", in, got)
		}
	}
	if got := New("http://x", "k", "summary").tier; got != "summary" {
		t.Errorf("valid tier was not preserved: %q", got)
	}
}

// Every non-200 and every empty body must surface as an error. Callers cache ""
// as "the LLM had nothing to say", so a silent empty return would poison the
// summary cache with blanks that never regenerate.
func TestFailuresAreErrorsNotEmptyStrings(t *testing.T) {
	for _, tc := range []struct {
		name, body  string
		status      int
		wantErrPart string
	}{
		{"seat spent", `{"detail":"seat quota exhausted, resets_at 1785480000"}`, 503, "503"},
		{"bad auth", `{"detail":"unauthorized"}`, 401, "401"},
		{"upstream broke", `{"detail":"openrouter 502"}`, 502, "502"},
		{"malformed json", `not json at all`, 200, "decode response"},
		{"200 but no text", `{"backend":"claude","text":"   "}`, 200, "no text"},
		{"200 structured only", `{"backend":"claude","output":{"a":1}}`, 200, "no text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			got, err := New(srv.URL, "k", "summary").Generate(context.Background(), "s", "u")
			if err == nil {
				t.Fatalf("want an error, got text %q — a blank would be cached as a real summary", got)
			}
			if got != "" {
				t.Errorf("text should be empty on error, got %q", got)
			}
			if !strings.Contains(err.Error(), tc.wantErrPart) {
				t.Errorf("error %q does not mention %q, so the failure is not diagnosable", err, tc.wantErrPart)
			}
		})
	}
}

// The 503 body carries the seat reset time — the one detail that makes the
// failure actionable. It must survive into the error.
func TestQuotaErrorKeepsResetTime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"detail":"seat quota exhausted (resets_at=1785480000)"}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "k", "summary").Generate(context.Background(), "s", "u")
	if err == nil || !strings.Contains(err.Error(), "1785480000") {
		t.Errorf("reset time lost from error: %v", err)
	}
}

func TestContextCancellationIsHonored(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		_, _ = w.Write([]byte(`{"text":"too late"}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := New(srv.URL, "k", "summary").Generate(ctx, "s", "u"); err == nil {
		t.Error("want an error when ctx expires; a hung gateway must not hold a job lease")
	}
}

// The timeout chain is load-bearing: gateway 180s < client 200s < lease 240s. A
// client timeout below the gateway's would cut off calls it would have answered;
// above the lease, the janitor reclaims the job mid-call and a second worker
// redoes the same summary — the duplicate-LLM-call shape this repo already paid
// for once.
func TestTimeoutOrderingHolds(t *testing.T) {
	const gatewayClaudeTimeout = 180 * time.Second
	if RequestTimeout <= gatewayClaudeTimeout {
		t.Errorf("RequestTimeout %v must exceed the gateway's %v", RequestTimeout, gatewayClaudeTimeout)
	}
	if handlers.SummaryLease <= RequestTimeout {
		t.Errorf("SummaryLease %v must exceed RequestTimeout %v, or leases expire mid-call",
			handlers.SummaryLease, RequestTimeout)
	}
}

// A trailing slash in the configured URL must not produce "//generate".
func TestBaseURLTrailingSlash(t *testing.T) {
	if got := New("http://x:8750/", "k", "cheap").baseURL; got != "http://x:8750" {
		t.Errorf("baseURL = %q, want no trailing slash", got)
	}
}
