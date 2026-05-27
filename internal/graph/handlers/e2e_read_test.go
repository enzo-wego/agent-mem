package handlers_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agent-mem/agent-mem/internal/graph/handlers"
)

// TestReadE2E_TabbyIncident is the end-to-end integration test that seeds the
// Tabby incident fixture graph and verifies that /resolve over jira:PAY-2128
// surfaces all four expected nodes.
func TestReadE2E_TabbyIncident(t *testing.T) {
	pool := testDB(t)
	loadFixtureTabbyIncident(t, pool)

	h, err := handlers.NewResolve(pool)
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.NewBufferString(`{
		"seeds": ["jira:PAY-2128"],
		"query": "Lei, what's going on with TRY currency?",
		"asker_eeid": 982,
		"depth": 2,
		"budget_tokens": 4000,
		"include_bodies": true
	}`)
	r := httptest.NewRequest("POST", "/api/graph/resolve", body)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Artifacts []struct {
			NodeID string `json:"node_id"`
		} `json:"artifacts"`
	}
	json.NewDecoder(w.Body).Decode(&resp)

	wantPresent := []string{
		"jira:PAY-2128",
		"slack:C08S954G2LX:1778119437.328319",
		"gh_pr:wego/payments#1960",
		"cf:3861872666",
	}
	got := make(map[string]bool)
	for _, a := range resp.Artifacts {
		got[a.NodeID] = true
	}
	for _, want := range wantPresent {
		if !got[want] {
			t.Errorf("missing %q in artifacts; got %v", want, resp.Artifacts)
		}
	}
}
