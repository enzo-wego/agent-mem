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

// A referenced node we never fetched (no title, no url) is an un-enriched stub —
// e.g. "RFC-53" mis-typed as a Jira key, or a "feature:" entity. It must not
// surface in the panel as a raw, un-openable id row.
func TestNeighbors_DropsUnenrichedStubs(t *testing.T) {
	pool := testDB(t)
	seedNode(t, pool, "root", "slack_thread", "Root")
	seedNode(t, pool, "real", "jira", "Real ticket")
	seedNodeURL(t, pool, "real", "https://wegomushi.atlassian.net/browse/PAY-1")
	seedNode(t, pool, "jira:RFC-53", "jira", "") // stub: no title, no url
	seedEdge(t, pool, "root", "real", "REFERENCES")
	seedEdge(t, pool, "root", "jira:RFC-53", "REFERENCES")

	r := chi.NewRouter()
	r.Mount("/api/graph", handlers.NewNeighbors(pool))

	req := httptest.NewRequest("GET", "/api/graph/node/root/neighbors?depth=1", nil)
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
		} `json:"neighbors"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Neighbors) != 1 {
		t.Fatalf("want 1 neighbour (stub dropped), got %d", len(resp.Neighbors))
	}
	if resp.Neighbors[0].Node.NodeID != "real" {
		t.Errorf("want enriched node 'real', got %q", resp.Neighbors[0].Node.NodeID)
	}
}
