package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/agent-mem/agent-mem/internal/graph/handlers"
)

func TestNode_LookupByURL(t *testing.T) {
	pool := testDB(t)
	seedNode(t, pool, "jira:PAY-2128", "jira", "Tabby authorizations failing")
	seedNodeURL(t, pool, "jira:PAY-2128", "https://wegomushi.atlassian.net/browse/PAY-2128")

	h := handlers.NewNode(pool)
	r := httptest.NewRequest("GET",
		"/api/graph/node?url=https%3A%2F%2Fwegomushi.atlassian.net%2Fbrowse%2FPAY-2128", nil)
	r.Header.Set("X-Asker-User", "U07UAC0J7T3")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		NodeID string `json:"node_id"`
		Type   string `json:"type"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.NodeID != "jira:PAY-2128" {
		t.Errorf("got node_id=%q", resp.NodeID)
	}
}

func TestNode_404OnMissing(t *testing.T) {
	pool := testDB(t)
	h := handlers.NewNode(pool)
	r := httptest.NewRequest("GET", "/api/graph/node?url=https%3A%2F%2Fnope", nil)
	r.Header.Set("X-Asker-User", "U07UAC0J7T3")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status %d want 404", w.Code)
	}
}

// A pasted Slack permalink — exactly as Slack's "Copy link" produces it, with
// ?thread_ts=…&cid=… and any #fragment — must resolve to the message node via
// the path alone, plus a trailing-slash variant.
func TestNode_SlackPermalinkResolvesIgnoringQueryAndFragment(t *testing.T) {
	pool := testDB(t)
	const nodeID = "slack:C0AV14LGPMG:1782118242.921599"
	seedNode(t, pool, nodeID, "slack", "Saudi Rail tax thread")

	h := handlers.NewNode(pool)
	for _, tc := range []struct{ name, rawURL string }{
		{"full copy-link", "https://wego.slack.com/archives/C0AV14LGPMG/p1782118242921599?thread_ts=1781081424.346499&cid=C0AV14LGPMG"},
		{"trailing slash", "https://wego.slack.com/archives/C0AV14LGPMG/p1782118242921599/"},
		{"fragment", "https://wego.slack.com/archives/C0AV14LGPMG/p1782118242921599#thread-anchor"},
	} {
		r := httptest.NewRequest("GET", "/api/graph/node?url="+url.QueryEscape(tc.rawURL), nil)
		r.Header.Set("X-Asker-User", "U07UAC0J7T3")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d body %s", tc.name, w.Code, w.Body.String())
		}
		var resp struct {
			NodeID string `json:"node_id"`
		}
		json.NewDecoder(w.Body).Decode(&resp)
		if resp.NodeID != nodeID {
			t.Errorf("%s: got node_id=%q want %q", tc.name, resp.NodeID, nodeID)
		}
	}
}

// A non-Slack URL with tracking params must resolve to the node stored under the
// bare URL (query/fragment stripped before comparison).
func TestNode_NonSlackURLIgnoresTrackingParams(t *testing.T) {
	pool := testDB(t)
	const (
		nodeID  = "jira:PAY-2128"
		bareURL = "https://wegomushi.atlassian.net/browse/PAY-2128"
	)
	seedNode(t, pool, nodeID, "jira", "Tabby authorizations failing")
	seedNodeURL(t, pool, nodeID, bareURL)

	h := handlers.NewNode(pool)
	r := httptest.NewRequest("GET", "/api/graph/node?url="+url.QueryEscape(bareURL+"?utm_source=slack"), nil)
	r.Header.Set("X-Asker-User", "U07UAC0J7T3")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		NodeID string `json:"node_id"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.NodeID != nodeID {
		t.Errorf("got node_id=%q want %q", resp.NodeID, nodeID)
	}
}
