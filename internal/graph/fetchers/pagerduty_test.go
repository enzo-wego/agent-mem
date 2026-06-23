package fetchers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPagerDutyFetcher_Matches(t *testing.T) {
	f := &pagerDutyFetcher{}
	cases := []struct {
		input string
		want  bool
	}{
		{"pagerduty:P8K3M2N", true},
		{"https://wegotravel.pagerduty.com/incidents/P8K3M2N", true},
		{"jira:PAY-123", false},
		{"slack:C:1.2", false},
		{"pagerduty:lower-case", false},
	}
	for _, tc := range cases {
		got := f.Matches(tc.input)
		if got != tc.want {
			t.Errorf("Matches(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestPagerDutyFetcher_HappyPath(t *testing.T) {
	resp := pdIncidentResponse{
		Incident: pdIncident{
			ID:                 "P8K3M2N",
			Title:              "High error rate",
			HTMLUrl:            "https://wegotravel.pagerduty.com/incidents/P8K3M2N",
			LastStatusChangeAt: "2024-01-15T10:30:00Z",
			FirstTriggerLogEntry: &pdLogEntry{
				Agent: &pdAgent{ID: "PAGENT1", Name: "Alice", Email: "alice@example.com"},
			},
		},
	}

	var gotAuthHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	cfg := Config{
		PagerDutyToken:   "pd-secret",
		PagerDutyBaseURL: srv.URL,
		HTTPClient:       srv.Client(),
	}
	f := newPagerDutyFetcher(cfg, noLogger())
	body, err := f.Fetch(context.Background(), "pagerduty:P8K3M2N")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body.NodeID != "pagerduty:P8K3M2N" {
		t.Errorf("nodeID = %q", body.NodeID)
	}
	if body.Title != "High error rate" {
		t.Errorf("title = %q", body.Title)
	}
	if body.Author.ExternalID != "PAGENT1" {
		t.Errorf("author = %q", body.Author.ExternalID)
	}
	if gotAuthHeader != "Token token=pd-secret" {
		t.Errorf("auth header = %q, want %q", gotAuthHeader, "Token token=pd-secret")
	}
	if body.ContentType != "application/json" {
		t.Errorf("content type = %q", body.ContentType)
	}
}

func TestPagerDutyFetcher_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	cfg := Config{PagerDutyToken: "t", PagerDutyBaseURL: srv.URL, HTTPClient: srv.Client()}
	f := newPagerDutyFetcher(cfg, noLogger())
	_, err := f.Fetch(context.Background(), "pagerduty:P8K3M2N")
	if err == nil {
		t.Fatal("expected error for 404")
	}
}

func TestPagerDutyFetcher_5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := Config{PagerDutyToken: "t", PagerDutyBaseURL: srv.URL, HTTPClient: srv.Client()}
	f := newPagerDutyFetcher(cfg, noLogger())
	_, err := f.Fetch(context.Background(), "pagerduty:P8K3M2N")
	if err == nil {
		t.Fatal("expected error for 5xx")
	}
}
