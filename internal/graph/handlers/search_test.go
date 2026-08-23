package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agent-mem/agent-mem/internal/gemini"
	"github.com/agent-mem/agent-mem/internal/graph/handlers"
	"github.com/pgvector/pgvector-go"
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

type fixedSearchEmbedder struct {
	vector []float32
}

func (e fixedSearchEmbedder) Embed(context.Context, string) ([]float32, error) {
	return e.vector, nil
}

func (e fixedSearchEmbedder) EmbedWithOptions(context.Context, string, gemini.EmbedOptions) ([]float32, error) {
	return e.vector, nil
}

func TestSearch_SemanticExcludesNullEmbeddingsBelowLimit(t *testing.T) {
	pool := testDB(t)
	seedNode(t, pool, "jira:PAY-1", "jira", "indexed result")
	seedNode(t, pool, "jira:PAY-2", "jira", "null result")

	vector := make([]float32, handlers.GraphEmbeddingDims)
	vector[0] = 1
	if _, err := pool.Exec(context.Background(), `
INSERT INTO graph.artifact_index
  (node_id, summary, summary_kind, embedding, refreshed_at, machine_id)
VALUES
  ('jira:PAY-1', 'indexed result', 'heuristic', $1, NOW(), 'test'),
  ('jira:PAY-2', 'null result', 'heuristic', NULL, NOW(), 'test')`,
		pgvector.NewVector(vector)); err != nil {
		t.Fatalf("seed artifact index: %v", err)
	}

	h, err := handlers.NewSearchWithEmbedder(pool, fixedSearchEmbedder{vector: vector})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/api/graph/search?q=payment&limit=10", nil)
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
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Results) != 1 || resp.Results[0].NodeID != "jira:PAY-1" {
		t.Fatalf("results = %+v, want only jira:PAY-1", resp.Results)
	}
}
