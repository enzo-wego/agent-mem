package fetchers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDatadogFetcher_Matches(t *testing.T) {
	f := &datadogFetcher{}
	cases := []struct {
		input string
		want  bool
	}{
		{"datadog:monitor:133274814", true},
		{"datadog:dashboard:456", true},
		{"https://app.datadoghq.com/monitors/133274814", true},
		{"jira:PAY-123", false},
		{"slack:C:1.2", false},
		{"datadog:unknown:123", false},
	}
	for _, tc := range cases {
		got := f.Matches(tc.input)
		if got != tc.want {
			t.Errorf("Matches(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestDatadogFetcher_HappyPath(t *testing.T) {
	monitor := ddMonitorResponse{
		ID:       133274814,
		Name:     "High latency alert",
		Modified: time.Now(),
		Creator:  &ddCreator{Name: "Alice", Email: "alice@example.com"},
	}

	var gotAPIKey, gotAppKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("DD-API-KEY")
		gotAppKey = r.Header.Get("DD-APPLICATION-KEY")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(monitor)
	}))
	defer srv.Close()

	cfg := Config{
		DatadogAPIKey:  "dd-api-key",
		DatadogAppKey:  "dd-app-key",
		DatadogBaseURL: srv.URL,
		HTTPClient:     srv.Client(),
	}
	f := newDatadogFetcher(cfg, noLogger())
	body, err := f.Fetch(context.Background(), "datadog:monitor:133274814")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body.NodeID != "datadog:monitor:133274814" {
		t.Errorf("nodeID = %q", body.NodeID)
	}
	if body.Title != "High latency alert" {
		t.Errorf("title = %q", body.Title)
	}
	if body.Author.Email != "alice@example.com" {
		t.Errorf("author email = %q", body.Author.Email)
	}
	if gotAPIKey != "dd-api-key" {
		t.Errorf("DD-API-KEY = %q", gotAPIKey)
	}
	if gotAppKey != "dd-app-key" {
		t.Errorf("DD-APPLICATION-KEY = %q", gotAppKey)
	}
}

func TestDatadogFetcher_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	cfg := Config{DatadogAPIKey: "k", DatadogAppKey: "a", DatadogBaseURL: srv.URL, HTTPClient: srv.Client()}
	f := newDatadogFetcher(cfg, noLogger())
	_, err := f.Fetch(context.Background(), "datadog:monitor:123")
	if err == nil {
		t.Fatal("expected error for 404")
	}
}

func TestDatadogFetcher_5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := Config{DatadogAPIKey: "k", DatadogAppKey: "a", DatadogBaseURL: srv.URL, HTTPClient: srv.Client()}
	f := newDatadogFetcher(cfg, noLogger())
	_, err := f.Fetch(context.Background(), "datadog:monitor:123")
	if err == nil {
		t.Fatal("expected error for 5xx")
	}
}
