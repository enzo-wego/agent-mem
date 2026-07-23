package handlers_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-mem/agent-mem/internal/graph/handlers"
)

func TestResolve_SeedExpandsAndHydrates(t *testing.T) {
	pool := testDB(t)
	// Seed: thread A → references PAY-2128 → references PR 1960.
	seedNode(t, pool, "slack:C:1", "slack_thread", "TRY currency issue")
	seedBody(t, pool, "slack:C:1", "TRY currency issue body...")
	seedNode(t, pool, "jira:PAY-2128", "jira", "Tabby installments_count")
	seedBody(t, pool, "jira:PAY-2128", "Root cause: missing installments_count")
	seedNode(t, pool, "gh_pr:wego/payments#1960", "gh_pr", "fix(tabby) fallback")
	seedBody(t, pool, "gh_pr:wego/payments#1960", "PR body...")
	seedEdge(t, pool, "slack:C:1", "jira:PAY-2128", "REFERENCES")
	seedEdge(t, pool, "jira:PAY-2128", "gh_pr:wego/payments#1960", "REFERENCES")

	h, _ := handlers.NewResolve(pool)
	body := strings.NewReader(`{
		"seeds": ["slack:C:1"],
		"query": "what's the TRY currency issue?",
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
			Body   string `json:"body,omitempty"`
		} `json:"artifacts"`
		GraphTrace struct {
			Seeds         []string `json:"seeds"`
			ExpandedNodes int      `json:"expanded_nodes"`
		} `json:"graph_trace"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Artifacts) < 3 {
		t.Fatalf("want >=3 artifacts, got %d", len(resp.Artifacts))
	}
	// Seed first.
	if resp.Artifacts[0].NodeID != "slack:C:1" {
		t.Errorf("seed should be first; got %s", resp.Artifacts[0].NodeID)
	}
	var bodies string
	for _, a := range resp.Artifacts[1:] {
		bodies += a.Body
	}
	if !bytes.Contains([]byte(bodies), []byte("installments_count")) {
		t.Errorf("expected PAY-2128 body included; got %+v", resp.Artifacts)
	}
}

func TestResolve_RawURLSeedCanonicalizesToNodeID(t *testing.T) {
	pool := testDB(t)

	const (
		prID     = "gh_pr:wego/payments#2198"
		prURL    = "https://github.com/wego/payments/pull/2198"
		jiraID   = "jira:PAY-2245"
		rawQuery = "is WithRebateRepo safe to remove?"
	)

	seedNode(t, pool, prID, "gh_pr", "remove WithRebateRepo")
	seedNodeURL(t, pool, prID, prURL)
	seedBody(t, pool, prID, "Removes the unused rebate repository dependency.")
	seedNode(t, pool, jiraID, "jira", "Remove obsolete rebate repository")
	seedBody(t, pool, jiraID, "The repository is no longer used by the payment flow.")
	seedEdge(t, pool, prID, jiraID, "REFERENCES")

	body := strings.NewReader(`{
		"seeds": ["` + prURL + `"],
		"query": "` + rawQuery + `",
		"depth": 2,
		"budget_tokens": 4000,
		"include_bodies": true
	}`)
	r := httptest.NewRequest(http.MethodPost, "/api/graph/resolve", body)
	w := httptest.NewRecorder()

	h, err := handlers.NewResolve(pool)
	if err != nil {
		t.Fatal(err)
	}
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}

	var resp struct {
		Artifacts []struct {
			NodeID string `json:"node_id"`
		} `json:"artifacts"`
		GraphTrace struct {
			Seeds []string `json:"seeds"`
		} `json:"graph_trace"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Artifacts) < 2 {
		t.Fatalf("want seed plus neighbor, got %+v", resp.Artifacts)
	}
	if resp.Artifacts[0].NodeID != prID {
		t.Fatalf("first artifact = %q, want %q", resp.Artifacts[0].NodeID, prID)
	}
	if len(resp.GraphTrace.Seeds) != 1 || resp.GraphTrace.Seeds[0] != prURL {
		t.Fatalf("graph_trace.seeds = %v, want original URL %q", resp.GraphTrace.Seeds, prURL)
	}

	var foundJira bool
	for _, artifact := range resp.Artifacts {
		if artifact.NodeID == jiraID {
			foundJira = true
			break
		}
	}
	if !foundJira {
		t.Fatalf("expected neighbor %q in %+v", jiraID, resp.Artifacts)
	}
}
