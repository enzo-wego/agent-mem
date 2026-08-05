package handlers_test

import (
	"bytes"
	"context"
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

func TestResolve_SlackReplySeedNormalizesToThreadRoot(t *testing.T) {
	pool := testDB(t)

	const (
		channelID = "C048WV1BZTK"
		rootTS    = "1779273246.053139"
		replyTS   = "1785348998.358759"
		rootID    = "slack:" + channelID + ":" + rootTS
		replyID   = "slack:" + channelID + ":" + replyTS
		jiraID    = "jira:PAY-2128"
	)

	seedNode(t, pool, rootID, "slack", "Revolut VCC card declines")
	seedBody(t, pool, rootID, "Root message")
	seedNode(t, pool, replyID, "slack", "Reply")
	seedBody(t, pool, replyID, "Reply message")
	seedNode(t, pool, jiraID, "jira", "Cross-source context")
	seedBody(t, pool, jiraID, "Jira body")
	seedEdge(t, pool, rootID, jiraID, "REFERENCES")

	if _, err := pool.Exec(context.Background(), `
UPDATE graph.nodes
SET scope = 'slack:' || $1,
    metadata = jsonb_build_object('ts', split_part(id, ':', 3))
                 || CASE WHEN id = $2
                         THEN jsonb_build_object('thread_ts', $3::text)
                         ELSE '{}'::jsonb
                    END
WHERE id = ANY($4)`,
		channelID, replyID, rootTS, []string{rootID, replyID}); err != nil {
		t.Fatal(err)
	}

	body := strings.NewReader(`{
		"seeds": ["` + replyID + `"],
		"depth": 1,
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
			NodeID   string `json:"node_id"`
			ThreadTS string `json:"thread_ts"`
			Hop      int    `json:"hop"`
		} `json:"artifacts"`
		GraphTrace struct {
			Seeds []string `json:"seeds"`
		} `json:"graph_trace"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Artifacts) == 0 {
		t.Fatal("expected resolved artifacts")
	}
	if resp.Artifacts[0].NodeID != rootID || resp.Artifacts[0].Hop != 0 {
		t.Fatalf("hop-0 artifact = %+v, want root %q", resp.Artifacts[0], rootID)
	}
	if len(resp.GraphTrace.Seeds) != 1 || resp.GraphTrace.Seeds[0] != replyID {
		t.Fatalf("graph_trace.seeds = %v, want original reply %q", resp.GraphTrace.Seeds, replyID)
	}

	var foundJira, foundReplyMetadata bool
	for _, artifact := range resp.Artifacts {
		if artifact.NodeID == jiraID {
			foundJira = true
		}
		if artifact.NodeID == replyID && artifact.ThreadTS == rootTS {
			foundReplyMetadata = true
		}
	}
	if !foundJira {
		t.Fatalf("expected root neighbor %q in %+v", jiraID, resp.Artifacts)
	}
	if !foundReplyMetadata {
		t.Fatalf("expected reply artifact thread_ts %q in %+v", rootTS, resp.Artifacts)
	}
}

// TestResolve_SlackPermalinkSeedSurfacesThreadFiles reproduces the prod case: a
// pasted Slack "Copy link" URL (a reply, with ?thread_ts&cid) seeds resolve, the
// reply promotes to its thread root, and the root's bodyless slack_file
// neighbors surface as zero-token artifacts (and as cache misses).
func TestResolve_SlackPermalinkSeedSurfacesThreadFiles(t *testing.T) {
	pool := testDB(t)

	const (
		channelID = "C0AV14LGPMG"
		rootTS    = "1781081424.346499"
		replyTS   = "1782118242.921599"
		rootID    = "slack:" + channelID + ":" + rootTS
		replyID   = "slack:" + channelID + ":" + replyTS
		file1     = "slack_file:F0B90RTPEPK"
		file2     = "slack_file:F0B6RMXUKSA"
		fullURL   = "https://wego.slack.com/archives/" + channelID + "/p1782118242921599?thread_ts=" + rootTS + "&cid=" + channelID
	)

	seedNode(t, pool, rootID, "slack", "Saudi Rail tax thread")
	seedBody(t, pool, rootID, "Discussion of Saudi Rail tax filing")
	seedNode(t, pool, replyID, "slack", "reply")
	seedBody(t, pool, replyID, "here are the sheets")
	// Files are bodyless by nature — a title + URL only, no artifact_bodies row.
	seedNode(t, pool, file1, "slack_file", "Saudi Rail - Tax Analysis")
	seedNodeURL(t, pool, file1, "https://docs.google.com/spreadsheets/d/tax")
	seedNode(t, pool, file2, "slack_file", "Saudi_Rail(HHR)_GoLive_Checklist")
	seedNodeURL(t, pool, file2, "https://docs.google.com/spreadsheets/d/golive")
	seedEdge(t, pool, rootID, file1, "REFERENCES")
	seedEdge(t, pool, rootID, file2, "REFERENCES")

	// The reply carries thread_ts so canonicalizeSeeds promotes it to the root.
	if _, err := pool.Exec(context.Background(),
		`UPDATE graph.nodes SET metadata = jsonb_build_object('thread_ts', $2::text) WHERE id = $1`,
		replyID, rootTS); err != nil {
		t.Fatal(err)
	}

	body := strings.NewReader(`{
		"seeds": ["` + fullURL + `"],
		"query": "Saudi Rail tax",
		"depth": 1,
		"budget_tokens": 16000,
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
			Type   string `json:"type"`
		} `json:"artifacts"`
		CacheMisses []string `json:"cache_misses"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Artifacts) == 0 {
		t.Fatal("want >0 artifacts, got 0")
	}
	inArtifacts := map[string]bool{}
	for _, a := range resp.Artifacts {
		inArtifacts[a.NodeID] = true
	}
	if !inArtifacts[file1] || !inArtifacts[file2] {
		t.Errorf("want both files in artifacts; got %+v", resp.Artifacts)
	}
	// Files still report as cache misses (fetch_body enqueue unchanged).
	inMisses := map[string]bool{}
	for _, m := range resp.CacheMisses {
		inMisses[m] = true
	}
	if !inMisses[file1] || !inMisses[file2] {
		t.Errorf("want both files in cache_misses; got %v", resp.CacheMisses)
	}
}
