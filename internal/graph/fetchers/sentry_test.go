package fetchers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSentryFetcher_Matches(t *testing.T) {
	f := &sentryFetcher{}
	cases := []struct {
		input string
		want  bool
	}{
		{"sentry:4872610293", true},
		{"sentry:ISSUE-123_ABC", true},
		{"https://sentry.io/wego/payments/issues/4872610293/", true},
		{"jira:PAY-123", false},
		{"slack:C:1.2", false},
	}
	for _, tc := range cases {
		got := f.Matches(tc.input)
		if got != tc.want {
			t.Errorf("Matches(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestSentryFetcher_HappyPath(t *testing.T) {
	issue := sentryIssueResponse{
		ID:        "4872610293",
		Title:     "ZeroDivisionError at /checkout",
		LastSeen:  time.Now(),
		Actor:     &sentryActor{ID: "suser-1", Name: "Bob", Email: "bob@example.com"},
		Permalink: "https://sentry.io/wego/payments/issues/4872610293/",
	}

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(issue)
	}))
	defer srv.Close()

	cfg := Config{
		SentryAuthToken: "sn-secret",
		SentryBaseURL:   srv.URL,
		HTTPClient:      srv.Client(),
	}
	f := newSentryFetcher(cfg, noLogger())
	body, err := f.Fetch(context.Background(), "sentry:4872610293")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body.NodeID != "sentry:4872610293" {
		t.Errorf("nodeID = %q", body.NodeID)
	}
	if body.Title != "ZeroDivisionError at /checkout" {
		t.Errorf("title = %q", body.Title)
	}
	if body.Author.ExternalID != "suser-1" {
		t.Errorf("author = %q", body.Author.ExternalID)
	}
	if gotAuth != "Bearer sn-secret" {
		t.Errorf("auth = %q, want %q", gotAuth, "Bearer sn-secret")
	}
}

func TestSentryFetcher_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	cfg := Config{SentryAuthToken: "t", SentryBaseURL: srv.URL, HTTPClient: srv.Client()}
	f := newSentryFetcher(cfg, noLogger())
	_, err := f.Fetch(context.Background(), "sentry:4872610293")
	if err == nil {
		t.Fatal("expected error for 404")
	}
}

func TestSentryFetcher_5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := Config{SentryAuthToken: "t", SentryBaseURL: srv.URL, HTTPClient: srv.Client()}
	f := newSentryFetcher(cfg, noLogger())
	_, err := f.Fetch(context.Background(), "sentry:4872610293")
	if err == nil {
		t.Fatal("expected error for 5xx")
	}
}
