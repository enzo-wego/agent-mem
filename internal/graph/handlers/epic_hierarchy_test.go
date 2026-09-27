package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Fixture: one epic (PAY-100) with two issues (PAY-101, PAY-102); thread A
// REFERENCES PAY-101; thread B is SAME_TOPIC to A at 0.85; thread C is
// SAME_TOPIC to A at 0.6 and must NOT join; a reply under A inherits; thread D
// is SAME_TOPIC to B at 0.9 (chained, must NOT join); message E has an eligible
// gate decision and joins only the business root.
func seedEpicFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("fixture: %v\n%s", err, sql)
		}
	}
	node := func(id, typ, key, meta string) {
		exec(`INSERT INTO graph.nodes (id, type, natural_key, title, scope, metadata, machine_id)
		      VALUES ($1,$2,$3,$1,'slack:C1',$4::jsonb,'test')`, id, typ, key, meta)
	}
	node("jira:PAY-100", "jira", "PAY-100", `{}`)
	node("jira:PAY-101", "jira", "PAY-101", `{}`)
	node("jira:PAY-102", "jira", "PAY-102", `{}`)
	node("slack:C1:1.0", "slack", "slack:C1:1.0", `{"thread_ts":"1.0"}`) // thread A root
	node("slack:C1:1.5", "slack", "slack:C1:1.5", `{"thread_ts":"1.0"}`) // reply under A
	node("slack:C1:2.0", "slack", "slack:C1:2.0", `{"thread_ts":"2.0"}`) // B: 0.85 to A
	node("slack:C1:3.0", "slack", "slack:C1:3.0", `{"thread_ts":"3.0"}`) // C: 0.6 to A
	node("slack:C1:4.0", "slack", "slack:C1:4.0", `{"thread_ts":"4.0"}`) // D: 0.9 to B (chained)
	node("slack:C1:5.0", "slack", "slack:C1:5.0", `{"thread_ts":"5.0"}`) // E: eligible only
	node("gh_pr:wego/payments#1", "gh_pr", "wego/payments#1", `{}`)      // PR → PAY-102

	exec(`INSERT INTO graph.jira_epic_map (issue_key, epic_key, epic_summary, machine_id) VALUES
	      ('PAY-100','PAY-100','Epic one','test'),
	      ('PAY-101','PAY-100','Epic one','test'),
	      ('PAY-102','PAY-100','Epic one','test')`)

	edge := func(from, to, kind, meta string) {
		exec(`INSERT INTO graph.edges (from_node_id, to_node_id, kind, metadata, machine_id)
		      VALUES ($1,$2,$3,$4::jsonb,'test')`, from, to, kind, meta)
	}
	edge("slack:C1:1.0", "jira:PAY-101", "REFERENCES", `{}`)
	edge("gh_pr:wego/payments#1", "jira:PAY-102", "REFERENCES", `{}`)
	edge("slack:C1:1.0", "slack:C1:2.0", "SAME_TOPIC", `{"confidence":"0.85"}`)
	edge("slack:C1:3.0", "slack:C1:1.0", "SAME_TOPIC", `{"confidence":"0.6"}`)
	edge("slack:C1:2.0", "slack:C1:4.0", "SAME_TOPIC", `{"confidence":"0.9"}`)

	exec(`INSERT INTO graph.eligibility_decisions
	      (channel_id, message_ts, score, decision, mode, scope_version, decision_source)
	      VALUES ('C1','5.0',0.71,'eligible','enforce',NOW(),'scored')`)
}

func membershipRows(t *testing.T, pool *pgxpool.Pool) map[[2]string]struct {
	via  string
	conf float64
} {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT node_id, epic_key, via, confidence FROM graph.epic_membership`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[[2]string]struct {
		via  string
		conf float64
	}{}
	for rows.Next() {
		var n, e, v string
		var c float64
		if err := rows.Scan(&n, &e, &v, &c); err != nil {
			t.Fatal(err)
		}
		out[[2]string{n, e}] = struct {
			via  string
			conf float64
		}{v, c}
	}
	return out
}

func TestRebuildEpicHierarchy_Fixture(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	t.Cleanup(func() { truncateGraphHandlerTables(t, pool) })
	seedEpicFixture(t, pool)

	if err := rebuildEpicHierarchy(context.Background(), pool, "test", "PAY", map[string]int{"PAY-100": 0}); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	// PART_OF: two issues → epic, epic → business root; no epic self-edge.
	var partOf []string
	rows, err := pool.Query(context.Background(),
		`SELECT from_node_id || '>' || to_node_id FROM graph.edges
		 WHERE kind='PART_OF' AND metadata->>'method'='jira-epic-link' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		partOf = append(partOf, s)
	}
	rows.Close()
	wantEdges := []string{
		"jira:PAY-100>business:payments",
		"jira:PAY-101>jira:PAY-100",
		"jira:PAY-102>jira:PAY-100",
	}
	if len(partOf) != len(wantEdges) {
		t.Fatalf("PART_OF edges = %v, want %v", partOf, wantEdges)
	}
	for i := range wantEdges {
		if partOf[i] != wantEdges[i] {
			t.Fatalf("PART_OF edges = %v, want %v", partOf, wantEdges)
		}
	}

	m := membershipRows(t, pool)
	want := map[[2]string]struct {
		via  string
		conf float64
	}{
		{"jira:PAY-100", "PAY-100"}:          {viaEpicSelf, 1},
		{"jira:PAY-101", "PAY-100"}:          {viaEpicSelf, 1},
		{"jira:PAY-102", "PAY-100"}:          {viaEpicSelf, 1},
		{"slack:C1:1.0", "PAY-100"}:          {viaKey, 1},
		{"gh_pr:wego/payments#1", "PAY-100"}: {viaKey, 1},
		{"slack:C1:2.0", "PAY-100"}:          {viaTopicLink, 0.85},
		{"slack:C1:1.5", "PAY-100"}:          {viaKey, 1}, // reply inherits root
	}
	for k, w := range want {
		got, ok := m[k]
		if !ok {
			t.Errorf("missing membership %v (want %s)", k, w.via)
			continue
		}
		if got.via != w.via || got.conf != w.conf {
			t.Errorf("membership %v = %+v, want %+v", k, got, w)
		}
	}
	for _, id := range []string{"slack:C1:3.0", "slack:C1:4.0", "slack:C1:5.0"} {
		if r, ok := m[[2]string{id, "PAY-100"}]; ok {
			t.Errorf("%s must not be a PAY-100 member, got %+v", id, r)
		}
	}

	// Business root: every epic member, the eligible message, and the root itself.
	if r := m[[2]string{"slack:C1:5.0", businessRootID}]; r.via != viaEligible || r.conf != 0.71 {
		t.Errorf("eligible message business row = %+v", r)
	}
	if r := m[[2]string{"slack:C1:2.0", businessRootID}]; r.via != viaTopicLink || r.conf != 0.85 {
		t.Errorf("topic_link member business row = %+v", r)
	}
	if r := m[[2]string{businessRootID, businessRootID}]; r.via != viaEpicSelf {
		t.Errorf("business root self row = %+v", r)
	}
	if _, ok := m[[2]string{"slack:C1:3.0", businessRootID}]; ok {
		t.Errorf("0.6 topic neighbour must not reach the business root")
	}

	// Epic window on the epic's own row spans all members.
	var first, last *string
	if err := pool.QueryRow(context.Background(), `
SELECT (SELECT MIN(COALESCE(n.created_at, n.first_seen_at))::text FROM graph.epic_membership mm JOIN graph.nodes n ON n.id=mm.node_id WHERE mm.epic_key='PAY-100'),
       m.first_at::text
FROM graph.epic_membership m WHERE m.node_id='jira:PAY-100' AND m.epic_key='PAY-100'`).Scan(&first, &last); err != nil {
		t.Fatal(err)
	}
	if first == nil || last == nil || *first != *last {
		t.Errorf("epic window first_at = %v, want members' min %v", last, first)
	}

	// Re-run is idempotent and drops a row whose mapping disappeared.
	if _, err := pool.Exec(context.Background(), `DELETE FROM graph.jira_epic_map WHERE issue_key='PAY-102'`); err != nil {
		t.Fatal(err)
	}
	if err := rebuildEpicHierarchy(context.Background(), pool, "test", "PAY", map[string]int{"PAY-100": 0}); err != nil {
		t.Fatalf("rebuild 2: %v", err)
	}
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM graph.edges WHERE kind='PART_OF' AND from_node_id='jira:PAY-102'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("stale PART_OF edge for PAY-102 survived the rebuild")
	}
	m = membershipRows(t, pool)
	if _, ok := m[[2]string{"gh_pr:wego/payments#1", "PAY-100"}]; ok {
		t.Errorf("PR referencing an unmapped issue must leave the epic")
	}
}

// The business root is never a corridor, whatever its degree.
func TestExpandableThrough_BusinessRoot(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	t.Cleanup(func() { truncateGraphHandlerTables(t, pool) })
	if _, err := pool.Exec(context.Background(), `
INSERT INTO graph.nodes (id, type, natural_key, machine_id)
VALUES ('business:payments','business','payments','test')`); err != nil {
		t.Fatal(err)
	}
	if expandableThrough(context.Background(), pool, "business:payments") {
		t.Error("business root must not be expandable through")
	}
}

// GET /api/graph/epic/{key} lists members grouped by type with via, folds
// replies into the count, and reports the epic window; unknown keys are 404.
func TestEpicEndpoint_Fixture(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	t.Cleanup(func() { truncateGraphHandlerTables(t, pool) })
	seedEpicFixture(t, pool)
	if err := rebuildEpicHierarchy(context.Background(), pool, "test", "PAY", map[string]int{"PAY-100": 0}); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	r := chi.NewRouter()
	r.Method("GET", "/api/graph/epic/{key}", NewEpic(pool))
	get := func(key string) (int, epicResponse) {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/graph/epic/"+key, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var resp epicResponse
		_ = json.NewDecoder(w.Body).Decode(&resp)
		return w.Code, resp
	}

	code, resp := get("pay-100")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if resp.EpicKey != "PAY-100" || resp.NodeID != "jira:PAY-100" || resp.FirstAt == nil || resp.LastAt == nil {
		t.Errorf("header = %+v", resp)
	}
	// jira: PAY-101, PAY-102; slack roots: A (key), B (topic_link); gh_pr: 1; reply folded.
	if got := len(resp.Members["jira"]); got != 2 {
		t.Errorf("jira members = %d, want 2", got)
	}
	if got := len(resp.Members["slack"]); got != 2 {
		t.Errorf("slack members = %d, want 2: %+v", got, resp.Members["slack"])
	}
	if got := len(resp.Members["gh_pr"]); got != 1 || resp.Members["gh_pr"][0].Via != viaKey {
		t.Errorf("gh_pr members = %+v", resp.Members["gh_pr"])
	}
	if resp.Replies != 1 || resp.Total != 6 || resp.ByVia[viaTopicLink] != 1 {
		t.Errorf("counts: replies=%d total=%d by_via=%v", resp.Replies, resp.Total, resp.ByVia)
	}

	code, resp = get("payments")
	if code != http.StatusOK || !resp.Business || resp.ByVia[viaEligible] != 1 {
		t.Errorf("business root: status %d resp %+v", code, resp)
	}
	if code, _ := get("PAY-404"); code != http.StatusNotFound {
		t.Errorf("unknown epic status = %d, want 404", code)
	}
}
