package fetchers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSlackFetcher_Matches(t *testing.T) {
	f := &slackFetcher{}
	cases := []struct {
		input string
		want  bool
	}{
		{"slack:C08S954G2LX:1779710863.216389", true},
		{"https://wego.slack.com/archives/C08S954G2LX/p1779710863216389", true},
		{"jira:PAY-123", false},
		{"gh_pr:wego/payments#1", false},
		{"https://app.datadoghq.com/monitors/123", false},
		{"slack:INVALID", false},
	}
	for _, tc := range cases {
		got := f.Matches(tc.input)
		if got != tc.want {
			t.Errorf("Matches(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestSlackFetcher_HappyPath_Thread(t *testing.T) {
	payload := slackAPIResponse{
		OK: true,
		Messages: []slackMessage{
			{User: "U123", Text: "Hello thread", Ts: "1779710863.216389"},
			{User: "U456", Text: "Reply here", Ts: "1779710864.000001"},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Expect bearer auth.
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(payload)
	}))
	defer srv.Close()

	cfg := Config{
		SlackBotToken: "test-token",
		HTTPClient:    srv.Client(),
	}
	// Point API calls to test server by overriding the URL in the fetcher's doGet.
	// We do this by using a transport that rewrites the host.
	cfg.HTTPClient = newRewriteClient(srv.URL, srv.Client())

	f := newSlackFetcher(cfg, noLogger())
	body, err := f.Fetch(context.Background(), "slack:C08S954G2LX:1779710863.216389")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body.Author.ExternalID != "U123" {
		t.Errorf("author = %q, want U123", body.Author.ExternalID)
	}
	if body.NodeID != "slack:C08S954G2LX:1779710863.216389" {
		t.Errorf("nodeID = %q", body.NodeID)
	}
	if body.Type != "slack" {
		t.Errorf("type = %q", body.Type)
	}
	// Raw should contain parent text + reply separator.
	raw := string(body.Raw)
	if raw == "" {
		t.Error("raw body is empty")
	}
	if body.Title == "" {
		t.Error("title is empty")
	}
}

func TestSlackFetcher_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	cfg := Config{
		SlackBotToken: "test-token",
		HTTPClient:    newRewriteClient(srv.URL, srv.Client()),
	}
	f := newSlackFetcher(cfg, noLogger())
	_, err := f.Fetch(context.Background(), "slack:C08S954G2LX:1779710863.216389")
	if err == nil {
		t.Fatal("expected error for 404, got nil")
	}
}

func TestSlackFetcher_5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := Config{
		SlackBotToken: "test-token",
		HTTPClient:    newRewriteClient(srv.URL, srv.Client()),
	}
	f := newSlackFetcher(cfg, noLogger())
	_, err := f.Fetch(context.Background(), "slack:C08S954G2LX:1779710863.216389")
	if err == nil {
		t.Fatal("expected error for 5xx, got nil")
	}
}

func TestSlackFetcher_AuthHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(slackAPIResponse{
			OK:       true,
			Messages: []slackMessage{{User: "U1", Text: "hi", Ts: "1779710863.000001"}},
		})
	}))
	defer srv.Close()

	cfg := Config{
		SlackBotToken: "xoxb-secret",
		HTTPClient:    newRewriteClient(srv.URL, srv.Client()),
	}
	f := newSlackFetcher(cfg, noLogger())
	f.Fetch(context.Background(), "slack:C08S:1779710863.000001") //nolint:errcheck
	if gotAuth != "Bearer xoxb-secret" {
		t.Errorf("auth header = %q, want %q", gotAuth, "Bearer xoxb-secret")
	}
}
