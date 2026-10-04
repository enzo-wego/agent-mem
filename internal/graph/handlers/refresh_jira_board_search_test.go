package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// TestJiraBoard_SearchRequest pins the request refresh_jira_board sends to
// POST /rest/api/3/search/jql and checks that two pages are followed.
func TestJiraBoard_SearchRequest(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/rest/api/3/search/jql" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		u, p, ok := r.BasicAuth()
		if !ok || u != "me@x" || p != "tok" {
			t.Errorf("basic auth = %q %q %v", u, p, ok)
		}
		var b map[string]any
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Errorf("decode body: %v", err)
		}
		bodies = append(bodies, b)
		if _, has := b["nextPageToken"]; !has {
			_, _ = w.Write([]byte(`{"issues":[{"key":"PAY-1","fields":{"summary":"a"}}],"nextPageToken":"t2"}`))
			return
		}
		_, _ = w.Write([]byte(`{"issues":[{"key":"PAY-2","fields":{"summary":"b"}}]}`))
	}))
	defer srv.Close()

	var keys []string
	tok := ""
	for i := range 5 {
		rows, next, err := jiraSearchPage(context.Background(), srv.Client(), srv.URL, "me@x", "tok", "project = PAY", tok)
		if err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
		for _, r := range rows {
			keys = append(keys, r.IssueKey)
		}
		if next == "" {
			break
		}
		tok = next
	}
	if !reflect.DeepEqual(keys, []string{"PAY-1", "PAY-2"}) {
		t.Fatalf("keys = %v", keys)
	}
	if len(bodies) != 2 {
		t.Fatalf("requests = %d, want 2", len(bodies))
	}
	for i, b := range bodies {
		if b["jql"] != "project = PAY" {
			t.Errorf("page %d jql = %v", i, b["jql"])
		}
		if !reflect.DeepEqual(b["fields"], []any{"summary", "status", "parent"}) {
			t.Errorf("page %d fields = %v", i, b["fields"])
		}
		if b["maxResults"] != float64(100) {
			t.Errorf("page %d maxResults = %v", i, b["maxResults"])
		}
	}
	if _, has := bodies[0]["nextPageToken"]; has {
		t.Errorf("page 0 sent nextPageToken")
	}
	if bodies[1]["nextPageToken"] != "t2" {
		t.Errorf("page 1 nextPageToken = %v", bodies[1]["nextPageToken"])
	}
}
