package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
