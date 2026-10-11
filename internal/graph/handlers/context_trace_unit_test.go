package handlers

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	graphtrace "github.com/agent-mem/agent-mem/internal/graph/trace"
)

type captureTraceStore struct {
	mu      sync.Mutex
	records []graphtrace.Record
}

func (s *captureTraceStore) Write(_ context.Context, record graphtrace.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, record)
	return nil
}
func (*captureTraceStore) Prune(context.Context, time.Time) (int64, error) { return 0, nil }
func (s *captureTraceStore) one(t *testing.T) graphtrace.Record {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.records) != 1 {
		t.Fatalf("records=%d want=1", len(s.records))
	}
	return s.records[0]
}

func TestContextTraceAddsRequestIDWithoutChangingJSONBytes(t *testing.T) {
	const original = "{\"z\":1,\"integer\":9007199254740993}\n"
	store := &captureTraceStore{}
	recorder := graphtrace.NewRecorder(store, graphtrace.Options{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, original)
	})
	w := httptest.NewRecorder()
	withContextTrace(handler, "search", "test", recorder).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/?q=x", nil))
	recorder.Close(time.Second)
	id := w.Header().Get("X-Graph-Request-Id")
	want := "{\"request_id\":\"" + id + "\",\"z\":1,\"integer\":9007199254740993}\n"
	if w.Body.String() != want {
		t.Fatalf("body changed\ngot  %q\nwant %q", w.Body.String(), want)
	}
}

func TestTraceCopyLimitsBodyWithoutTruncatingHandlerInput(t *testing.T) {
	body := bytes.Repeat([]byte("x"), (1<<20)+73)
	store := &captureTraceStore{}
	recorder := graphtrace.NewRecorder(store, graphtrace.Options{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := len(requestBody(r)); got != 1<<20 {
			t.Errorf("trace copy bytes=%d want=%d", got, 1<<20)
		}
		got, _ := io.ReadAll(r.Body)
		if !bytes.Equal(got, body) {
			t.Errorf("handler body bytes=%d want=%d", len(got), len(body))
		}
		http.Error(w, "bad", http.StatusBadRequest)
	})
	withContextTrace(handler, "resolve", "test", recorder).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body)))
	recorder.Close(time.Second)
	_ = store.one(t)
}

func TestCapStringUsesRunesAndStripsNUL(t *testing.T) {
	if got := capString("a\x00界b", 2); got != "a界" {
		t.Fatalf("capString=%q", got)
	}
}

func TestTraceRequestCapsStringsArraysAndUsesCanonicalSeeds(t *testing.T) {
	long := strings.Repeat("界", 2001)
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	diag := &requestTraceDiag{canonicalSeeds: []string{"slack:root"}}
	ctx := context.WithValue(contextWithTraceBody(r, []byte(`{"seeds":["raw"],"query":"`+long+`","epic":["`+strings.Join(make([]string, 60), `","`)+`"]}`)), requestTraceDiagKey{}, diag)
	r = r.WithContext(ctx)
	got := traceRequest(r, "resolve").(map[string]any)
	if len([]rune(got["query"].(string))) != 2000 {
		t.Fatalf("query runes=%d", len([]rune(got["query"].(string))))
	}
	if seeds := got["canonical_seeds"].([]string); len(seeds) != 1 || seeds[0] != "slack:root" {
		t.Fatalf("canonical_seeds=%v", seeds)
	}
	if epic := got["epic"].([]any); len(epic) != 50 {
		t.Fatalf("epic entries=%d want=50", len(epic))
	}
}

func TestTraceRequestSanitizesNestedStringsAndKeys(t *testing.T) {
	long := strings.Repeat("界", 2500)
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	body := `{"seeds":["` + long + `"],"bad\u0000key":"a\u0000b"}`
	r = r.WithContext(contextWithTraceBody(r, []byte(body)))
	got := traceRequest(r, "resolve").(map[string]any)
	if seed := got["seeds"].([]any)[0].(string); len([]rune(seed)) != 2000 {
		t.Fatalf("seed runes=%d want=2000", len([]rune(seed)))
	}
	if got["badkey"] != "ab" {
		t.Fatalf("NUL not stripped from key/value: %v", got)
	}
}
