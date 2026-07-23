package graphmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type observedRequest struct {
	method      string
	escapedPath string
	query       string
	auth        string
	contentType string
	body        map[string]any
}

func TestClient_ProxiesGraphEndpoints(t *testing.T) {
	var (
		mu       sync.Mutex
		observed []observedRequest
	)
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		item := observedRequest{
			method:      r.Method,
			escapedPath: r.URL.EscapedPath(),
			query:       r.URL.RawQuery,
			auth:        r.Header.Get("Authorization"),
			contentType: r.Header.Get("Content-Type"),
		}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&item.body)
		}
		mu.Lock()
		observed = append(observed, item)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer worker.Close()

	client, err := NewClient(worker.URL, "worker-secret", worker.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	calls := []func() (map[string]any, error){
		func() (map[string]any, error) {
			return client.Search(ctx, "TRY currency", []string{"slack_thread", "jira"}, 5)
		},
		func() (map[string]any, error) {
			return client.Node(ctx, "jira:PAY-2223", "")
		},
		func() (map[string]any, error) {
			return client.Node(ctx, "", "https://example.com/path?a=1&b=2")
		},
		func() (map[string]any, error) {
			return client.Neighbors(ctx, "gh_pr:wego/payments#2198", 2, []string{"REFERENCES", "SAME_TOPIC"})
		},
		func() (map[string]any, error) {
			return client.ClusterSummary(ctx, "jira:PAY-2223", 3)
		},
		func() (map[string]any, error) {
			return client.Resolve(ctx, ResolveRequest{
				Seeds:         []string{"https://github.com/wego/payments/pull/2198"},
				Query:         "is WithRebateRepo safe to remove?",
				Depth:         2,
				BudgetTokens:  4000,
				IncludeBodies: true,
			})
		},
		func() (map[string]any, error) {
			err := client.Probe(ctx)
			return map[string]any{"ok": err == nil}, err
		},
	}
	for i, call := range calls {
		got, err := call()
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if got["ok"] != true {
			t.Fatalf("call %d returned %#v", i, got)
		}
	}

	expected := []observedRequest{
		{method: http.MethodGet, escapedPath: "/api/graph/search", query: "limit=5&q=TRY+currency&types=slack_thread%2Cjira"},
		{method: http.MethodGet, escapedPath: "/api/graph/node", query: "id=jira%3APAY-2223"},
		{method: http.MethodGet, escapedPath: "/api/graph/node", query: "url=https%3A%2F%2Fexample.com%2Fpath%3Fa%3D1%26b%3D2"},
		{method: http.MethodGet, escapedPath: "/api/graph/node/gh_pr:wego%2Fpayments%232198/neighbors", query: "depth=2&kind=REFERENCES&kind=SAME_TOPIC"},
		{method: http.MethodGet, escapedPath: "/api/graph/cluster/summary", query: "depth=3&node=jira%3APAY-2223"},
		{method: http.MethodPost, escapedPath: "/api/graph/resolve", contentType: "application/json"},
		{method: http.MethodGet, escapedPath: "/api/settings"},
	}
	if len(observed) != len(expected) {
		t.Fatalf("observed %d requests, want %d: %#v", len(observed), len(expected), observed)
	}
	for i, want := range expected {
		got := observed[i]
		if got.method != want.method || got.escapedPath != want.escapedPath || got.query != want.query {
			t.Errorf("request %d = %s %s?%s, want %s %s?%s",
				i, got.method, got.escapedPath, got.query, want.method, want.escapedPath, want.query)
		}
		if got.auth != "Bearer worker-secret" {
			t.Errorf("request %d Authorization = %q", i, got.auth)
		}
		if want.contentType != "" && got.contentType != want.contentType {
			t.Errorf("request %d Content-Type = %q, want %q", i, got.contentType, want.contentType)
		}
	}
	resolveBody := observed[5].body
	if resolveBody["query"] != "is WithRebateRepo safe to remove?" ||
		resolveBody["depth"] != float64(2) ||
		resolveBody["budget_tokens"] != float64(4000) ||
		resolveBody["include_bodies"] != true {
		t.Fatalf("resolve body = %#v", resolveBody)
	}
}

func TestClient_WorkerAndJSONErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
		code int
		want string
	}{
		{name: "worker status", code: http.StatusUnauthorized, body: `{"error":"unauthorized"}`, want: "401"},
		{name: "invalid JSON", code: http.StatusOK, body: `not-json`, want: "decode worker response"},
		{name: "oversized", code: http.StatusOK, body: strings.Repeat("x", maxResponseBytes+1), want: "exceeds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.code)
				fmt.Fprint(w, tt.body)
			}))
			defer worker.Close()
			client, err := NewClient(worker.URL, "secret", worker.Client())
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Node(context.Background(), "jira:PAY-1", "")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestClient_CancellationReachesWorker(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	worker := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer worker.Close()

	client, err := NewClient(worker.URL, "secret", worker.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := client.Search(ctx, "cancel me", nil, 10)
		errCh <- err
	}()
	<-started
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("client did not return after cancellation")
	}
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not observe cancellation")
	}
}

func TestNewClient_RejectsInvalidBaseURL(t *testing.T) {
	for _, raw := range []string{"", "://bad", "/relative"} {
		t.Run(url.PathEscape(raw), func(t *testing.T) {
			if _, err := NewClient(raw, "secret", http.DefaultClient); err == nil {
				t.Fatalf("NewClient(%q) succeeded", raw)
			}
		})
	}
}
