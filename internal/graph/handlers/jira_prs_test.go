package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/handlers"
)

func jpSetCreated(t *testing.T, pool *pgxpool.Pool, id, ts string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE graph.nodes SET created_at = $2::timestamptz WHERE id = $1`, id, ts); err != nil {
		t.Fatal(err)
	}
}

func seedJiraPRs(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	pat := spPerson(t, pool, "Pat Author", "UJPPAT1", false)
	spNode(t, pool, "jira:JP-1", "jira", "JP-1 zebracrossing ticket", "", "", "", "{}", 0, false)
	spNode(t, pool, "jira:JP-2", "jira", "JP-2 no-PR ticket", "", "", "", "{}", 0, false)
	spNode(t, pool, "jira:JP-3", "jira", "JP-3 many PRs", "", "", "", "{}", 0, false)
	spNode(t, pool, "cf:jp-hub", "cf", "hub", "", "", "", "{}", 0, false)
	spNode(t, pool, "gh_pr:o/r#1", "gh_pr", "PR one", "", "", "https://example.com/pr/1", "{}", pat, false)
	spNode(t, pool, "gh_pr:o/r#2", "gh_pr", "PR two", "", "", "", "{}", 0, false)
	spNode(t, pool, "gh_pr:o/r#3", "gh_pr", "PR three", "", "github", "https://example.com/pr/3", "{}", 0, false)
	spNode(t, pool, "gh_pr:o/r#4", "gh_pr", "PR four", "", "", "https://example.com/pr/4", "{}", 0, false)
	spNode(t, pool, "gh_pr:o/r#5", "gh_pr", "PR five", "", "", "https://example.com/pr/5", "{}", 0, true)
	jpSetCreated(t, pool, "gh_pr:o/r#1", "2026-09-01")
	jpSetCreated(t, pool, "gh_pr:o/r#3", "2026-09-02")
	jpSetCreated(t, pool, "gh_pr:o/r#2", "2026-09-03")
	seedEdge(t, pool, "gh_pr:o/r#1", "jira:JP-1", "REFERENCES")
	seedEdge(t, pool, "jira:JP-1", "gh_pr:o/r#2", "REFERENCES")
	seedEdge(t, pool, "gh_pr:o/r#3", "jira:JP-1", "REFERENCES")
	seedEdge(t, pool, "jira:JP-1", "gh_pr:o/r#3", "REFERENCES")
	seedEdge(t, pool, "gh_pr:o/r#4", "jira:JP-1", "SAME_TOPIC")
	seedEdge(t, pool, "gh_pr:o/r#5", "jira:JP-1", "REFERENCES")
	for i := 1; i <= 25; i++ {
		id := fmt.Sprintf("gh_pr:o/c#%d", i)
		spNode(t, pool, id, "gh_pr", fmt.Sprintf("Many %d", i), "", "", "", "{}", 0, false)
		seedEdge(t, pool, id, "jira:JP-3", "REFERENCES")
	}
	seedEdge(t, pool, "cf:jp-hub", "jira:JP-1", "REFERENCES")
	seedEdge(t, pool, "cf:jp-hub", "jira:JP-2", "REFERENCES")
}

func jpAsker(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
INSERT INTO graph.people (eeid, display_name, slack_user_id, machine_id)
VALUES (4242, 'Asker', 'USPASK1', 'test') ON CONFLICT (eeid) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
}

type jpSeed struct {
	PRCount int              `json:"pr_count"`
	PRs     []map[string]any `json:"prs"`
}

func jpNeighbors(t *testing.T, pool *pgxpool.Pool, id, query, asker string) ([]nbRow, map[string]json.RawMessage) {
	t.Helper()
	r := chi.NewRouter()
	r.Mount("/api/graph", handlers.NewNeighbors(pool))
	req := httptest.NewRequest("GET", "/api/graph/node/"+url.PathEscape(id)+"/neighbors?"+query, nil)
	if asker != "" {
		req.Header.Set("X-Asker-User", asker)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Neighbors []nbRow `json:"neighbors"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	json.Unmarshal(w.Body.Bytes(), &top)
	return resp.Neighbors, top
}

func jpRow(t *testing.T, rows []nbRow, id string) map[string]any {
	t.Helper()
	for _, r := range rows {
		if r.Node["node_id"] == id {
			return r.Node
		}
	}
	t.Fatalf("row %s missing", id)
	return nil
}

func jpPRIDs(prs []any) []string {
	var ids []string
	for _, p := range prs {
		ids = append(ids, p.(map[string]any)["node_id"].(string))
	}
	return ids
}

func jpSeedOf(t *testing.T, top map[string]json.RawMessage) (jpSeed, bool) {
	t.Helper()
	raw, ok := top["seed_prs"]
	if !ok {
		return jpSeed{}, false
	}
	var s jpSeed
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s, true
}

func TestNeighborsCards_JiraRowsCarryPRs(t *testing.T) {
	pool := testDB(t)
	seedJiraPRs(t, pool)
	rows, _ := jpNeighbors(t, pool, "cf:jp-hub", "depth=1&cards=1", "")
	jp2 := jpRow(t, rows, "jira:JP-2")
	if _, ok := jp2["pr_count"]; ok {
		t.Errorf("JP-2 has pr_count")
	}
	if _, ok := jp2["prs"]; ok {
		t.Errorf("JP-2 has prs")
	}
	jp1 := jpRow(t, rows, "jira:JP-1")
	if jp1["pr_count"] != float64(3) {
		t.Fatalf("pr_count = %v", jp1["pr_count"])
	}
	prs := jp1["prs"].([]any)
	if got, want := jpPRIDs(prs), []string{"gh_pr:o/r#2", "gh_pr:o/r#3", "gh_pr:o/r#1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("prs order = %v, want %v", got, want)
	}
	if u := prs[0].(map[string]any)["url"]; u != "" {
		t.Errorf("#2 url = %v", u)
	}
}

func TestNeighborsCards_SeedPRs(t *testing.T) {
	pool := testDB(t)
	seedJiraPRs(t, pool)
	_, top := jpNeighbors(t, pool, "jira:JP-1", "cards=1", "")
	s, ok := jpSeedOf(t, top)
	if !ok || s.PRCount != 3 {
		t.Errorf("seed_prs = %+v ok=%v", s, ok)
	}
	_, top = jpNeighbors(t, pool, "jira:JP-2", "cards=1", "")
	if _, ok := top["seed_prs"]; ok {
		t.Errorf("JP-2 has seed_prs")
	}
}

func TestNeighborsCards_SeedPRsCapsAt20(t *testing.T) {
	pool := testDB(t)
	seedJiraPRs(t, pool)
	_, top := jpNeighbors(t, pool, "jira:JP-3", "cards=1", "")
	s, ok := jpSeedOf(t, top)
	if !ok || s.PRCount != 25 || len(s.PRs) != 20 {
		t.Errorf("seed_prs count=%d len=%d ok=%v", s.PRCount, len(s.PRs), ok)
	}
}

func TestNeighbors_DefaultHasNoPRFields(t *testing.T) {
	pool := testDB(t)
	seedJiraPRs(t, pool)
	rows, top := jpNeighbors(t, pool, "jira:JP-1", "", "")
	var keys []string
	for k := range top {
		keys = append(keys, k)
	}
	if !reflect.DeepEqual(keys, []string{"neighbors"}) {
		t.Errorf("keys = %v", keys)
	}
	for _, r := range rows {
		for _, k := range []string{"pr_count", "prs"} {
			if _, ok := r.Node[k]; ok {
				t.Errorf("row %v has %s", r.Node["node_id"], k)
			}
		}
	}
}

func jpFind(rs []map[string]any, nodeID string) map[string]any {
	for _, r := range rs {
		if r["node_id"] == nodeID {
			return r
		}
	}
	return nil
}

func TestSearch_HybridJiraCarriesPRs(t *testing.T) {
	pool := testDB(t)
	seedJiraPRs(t, pool)
	h, err := handlers.NewSearch(pool)
	if err != nil {
		t.Fatal(err)
	}
	code, resp, _ := spSearch(t, h, "match=hybrid&q=zebracrossing", "")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	r := jpFind(resp.Results, "jira:JP-1")
	if r == nil || r["pr_count"] != float64(3) {
		t.Fatalf("hybrid JP-1 = %v", r)
	}
	_, resp, _ = spSearch(t, h, "q=zebracrossing", "")
	if len(resp.Results) == 0 || jpFind(resp.Results, "jira:JP-1") == nil && !containsStr(resultIDs(resp.Results), "jira:JP-1") {
		t.Fatalf("default results = %v", resultIDs(resp.Results))
	}
	for _, r := range resp.Results {
		if _, ok := r["pr_count"]; ok {
			t.Errorf("default result has pr_count")
		}
	}
}

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func TestJiraPRs_ScopeFiltered(t *testing.T) {
	pool := testDB(t)
	seedJiraPRs(t, pool)
	jpAsker(t, pool)
	h, err := handlers.NewSearch(pool)
	if err != nil {
		t.Fatal(err)
	}
	_, resp, _ := spSearch(t, h, "match=hybrid&q=zebracrossing", "USPASK1")
	r := jpFind(resp.Results, "jira:JP-1")
	if r == nil || r["pr_count"] != float64(2) {
		t.Fatalf("scoped JP-1 = %v", r)
	}
	if ids := jpPRIDs(r["prs"].([]any)); containsStr(ids, "gh_pr:o/r#3") {
		t.Errorf("#3 leaked: %v", ids)
	}
}

func TestNeighborsCards_SeedPRsScopeFiltered(t *testing.T) {
	pool := testDB(t)
	seedJiraPRs(t, pool)
	jpAsker(t, pool)
	_, top := jpNeighbors(t, pool, "jira:JP-1", "cards=1", "USPASK1")
	s, ok := jpSeedOf(t, top)
	if !ok || s.PRCount != 2 {
		t.Fatalf("seed_prs = %+v ok=%v", s, ok)
	}
	for _, p := range s.PRs {
		if p["node_id"] == "gh_pr:o/r#3" {
			t.Errorf("#3 leaked")
		}
	}
	rows, _ := jpNeighbors(t, pool, "cf:jp-hub", "depth=1&cards=1", "USPASK1")
	if jp1 := jpRow(t, rows, "jira:JP-1"); jp1["pr_count"] != float64(2) {
		t.Errorf("row pr_count = %v", jp1["pr_count"])
	}
}
