package fetchers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWegoHubFetcher_Matches(t *testing.T) {
	f := &wegoHubFetcher{}
	cases := []struct {
		input string
		want  bool
	}{
		{"wegohub:q4-report", true},
		{"https://internal.wego.com/hub/apps/q4-report", true},
		{"https://internal.wego.com/hub/apps/flight-dashboard-q4/index.html", true},
		{"cf:123", false},
		{"slack:C:1.2", false},
		{"wegohub:Bad_Slug", false},
	}
	for _, tc := range cases {
		if got := f.Matches(tc.input); got != tc.want {
			t.Errorf("Matches(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestWegoHubFetcher_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/files/q4-report":
			// metadata + file list, behind the Bearer token
			if r.Header.Get("Authorization") != "Bearer tok" {
				http.Error(w, "no auth", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"status":"ok","data":{"description":"Q4 finance summary","owner":"ryan@wego.com","files":["index.html","styles.css"]}}`))
		case "/apps/q4-report/index.html":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html><body><h1>Q4</h1><p>Revenue up</p></body></html>"))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := Config{WegoHubToken: "tok", WegoHubBaseURL: srv.URL, HTTPClient: srv.Client()}
	f := newWegoHubFetcher(cfg, noLogger())

	body, err := f.Fetch(context.Background(), "wegohub:q4-report")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body.NodeID != "wegohub:q4-report" {
		t.Errorf("nodeID = %q", body.NodeID)
	}
	if body.Title != "Q4 finance summary" {
		t.Errorf("title = %q", body.Title)
	}
	if body.Author.Email != "ryan@wego.com" {
		t.Errorf("author email = %q", body.Author.Email)
	}
	if !strings.Contains(string(body.Raw), "Revenue up") {
		t.Errorf("raw missing body: %q", body.Raw)
	}
	if body.URL != srv.URL+"/apps/q4-report" {
		t.Errorf("url = %q", body.URL)
	}
}

// When the metadata API is unavailable, the fetcher still serves index.html.
func TestWegoHubFetcher_MetadataDownFallsBackToIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/apps/my-app/index.html" {
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<p>hi</p>"))
			return
		}
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := Config{WegoHubBaseURL: srv.URL, HTTPClient: srv.Client()}
	f := newWegoHubFetcher(cfg, noLogger())
	body, err := f.Fetch(context.Background(), "wegohub:my-app")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body.Title != "my-app" { // falls back to slug
		t.Errorf("title = %q, want slug", body.Title)
	}
	if string(body.Raw) != "<p>hi</p>" {
		t.Errorf("raw = %q", body.Raw)
	}
}
