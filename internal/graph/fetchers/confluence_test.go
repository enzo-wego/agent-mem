package fetchers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestConfluenceFetcher_Matches(t *testing.T) {
	f := &confluenceFetcher{}
	cases := []struct {
		input string
		want  bool
	}{
		{"cf:987654321", true},
		{"https://wegomushi.atlassian.net/wiki/spaces/ENG/pages/987654321/My+Page", true},
		{"jira:PAY-123", false},
		{"slack:C:1.2", false},
		{"cf:notanumber", false},
	}
	for _, tc := range cases {
		got := f.Matches(tc.input)
		if got != tc.want {
			t.Errorf("Matches(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestConfluenceFetcher_HappyPath(t *testing.T) {
	page := cfPageResponse{
		ID:    "987654321",
		Title: "Architecture Overview",
		Body: cfBody{
			Storage: cfStorage{Value: "<p>Hello <em>world</em></p>"},
		},
		Version: cfVersion{
			AuthorID:  "acct-xyz",
			CreatedAt: time.Now(),
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u == "" || p == "" {
			http.Error(w, "no auth", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(page)
	}))
	defer srv.Close()

	cfg := Config{
		JiraEmail:   "user@example.com",
		JiraToken:   "jira-token",
		CFBaseURL:   srv.URL,
		HTTPClient:  srv.Client(),
	}
	f := newConfluenceFetcher(cfg, noLogger())
	body, err := f.Fetch(context.Background(), "cf:987654321")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body.NodeID != "cf:987654321" {
		t.Errorf("nodeID = %q", body.NodeID)
	}
	if body.Title != "Architecture Overview" {
		t.Errorf("title = %q", body.Title)
	}
	if body.Author.ExternalID != "acct-xyz" {
		t.Errorf("author = %q", body.Author.ExternalID)
	}
	if string(body.Raw) != "<p>Hello <em>world</em></p>" {
		t.Errorf("raw = %q", body.Raw)
	}
}

func TestConfluenceFetcher_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	cfg := Config{JiraEmail: "u", JiraToken: "t", CFBaseURL: srv.URL, HTTPClient: srv.Client()}
	f := newConfluenceFetcher(cfg, noLogger())
	_, err := f.Fetch(context.Background(), "cf:123")
	if err == nil {
		t.Fatal("expected error for 404")
	}
}

func TestConfluenceFetcher_5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := Config{JiraEmail: "u", JiraToken: "t", CFBaseURL: srv.URL, HTTPClient: srv.Client()}
	f := newConfluenceFetcher(cfg, noLogger())
	_, err := f.Fetch(context.Background(), "cf:123")
	if err == nil {
		t.Fatal("expected error for 5xx")
	}
}
