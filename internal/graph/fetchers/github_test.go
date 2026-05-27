package fetchers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGitHubFetcher_Matches(t *testing.T) {
	f := &gitHubFetcher{}
	cases := []struct {
		input string
		want  bool
	}{
		{"gh_pr:wego/payments#1960", true},
		{"https://github.com/wego/payments/pull/1960", true},
		{"jira:PAY-123", false},
		{"slack:C:1.2", false},
		{"gh_pr:invalid", false},
	}
	for _, tc := range cases {
		got := f.Matches(tc.input)
		if got != tc.want {
			t.Errorf("Matches(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestGitHubFetcher_HappyPath(t *testing.T) {
	pr := ghPR{
		Number:    1960,
		Title:     "Fix payment flow",
		Body:      "## Summary\n\nFixed the issue.",
		HTMLURL:   "https://github.com/wego/payments/pull/1960",
		UpdatedAt: time.Now(),
		User:      ghUser{Login: "alice"},
	}
	comments := []ghComment{
		{Body: "LGTM!", CreatedAt: time.Now(), User: ghUser{Login: "bob"}},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/wego/payments/pulls/1960", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "no auth", http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(pr)
	})
	mux.HandleFunc("/repos/wego/payments/pulls/1960/comments", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(comments)
	})
	mux.HandleFunc("/repos/wego/payments/issues/1960/comments", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]ghComment{})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := Config{
		GHToken:   "gh-secret",
		GHBaseURL: srv.URL,
		HTTPClient: srv.Client(),
	}
	f := newGitHubFetcher(cfg, noLogger())
	body, err := f.Fetch(context.Background(), "gh_pr:wego/payments#1960")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body.NodeID != "gh_pr:wego/payments#1960" {
		t.Errorf("nodeID = %q", body.NodeID)
	}
	if body.Title != "Fix payment flow" {
		t.Errorf("title = %q", body.Title)
	}
	if body.Author.ExternalID != "alice" {
		t.Errorf("author = %q", body.Author.ExternalID)
	}
	raw := string(body.Raw)
	if raw == "" {
		t.Error("raw body is empty")
	}
}

func TestGitHubFetcher_AuthHeader(t *testing.T) {
	var gotAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/wego/repo/pulls/1", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(ghPR{Title: "t", User: ghUser{Login: "u"}})
	})
	mux.HandleFunc("/repos/wego/repo/pulls/1/comments", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]ghComment{})
	})
	mux.HandleFunc("/repos/wego/repo/issues/1/comments", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]ghComment{})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg := Config{GHToken: "gh-tok", GHBaseURL: srv.URL, HTTPClient: srv.Client()}
	f := newGitHubFetcher(cfg, noLogger())
	f.Fetch(context.Background(), "gh_pr:wego/repo#1") //nolint:errcheck
	if gotAuth != "Bearer gh-tok" {
		t.Errorf("auth header = %q", gotAuth)
	}
}

func TestGitHubFetcher_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	cfg := Config{GHToken: "t", GHBaseURL: srv.URL, HTTPClient: srv.Client()}
	f := newGitHubFetcher(cfg, noLogger())
	_, err := f.Fetch(context.Background(), "gh_pr:wego/repo#1")
	if err == nil {
		t.Fatal("expected error for 404")
	}
}

func TestGitHubFetcher_5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := Config{GHToken: "t", GHBaseURL: srv.URL, HTTPClient: srv.Client()}
	f := newGitHubFetcher(cfg, noLogger())
	_, err := f.Fetch(context.Background(), "gh_pr:wego/repo#1")
	if err == nil {
		t.Fatal("expected error for 5xx")
	}
}
