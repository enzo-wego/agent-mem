package handlers

import "testing"

func TestParseBoardEpicPage(t *testing.T) {
	body := []byte(`{"maxResults":50,"startAt":0,"isLast":false,"values":[` +
		`{"id":1,"key":"PAY-100","name":"A"},` +
		`{"id":2,"key":"PAY-50","name":"B"},` +
		`{"id":3,"key":"","name":"skip-blank"}]}`)

	keys, isLast, err := parseBoardEpicPage(body)
	if err != nil {
		t.Fatalf("parseBoardEpicPage: %v", err)
	}
	if isLast {
		t.Fatalf("isLast = true, want false")
	}
	want := []string{"PAY-100", "PAY-50"} // board order preserved; blank key dropped
	if len(keys) != len(want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("keys[%d] = %q, want %q", i, keys[i], want[i])
		}
	}
}

func TestParseBoardIssueParents(t *testing.T) {
	body := []byte(`{"total":3,"issues":[` +
		`{"fields":{"parent":{"key":"PAY-974"}}},` +
		`{"fields":{"parent":{"key":"PAY-1581"}}},` +
		`{"fields":{"parent":null}}]}`)

	parents, pageLen, total, err := parseBoardIssueParents(body)
	if err != nil {
		t.Fatalf("parseBoardIssueParents: %v", err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	if pageLen != 3 {
		t.Fatalf("pageLen = %d, want 3 (issue count incl. parentless, for pagination)", pageLen)
	}
	want := []string{"PAY-974", "PAY-1581"} // null parent dropped
	if len(parents) != len(want) {
		t.Fatalf("parents = %v, want %v", parents, want)
	}
	for i := range want {
		if parents[i] != want[i] {
			t.Fatalf("parents[%d] = %q, want %q", i, parents[i], want[i])
		}
	}
}
