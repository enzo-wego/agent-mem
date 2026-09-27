package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/handlers"
)

// seedMembership writes one graph.epic_membership row directly (the rebuild
// itself is covered in epic_hierarchy_test.go).
func seedMembership(t *testing.T, pool *pgxpool.Pool, nodeID, epicKey, via string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO graph.epic_membership (node_id, epic_key, via, confidence)
VALUES ($1, $2, $3, 1.0) ON CONFLICT DO NOTHING`, nodeID, epicKey, via); err != nil {
		t.Fatalf("seedMembership: %v", err)
	}
}

func TestSearch_EpicScope(t *testing.T) {
	pool := testDB(t)
	seedNode(t, pool, "slack:C:1", "slack", "GST invoice thread inside the epic")
	seedNode(t, pool, "slack:C:2", "slack", "GST invoice thread outside the epic")
	seedNode(t, pool, "slack:C:3", "slack", "GST invoice thread in payments only")
	seedMembership(t, pool, "slack:C:1", "PAY-2307", "key")
	seedMembership(t, pool, "slack:C:1", "business:payments", "key")
	seedMembership(t, pool, "slack:C:3", "business:payments", "eligible")

	h, err := handlers.NewSearch(pool)
	if err != nil {
		t.Fatal(err)
	}
	ids := func(rawQuery string) []string {
		t.Helper()
		r := httptest.NewRequest("GET", "/api/graph/search?"+rawQuery, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status %d body %s", w.Code, w.Body.String())
		}
		var resp struct {
			Results []struct {
				NodeID string `json:"node_id"`
			} `json:"results"`
		}
		_ = json.NewDecoder(w.Body).Decode(&resp)
		out := make([]string, 0, len(resp.Results))
		for _, r := range resp.Results {
			out = append(out, r.NodeID)
		}
		return out
	}

	if got := ids("q=GST+invoice"); len(got) != 3 {
		t.Errorf("unscoped = %v, want all 3", got)
	}
	if got := ids("q=GST+invoice&epic=PAY-2307"); len(got) != 1 || got[0] != "slack:C:1" {
		t.Errorf("epic scope = %v, want [slack:C:1]", got)
	}
	if got := ids("q=GST+invoice&business=payments"); len(got) != 2 {
		t.Errorf("business scope = %v, want C:1 and C:3", got)
	}
	if got := ids("q=GST+invoice&epic=PAY-9999"); len(got) != 0 {
		t.Errorf("unknown epic = %v, want none", got)
	}

	r := httptest.NewRequest("GET", "/api/graph/search?q=GST&business=loyalty", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("unknown business: status %d, want 400", w.Code)
	}
}

func TestNeighbors_EpicScope(t *testing.T) {
	pool := testDB(t)
	seedNode(t, pool, "root", "slack_thread", "Root")
	seedNode(t, pool, "in", "jira", "In-scope ticket")
	seedNode(t, pool, "out", "jira", "Out-of-scope ticket")
	seedEdge(t, pool, "root", "in", "REFERENCES")
	seedEdge(t, pool, "root", "out", "REFERENCES")
	seedMembership(t, pool, "in", "PAY-2307", "epic_self")

	r := chi.NewRouter()
	r.Mount("/api/graph", handlers.NewNeighbors(pool))
	req := httptest.NewRequest("GET", "/api/graph/node/root/neighbors?depth=1&epic=PAY-2307", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Neighbors []struct {
			Node struct {
				NodeID string `json:"node_id"`
			} `json:"node"`
		} `json:"neighbors"`
	}
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Neighbors) != 1 || resp.Neighbors[0].Node.NodeID != "in" {
		t.Errorf("scoped neighbors = %+v, want only \"in\"", resp.Neighbors)
	}
}

func TestResolve_EpicSeedAndScope(t *testing.T) {
	pool := testDB(t)
	seedNode(t, pool, "jira:PAY-100", "jira", "Epic")
	seedNode(t, pool, "jira:PAY-101", "jira", "Issue under epic")
	seedNode(t, pool, "slack:C:1", "slack", "Thread in epic")
	seedNode(t, pool, "slack:C:9", "slack", "Thread outside epic")
	seedEdge(t, pool, "jira:PAY-101", "jira:PAY-100", "PART_OF")
	seedEdge(t, pool, "slack:C:1", "jira:PAY-101", "REFERENCES")
	seedEdge(t, pool, "slack:C:9", "jira:PAY-101", "REFERENCES")
	seedMembership(t, pool, "jira:PAY-100", "PAY-100", "epic_self")
	seedMembership(t, pool, "jira:PAY-101", "PAY-100", "epic_self")
	seedMembership(t, pool, "slack:C:1", "PAY-100", "key")
	seedBody(t, pool, "slack:C:1", "GST invoice discussion inside the epic")
	seedBody(t, pool, "slack:C:9", "GST invoice discussion outside the epic")

	h, err := handlers.NewResolve(pool)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"seeds": []string{"epic:PAY-100"},
		"query": "what is in the epic",
		"depth": 1,
		"epic":  []string{"PAY-100"},
	})
	r := httptest.NewRequest("POST", "/api/graph/resolve", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Artifacts []struct {
			NodeID string `json:"node_id"`
			Hop    int    `json:"hop"`
		} `json:"artifacts"`
	}
	_ = json.NewDecoder(w.Body).Decode(&resp)
	got := map[string]int{}
	for _, a := range resp.Artifacts {
		got[a.NodeID] = a.Hop
	}
	// The epic seed expands to the epic and its issue (both hop 0); the
	// in-epic thread is reachable at hop 1; the outside thread is scoped away.
	for _, id := range []string{"jira:PAY-100", "jira:PAY-101"} {
		if hop, ok := got[id]; !ok || hop != 0 {
			t.Errorf("%s hop = %d (present %v), want seed at hop 0; got %v", id, hop, ok, got)
		}
	}
	if _, ok := got["slack:C:1"]; !ok {
		t.Errorf("in-epic thread missing: %v", got)
	}
	if _, ok := got["slack:C:9"]; ok {
		t.Errorf("out-of-epic thread leaked: %v", got)
	}
}
