package fetchers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestJiraFetcher_Matches(t *testing.T) {
	f := &jiraFetcher{}
	cases := []struct {
		input string
		want  bool
	}{
		{"jira:PAY-2128", true},
		{"jira:PROJ-1", true},
		{"https://wegomushi.atlassian.net/browse/PAY-2128", true},
		{"slack:C08:1.2", false},
		{"gh_pr:wego/payments#1", false},
		{"jira:lowercase-123", false},
	}
	for _, tc := range cases {
		got := f.Matches(tc.input)
		if got != tc.want {
			t.Errorf("Matches(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestJiraFetcher_HappyPath(t *testing.T) {
	adf := map[string]any{
		"version": 1,
		"type":    "doc",
		"content": []map[string]any{
			{
				"type": "paragraph",
				"content": []map[string]any{
					{"type": "text", "text": "Issue body"},
				},
			},
		},
	}
	resp := jiraIssueResponse{
		Key: "PAY-2128",
		Fields: jiraFields{
			Summary:  "Test issue",
			Updated:  "2024-01-15T10:30:00Z",
			Reporter: &jiraUser{AccountID: "acc-123", DisplayName: "Alice", EmailAddress: "alice@example.com"},
		},
	}
	adfBytes, _ := json.Marshal(adf)
	resp.Fields.Description = adfBytes

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/json" {
			http.Error(w, "bad accept", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	cfg := Config{
		JiraEmail:   "user@example.com",
		JiraToken:   "token-abc",
		JiraBaseURL: srv.URL,
		HTTPClient:  srv.Client(),
	}
	f := newJiraFetcher(cfg, noLogger())
	body, err := f.Fetch(context.Background(), "jira:PAY-2128")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body.NodeID != "jira:PAY-2128" {
		t.Errorf("nodeID = %q", body.NodeID)
	}
	if body.Title != "Test issue" {
		t.Errorf("title = %q", body.Title)
	}
	if body.Author.ExternalID != "acc-123" {
		t.Errorf("author ID = %q", body.Author.ExternalID)
	}
	if body.ContentType != "application/json" {
		t.Errorf("content type = %q", body.ContentType)
	}
	if len(body.Raw) == 0 {
		t.Error("raw body is empty")
	}
}

func TestJiraFetcher_AuthHeader(t *testing.T) {
	var user, pass string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ = r.BasicAuth()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(jiraIssueResponse{
			Key:    "PAY-1",
			Fields: jiraFields{Summary: "s"},
		})
	}))
	defer srv.Close()

	cfg := Config{
		JiraEmail:   "user@eg.com",
		JiraToken:   "secret",
		JiraBaseURL: srv.URL,
		HTTPClient:  srv.Client(),
	}
	f := newJiraFetcher(cfg, noLogger())
	f.Fetch(context.Background(), "jira:PAY-1") //nolint:errcheck
	if user != "user@eg.com" || pass != "secret" {
		t.Errorf("basic auth = %q:%q", user, pass)
	}
}

func TestJiraFetcher_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	cfg := Config{JiraEmail: "u", JiraToken: "t", JiraBaseURL: srv.URL, HTTPClient: srv.Client()}
	f := newJiraFetcher(cfg, noLogger())
	_, err := f.Fetch(context.Background(), "jira:PAY-1")
	if err == nil {
		t.Fatal("expected error for 404")
	}
}

func TestJiraFetcher_5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := Config{JiraEmail: "u", JiraToken: "t", JiraBaseURL: srv.URL, HTTPClient: srv.Client()}
	f := newJiraFetcher(cfg, noLogger())
	_, err := f.Fetch(context.Background(), "jira:PAY-1")
	if err == nil {
		t.Fatal("expected error for 5xx")
	}
}
