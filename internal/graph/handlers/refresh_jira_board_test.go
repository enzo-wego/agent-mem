package handlers

import "testing"

// The Jira search response parser must map parent → epic fields and tolerate
// issues with no parent (no epic).
func TestParseJiraEpicRows(t *testing.T) {
	body := []byte(`{
	  "issues": [
	    {"key":"PAY-2227","fields":{"summary":"olympias capability gate","status":{"name":"In Progress"},
	      "parent":{"key":"PAY-2197","fields":{"summary":"Scan Card","status":{"name":"To Do"}}}}},
	    {"key":"PAY-2231","fields":{"summary":"Investigate tracking","status":{"name":"To Do"}}}
	  ],
	  "nextPageToken": "abc"
	}`)
	rows, next, err := parseJiraEpicRows(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if next != "abc" {
		t.Errorf("nextPageToken = %q, want abc", next)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	r := rows[0]
	if r.IssueKey != "PAY-2227" || r.EpicKey != "PAY-2197" || r.EpicSummary != "Scan Card" ||
		r.EpicStatus != "To Do" || r.IssueStatus != "In Progress" {
		t.Errorf("row0 = %+v", r)
	}
	if rows[1].EpicKey != "" {
		t.Errorf("no-parent issue must have empty epic_key, got %q", rows[1].EpicKey)
	}
}
