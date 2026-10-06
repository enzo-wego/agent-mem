package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/handlers"
	"github.com/agent-mem/agent-mem/internal/graph/scoring"
)

func lkTS(n int) string { return fmt.Sprintf("%d.000001", 1790000000+n) }

// lkMsg inserts message n of the thread rooted at rootN in channel ch; when
// link is set the message REFERENCES that node. Returns the message node id.
func lkMsg(t *testing.T, pool *pgxpool.Pool, ch string, rootN, n int, link string) string {
	t.Helper()
	id := spMsg(t, pool, ch, lkTS(n), lkTS(rootN), "created a story", 0, "", false)
	if link != "" {
		seedEdge(t, pool, id, link, "REFERENCES")
	}
	return id
}

func lkHandler(t *testing.T, pool *pgxpool.Pool) *handlers.Search {
	t.Helper()
	h, err := handlers.NewSearch(pool)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func lkRows(t *testing.T, h *handlers.Search, query, asker string) ([]map[string]any, map[string]any) {
	t.Helper()
	code, top := shRaw(t, h, query, asker)
	if code != 200 {
		t.Fatalf("status %d for %s", code, query)
	}
	return shResults(top), top
}

// lkRankSources makes ids rank in the given order: the recency boost alone
// separates them (one day apart).
func lkRankSources(t *testing.T, pool *pgxpool.Pool, ids ...string) {
	t.Helper()
	shSetAlphas(t, pool, scoring.BoostAlphas{Rec: 1})
	for i, id := range ids {
		if _, err := pool.Exec(context.Background(),
			`UPDATE graph.nodes SET updated_at = now() - make_interval(days => $2) WHERE id = $1`, id, i+1); err != nil {
			t.Fatal(err)
		}
	}
}

func lkJira(t *testing.T, pool *pgxpool.Pool, key, token string) string {
	t.Helper()
	id := "jira:" + key
	spNode(t, pool, id, "jira", token+" ticket", token+" description", "", "", "{}", 0, false)
	return id
}

func lkIsLinked(r map[string]any) bool {
	return reflect.DeepEqual(r["match"], []any{"linked"})
}

func lkLinkedIDs(rs []map[string]any) []string {
	var out []string
	for _, r := range rs {
		if lkIsLinked(r) {
			out = append(out, r["id"].(string))
		}
	}
	return out
}

func lkRoot(ch string, n int) string { return "slack:" + ch + ":" + lkTS(n) }

func TestLinkedJiraSource(t *testing.T) {
	pool := testDB(t)
	spChannel(t, pool, "CLK1", "lk-chat")
	jira := lkJira(t, pool, "LK-1", "lkqtoken1")
	root := lkMsg(t, pool, "CLK1", 100, 100, jira)
	lkMsg(t, pool, "CLK1", 100, 200, "")
	rs, top := lkRows(t, lkHandler(t, pool), "q=lkqtoken1&match=hybrid", "")
	if got, want := shOrdered(rs), []string{jira, root}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	row := rs[1]
	if !lkIsLinked(row) || row["linked_via"] != jira || row["linked_via_title"] != "LK-1" {
		t.Fatalf("linked row = %v", row)
	}
	if row["score"] != rs[0]["score"] {
		t.Errorf("score %v != source score %v", row["score"], rs[0]["score"])
	}
	if ranks := row["score_breakdown"].(map[string]any)["ranks"]; ranks != nil && len(ranks.(map[string]any)) != 0 {
		t.Errorf("ranks = %v, want empty", ranks)
	}
	if top["total"] != float64(len(rs)) {
		t.Errorf("total = %v, rows = %d", top["total"], len(rs))
	}
	if row["channel"] != "lk-chat" || row["msg_count"] != float64(2) ||
		row["first_ts_ms"] != float64(1790000100000) || row["last_ts_ms"] != float64(1790000200000) {
		t.Errorf("card fields wrong: %v", row)
	}
	if _, ok := rs[0]["linked_via"]; ok {
		t.Errorf("source row has linked_via")
	}
}

func TestLinkedConfluenceSource(t *testing.T) {
	pool := testDB(t)
	spNode(t, pool, "cf:4231", "cf", "lkqtoken2 Fixture PRD", "lkqtoken2 body", "", "", "{}", 0, false)
	root := lkMsg(t, pool, "CLK2", 100, 100, "cf:4231")
	rs, _ := lkRows(t, lkHandler(t, pool), "q=lkqtoken2&match=hybrid", "")
	if got, want := shOrdered(rs), []string{"cf:4231", root}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	if !lkIsLinked(rs[1]) || rs[1]["linked_via"] != "cf:4231" || rs[1]["linked_via_title"] != "lkqtoken2 Fixture PRD" {
		t.Fatalf("linked row = %v", rs[1])
	}
}

func TestLinkedReplyReference(t *testing.T) {
	pool := testDB(t)
	jira := lkJira(t, pool, "LK-3", "lkqtoken3")
	root := lkMsg(t, pool, "CLK3", 100, 100, "")
	lkMsg(t, pool, "CLK3", 100, 200, jira)
	lkMsg(t, pool, "CLK3", 100, 300, jira)
	rs, _ := lkRows(t, lkHandler(t, pool), "q=lkqtoken3&match=hybrid", "")
	if got, want := shOrdered(rs), []string{jira, root}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	if rs[1]["node_id"] != root || !lkIsLinked(rs[1]) {
		t.Fatalf("row = %v", rs[1])
	}
}

func TestLinkedPerSourceCapNewestFirst(t *testing.T) {
	pool := testDB(t)
	jira := lkJira(t, pool, "LK-4", "lkqtoken4")
	lkMsg(t, pool, "CLK4", 100, 100, jira)
	lkMsg(t, pool, "CLK4", 200, 200, jira)
	// T3: old referencing root, newest non-referencing reply.
	lkMsg(t, pool, "CLK4", 50, 50, jira)
	lkMsg(t, pool, "CLK4", 50, 9000, "")
	// T4 and T5 share their latest message time.
	lkMsg(t, pool, "CLK4", 400, 400, jira)
	lkMsg(t, pool, "CLK4", 400, 5000, "")
	lkMsg(t, pool, "CLK4B", 500, 500, jira)
	lkMsg(t, pool, "CLK4B", 500, 5000, "")
	rs, _ := lkRows(t, lkHandler(t, pool), "q=lkqtoken4&match=hybrid", "")
	want := []string{jira, lkRoot("CLK4", 50), lkRoot("CLK4", 400), lkRoot("CLK4B", 500)}
	if got := shOrdered(rs); !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
}

func TestLinkedGlobalCap(t *testing.T) {
	pool := testDB(t)
	var srcs []string
	for i := 1; i <= 4; i++ {
		srcs = append(srcs, lkJira(t, pool, fmt.Sprintf("LK-5%d", i), "lkqtoken5"))
	}
	for i, src := range srcs {
		for j := range 3 {
			n := 1000*(i+1) + 100*(j+1)
			lkMsg(t, pool, "CLK5", n, n, src)
		}
	}
	lkRankSources(t, pool, srcs...)
	rs, _ := lkRows(t, lkHandler(t, pool), "q=lkqtoken5&match=hybrid", "")
	if got := len(lkLinkedIDs(rs)); got != 10 {
		t.Fatalf("linked rows = %d, want 10: %v", got, shOrdered(rs))
	}
	// Block layout: source, then its linked threads newest first.
	var want []string
	for i, src := range srcs {
		want = append(want, src)
		for j := 2; j >= 0; j-- {
			if i == 3 && j < 2 { // lowest-ranked source keeps only its newest thread
				continue
			}
			want = append(want, lkRoot("CLK5", 1000*(i+1)+100*(j+1)))
		}
	}
	if got := shOrdered(rs); !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v\nwant  %v", got, want)
	}
}

func TestLinkedOwnershipBeforeCaps(t *testing.T) {
	pool := testDB(t)
	a := lkJira(t, pool, "LK-6A", "lkqtoken6")
	b := lkJira(t, pool, "LK-6B", "lkqtoken6")
	lkRankSources(t, pool, a, b)
	// A links a1..a3 (newer) and X (oldest of A's four); B links X, b1, b2.
	for _, n := range []int{400, 500, 600} {
		lkMsg(t, pool, "CLK6", n, n, a)
	}
	lkMsg(t, pool, "CLK6", 100, 100, a)
	lkMsg(t, pool, "CLK6", 100, 101, b) // X, same thread
	for _, n := range []int{200, 300} {
		lkMsg(t, pool, "CLK6", n, n, b)
	}
	rs, _ := lkRows(t, lkHandler(t, pool), "q=lkqtoken6&match=hybrid", "")
	want := []string{a, lkRoot("CLK6", 600), lkRoot("CLK6", 500), lkRoot("CLK6", 400),
		b, lkRoot("CLK6", 300), lkRoot("CLK6", 200)}
	if got := shOrdered(rs); !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v\nwant  %v", got, want)
	}
}

func TestLinkedHardWindowNextEligible(t *testing.T) {
	pool := testDB(t)
	jira := lkJira(t, pool, "LK-7", "lkqtoken7")
	if _, err := pool.Exec(context.Background(), `UPDATE graph.nodes SET created_at = '2026-09-20T00:00:00Z' WHERE id = $1`, jira); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{100, 200, 300, 400} {
		lkMsg(t, pool, "CLK7", n, n, jira)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE graph.nodes SET created_at = to_timestamp((metadata->>'ts')::float8) WHERE type = 'slack'`); err != nil {
		t.Fatal(err)
	}
	// The newest thread falls outside the window.
	if _, err := pool.Exec(context.Background(),
		`UPDATE graph.nodes SET created_at = '2020-01-01T00:00:00Z' WHERE id = $1`, lkRoot("CLK7", 400)); err != nil {
		t.Fatal(err)
	}
	rs, _ := lkRows(t, lkHandler(t, pool), "q=lkqtoken7&match=hybrid&since=2026-09-01&until=2026-12-31", "")
	want := []string{jira, lkRoot("CLK7", 300), lkRoot("CLK7", 200), lkRoot("CLK7", 100)}
	if got := shOrdered(rs); !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
}

func TestLinkedDedupAgainstRanked(t *testing.T) {
	pool := testDB(t)
	jira := lkJira(t, pool, "LK-8", "lkqtoken8")
	tRoot := spMsg(t, pool, "CLK8", lkTS(100), lkTS(100), "lkqtoken8 discussion", 0, "", false)
	seedEdge(t, pool, tRoot, jira, "REFERENCES")
	u := lkMsg(t, pool, "CLK8", 200, 200, jira)
	rs, _ := lkRows(t, lkHandler(t, pool), "q=lkqtoken8&match=hybrid", "")
	seenT := 0
	for i, r := range rs {
		switch r["id"] {
		case tRoot:
			seenT++
			if !reflect.DeepEqual(r["match"], []any{"keyword"}) || r["linked_via"] != nil {
				t.Errorf("ranked T row = %v", r)
			}
		case u:
			if !lkIsLinked(r) || i == 0 || rs[i-1]["id"] != jira {
				t.Errorf("U row = %v at %d (ids %v)", r, i, shOrdered(rs))
			}
		}
	}
	if seenT != 1 || len(lkLinkedIDs(rs)) != 1 {
		t.Fatalf("T seen %d times, linked %v", seenT, lkLinkedIDs(rs))
	}
}

func TestLinkedACL(t *testing.T) {
	pool := testDB(t)
	jira := lkJira(t, pool, "LK-9", "lkqtoken9")
	vis := lkMsg(t, pool, "CLK9", 100, 100, jira)
	lkMsg(t, pool, "CLK9S", 200, 200, jira)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO graph.people (eeid, display_name, slack_user_id, machine_id) VALUES (4344, 'Asker', 'ULK9ASK', 'test')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO graph.member_scopes (eeid, scope) VALUES (4344, 'slack:CLK9')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM graph.member_scopes WHERE eeid = 4344`) })
	rs, _ := lkRows(t, lkHandler(t, pool), "q=lkqtoken9&match=hybrid", "ULK9ASK")
	if got := lkLinkedIDs(rs); !reflect.DeepEqual(got, []string{vis}) {
		t.Fatalf("linked = %v, want [%s]", got, vis)
	}
}

func TestLinkedEpicFilter(t *testing.T) {
	pool := testDB(t)
	jira := lkJira(t, pool, "LK-10", "lkqtoken10")
	in := lkMsg(t, pool, "CLK10", 100, 100, jira)
	lkMsg(t, pool, "CLK10", 200, 200, jira)
	seedMembership(t, pool, jira, "PAY-10", "key")
	seedMembership(t, pool, in, "PAY-10", "key")
	rs, _ := lkRows(t, lkHandler(t, pool), "q=lkqtoken10&match=hybrid&epic=PAY-10", "")
	if got := lkLinkedIDs(rs); !reflect.DeepEqual(got, []string{in}) {
		t.Fatalf("linked = %v, want [%s]", got, in)
	}
}

func TestLinkedMissingOrDeletedRoot(t *testing.T) {
	pool := testDB(t)
	jira := lkJira(t, pool, "LK-11", "lkqtoken11")
	// Thread 100: root soft-deleted. Thread 200: root never existed.
	spNode(t, pool, lkRoot("CLK11", 100), "slack", "", "gone", "slack:CLK11", "",
		fmt.Sprintf(`{"ts":%q,"thread_ts":%q}`, lkTS(100), lkTS(100)), 0, true)
	lkMsg(t, pool, "CLK11", 100, 101, jira)
	lkMsg(t, pool, "CLK11", 200, 201, jira)
	ok := lkMsg(t, pool, "CLK11", 300, 300, jira)
	rs, _ := lkRows(t, lkHandler(t, pool), "q=lkqtoken11&match=hybrid", "")
	if got := lkLinkedIDs(rs); !reflect.DeepEqual(got, []string{ok}) {
		t.Fatalf("linked = %v, want [%s]", got, ok)
	}
}

func TestLinkedOtherSourceTypesDoNotExpand(t *testing.T) {
	pool := testDB(t)
	spNode(t, pool, "gh_pr:wego/x#12", "gh_pr", "lkqtoken12 pr", "lkqtoken12 body", "", "", "{}", 0, false)
	lkMsg(t, pool, "CLK12", 100, 100, "gh_pr:wego/x#12")
	jira := lkJira(t, pool, "LK-12", "lkqtoken12")
	jt := lkMsg(t, pool, "CLK12", 200, 200, jira)
	rs, _ := lkRows(t, lkHandler(t, pool), "q=lkqtoken12&match=hybrid", "")
	if got := lkLinkedIDs(rs); !reflect.DeepEqual(got, []string{jt}) {
		t.Fatalf("linked = %v, want [%s]", got, jt)
	}
}

func TestLinkedOnlyKeptSourcesExpand(t *testing.T) {
	pool := testDB(t)
	kept := lkJira(t, pool, "LK-13A", "lkqtoken13")
	cut := lkJira(t, pool, "LK-13B", "lkqtoken13")
	lkRankSources(t, pool, kept, cut)
	kt := lkMsg(t, pool, "CLK13", 100, 100, kept)
	lkMsg(t, pool, "CLK13", 200, 200, cut)
	rs, _ := lkRows(t, lkHandler(t, pool), "q=lkqtoken13&match=hybrid&limit=1", "")
	if got, want := shOrdered(rs), []string{kept, kt}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
}

func TestLinkedTypesFilter(t *testing.T) {
	pool := testDB(t)
	h := lkHandler(t, pool)
	jira := lkJira(t, pool, "LK-14A", "lkqtoken14a")
	root := lkMsg(t, pool, "CLK14", 100, 100, jira)
	rs, _ := lkRows(t, h, "q=lkqtoken14a&match=hybrid&types=jira,cf", "")
	if got := shOrdered(rs); !reflect.DeepEqual(got, []string{jira}) {
		t.Fatalf("types=jira,cf ids = %v, want only the ticket", got)
	}
	rs, _ = lkRows(t, h, "q=lkqtoken14a&match=hybrid&types=jira,cf,slack", "")
	if got := lkLinkedIDs(rs); !reflect.DeepEqual(got, []string{root}) {
		t.Fatalf("types=jira,cf,slack linked = %v, want [%s]", got, root)
	}

	jira2 := lkJira(t, pool, "LK-14B", "lkqtoken14b")
	st := lkRoot("CLK14B", 100)
	spNode(t, pool, st, "slack_thread", "", "created a story", "slack:CLK14B", "",
		fmt.Sprintf(`{"ts":%q,"thread_ts":%q}`, lkTS(100), lkTS(100)), 0, false)
	seedEdge(t, pool, st, jira2, "REFERENCES")
	rs, _ = lkRows(t, h, "q=lkqtoken14b&match=hybrid&types=jira,slack_thread", "")
	if got := lkLinkedIDs(rs); !reflect.DeepEqual(got, []string{st}) {
		t.Fatalf("types=jira,slack_thread linked = %v, want [%s]", got, st)
	}
}

func TestLinkedNonHybridUnchanged(t *testing.T) {
	pool := testDB(t)
	h := lkHandler(t, pool)
	jira := lkJira(t, pool, "LK-15", "lkqtoken15")
	root := lkMsg(t, pool, "CLK15", 100, 100, jira)
	code, top := shRaw(t, h, "q=lkqtoken15", "")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	raw, _ := json.Marshal(top)
	if strings.Contains(string(raw), "linked_via") || strings.Contains(string(raw), root) {
		t.Fatalf("default mode leaked linked thread: %s", raw)
	}
	rs, _ := lkRows(t, h, "q=lkqtoken15&match=hybrid", "")
	if got := lkLinkedIDs(rs); !reflect.DeepEqual(got, []string{root}) {
		t.Fatalf("hybrid control linked = %v, want [%s]", got, root)
	}
}

func TestLinkedEmptyMatchKeepsKey(t *testing.T) {
	pool := testDB(t)
	// The query is the ticket key; its text never contains the key, so the
	// ticket is only pinned by pinOwnKey and has no keyword/semantic rank.
	spNode(t, pool, "jira:PAY-16", "jira", "unrelated ledger", "ledger export", "", "", "{}", 0, false)
	root := lkMsg(t, pool, "CLK16", 100, 100, "jira:PAY-16")
	r := httptest.NewRequest("GET", "/api/graph/search?q=PAY-16&match=hybrid", nil)
	w := httptest.NewRecorder()
	lkHandler(t, pool).ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	var top struct {
		Results []map[string]json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &top); err != nil {
		t.Fatal(err)
	}
	var pinned, linked bool
	for _, row := range top.Results {
		var id string
		_ = json.Unmarshal(row["id"], &id)
		switch id {
		case "jira:PAY-16":
			pinned = true
			if string(row["match"]) != "[]" {
				t.Errorf("pinned match = %s, want []", row["match"])
			}
			if _, ok := row["linked_via"]; ok {
				t.Errorf("pinned row has linked_via")
			}
			if _, ok := row["linked_via_title"]; ok {
				t.Errorf("pinned row has linked_via_title")
			}
		case root:
			linked = string(row["match"]) == `["linked"]`
		}
	}
	if !pinned || !linked {
		t.Fatalf("pinned=%v linked=%v in %s", pinned, linked, w.Body.String())
	}
}
