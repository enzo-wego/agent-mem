package handlers_test

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/agent-mem/agent-mem/internal/graph/handlers"
)

func TestNeighbors_Depth1(t *testing.T) {
	pool := testDB(t)
	seedNode(t, pool, "a", "slack_thread", "A")
	seedNode(t, pool, "b", "jira", "B")
	seedNode(t, pool, "c", "gh_pr", "C")
	seedEdge(t, pool, "a", "b", "REFERENCES")
	seedEdge(t, pool, "a", "c", "REFERENCES")

	r := chi.NewRouter()
	r.Mount("/api/graph", handlers.NewNeighbors(pool))

	req := httptest.NewRequest("GET", "/api/graph/node/a/neighbors?depth=1", nil)
	req.Header.Set("X-Asker-User", "U07UAC0J7T3")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Neighbors []struct {
			Node struct {
				NodeID string `json:"node_id"`
			} `json:"node"`
			Edge struct {
				Kind string `json:"kind"`
			} `json:"edge"`
			Hop int `json:"hop"`
		} `json:"neighbors"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Neighbors) != 2 {
		t.Errorf("want 2 neighbours, got %d", len(resp.Neighbors))
	}
}
