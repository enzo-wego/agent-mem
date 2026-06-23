package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agent-mem/agent-mem/internal/graph/handlers"
)

func TestSearch_SemanticAndType(t *testing.T) {
	pool := testDB(t)
	seedNode(t, pool, "slack:C:1", "slack_thread", "TripleA refund returns none, Juspay maps to pending")
	seedNode(t, pool, "jira:PAY-3", "jira", "PAY-3 Tabby installments")
	// No embedder needed for keyword fallback in test.

	h, err := handlers.NewSearch(pool)
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET",
		"/api/graph/search?q=TripleA+refund&types=slack_thread&limit=5", nil)
	r.Header.Set("X-Asker-User", "U07UAC0J7T3")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Results []struct {
			NodeID string `json:"node_id"`
			Type   string `json:"type"`
		} `json:"results"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Results) == 0 || resp.Results[0].NodeID != "slack:C:1" {
		t.Errorf("expected slack:C:1 top hit; got %v", resp.Results)
	}
}
