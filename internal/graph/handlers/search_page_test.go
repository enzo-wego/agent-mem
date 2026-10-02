package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"github.com/agent-mem/agent-mem/internal/gemini"
	"github.com/agent-mem/agent-mem/internal/graph/handlers"
)

// ---- fixtures -------------------------------------------------------------

type failingEmbedder struct{}

func (failingEmbedder) Embed(context.Context, string) ([]float32, error) {
	return nil, errors.New("embed boom")
}

func (failingEmbedder) EmbedWithOptions(context.Context, string, gemini.EmbedOptions) ([]float32, error) {
	return nil, errors.New("embed boom")
}

func spPerson(t *testing.T, pool *pgxpool.Pool, name, slackUID string, bot bool) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `
INSERT INTO graph.people (display_name, slack_user_id, is_bot, machine_id)
VALUES ($1, NULLIF($2,''), $3, 'test') RETURNING id`, name, slackUID, bot).Scan(&id)
	if err != nil {
		t.Fatalf("spPerson %s: %v", name, err)
	}
	return id
}

// spNode inserts a node with body/scope/url/metadata set (seedNode cannot).
func spNode(t *testing.T, pool *pgxpool.Pool, id, typ, title, body, scope, url, metadata string, author int64, deleted bool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO graph.nodes (id, type, natural_key, title, body, scope, url, metadata, author_person_id, deleted_at, machine_id)
VALUES ($1, $2, $1, NULLIF($3,''), $4, NULLIF($5,''), NULLIF($6,''), $7::jsonb, NULLIF($8,0),
        CASE WHEN $9 THEN now() END, 'test')`,
		id, typ, title, body, scope, url, metadata, author, deleted)
	if err != nil {
		t.Fatalf("spNode %s: %v", id, err)
	}
}

// spMsg inserts a Slack message node slack:<ch>:<ts> in thread threadTS.
func spMsg(t *testing.T, pool *pgxpool.Pool, ch, ts, threadTS, body string, author int64, authorMeta string, deleted bool) string {
	t.Helper()
	id := "slack:" + ch + ":" + ts
	meta := map[string]any{"ts": ts, "thread_ts": threadTS}
	if authorMeta != "" {
		meta["author"] = map[string]string{"display_name": authorMeta}
	}
	mb, _ := json.Marshal(meta)
	spNode(t, pool, id, "slack", "", body, "slack:"+ch, "", string(mb), author, deleted)
	return id
}

func spChannel(t *testing.T, pool *pgxpool.Pool, ch, name string) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
INSERT INTO graph.slack_channels (slack_channel_id, name, machine_id) VALUES ($1,$2,'test')
ON CONFLICT (slack_channel_id) DO UPDATE SET name = EXCLUDED.name`, ch, name)
	if err != nil {
		t.Fatalf("spChannel: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM graph.slack_channels WHERE slack_channel_id=$1`, ch)
	})
}

func spThreadSummary(t *testing.T, pool *pgxpool.Pool, ch, threadTS, summary, overview string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO graph.thread_summaries (channel_id, thread_ts, signature, summary, overview)
VALUES ($1,$2,'sig',$3,$4)
ON CONFLICT (channel_id, thread_ts) DO UPDATE SET summary=EXCLUDED.summary, overview=EXCLUDED.overview`,
		ch, threadTS, summary, overview)
	if err != nil {
		t.Fatalf("spThreadSummary: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM graph.thread_summaries WHERE channel_id=$1 AND thread_ts=$2`, ch, threadTS)
	})
}

func spEmbed(t *testing.T, pool *pgxpool.Pool, nodeID string, vec []float32) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO graph.artifact_index (node_id, summary, summary_kind, embedding, refreshed_at, machine_id)
VALUES ($1, 'idx', 'heuristic', $2, NOW(), 'test')
ON CONFLICT (node_id) DO UPDATE SET embedding = EXCLUDED.embedding`, nodeID, pgvector.NewVector(vec))
	if err != nil {
		t.Fatalf("spEmbed %s: %v", nodeID, err)
	}
}

func spUnitVec() []float32 {
	v := make([]float32, handlers.GraphEmbeddingDims)
	v[0] = 1
	return v
}

type searchResp struct {
	Results []map[string]any `json:"results"`
	Total   int              `json:"total"`
	SemErr  string           `json:"semantic_error"`
}

func spSearch(t *testing.T, h *handlers.Search, rawQuery string, asker string) (int, searchResp, map[string]json.RawMessage) {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/graph/search?"+rawQuery, nil)
	if asker != "" {
		r.Header.Set("X-Asker-User", asker)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var resp searchResp
	var top map[string]json.RawMessage
	if w.Code == http.StatusOK {
		body := w.Body.Bytes()
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		json.Unmarshal(body, &top)
	}
	return w.Code, resp, top
}

func resultIDs(rs []map[string]any) []string {
	var ids []string
	for _, r := range rs {
		ids = append(ids, r["id"].(string))
	}
	sort.Strings(ids)
	return ids
}

func keysOf(m map[string]any) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func matchOf(r map[string]any) []string {
	var out []string
	for _, m := range r["match"].([]any) {
		out = append(out, m.(string))
	}
	sort.Strings(out)
	return out
}

// ---- neighbors cards ------------------------------------------------------

type nbRow struct {
	Hop  int            `json:"hop"`
	Edge map[string]any `json:"edge"`
	Node map[string]any `json:"node"`
}

func spNeighbors(t *testing.T, pool *pgxpool.Pool, id, query string) ([]nbRow, map[string]json.RawMessage) {
	t.Helper()
	r := chi.NewRouter()
	r.Mount("/api/graph", handlers.NewNeighbors(pool))
	req := httptest.NewRequest("GET", "/api/graph/node/"+url.PathEscape(id)+"/neighbors?"+query, nil)
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

const spRoot = "slack:CSP1:100.000001"

func seedCardsFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	spChannel(t, pool, "CSP1", "payments-dev")
	alice := spPerson(t, pool, "Alice", "USPA1", false)
	bob := spPerson(t, pool, "Bob", "USPB1", false)
	carol := spPerson(t, pool, "Carol", "USPC1", false)
	dave := spPerson(t, pool, "Dave", "USPD1", false)
	eve := spPerson(t, pool, "U0ABCDEF1", "USPE1", false)
	const th = "100.000001"
	spMsg(t, pool, "CSP1", th, th, "root message about PAN", alice, "", false)
	spMsg(t, pool, "CSP1", "101.000001", th, "alice reply", alice, "", false)
	spMsg(t, pool, "CSP1", "102.000001", th, "bob reply 1", bob, "", false)
	spMsg(t, pool, "CSP1", "103.000001", th, "bob reply 2", bob, "", false)
	spMsg(t, pool, "CSP1", "104.000001", th, "carol reply", carol, "", false)
	spMsg(t, pool, "CSP1", "105.000001", th, "dave deleted", dave, "", true)
	spMsg(t, pool, "CSP1", "106.000001", th, "eve reply", eve, "Eve", false)
	seedNode(t, pool, "jira:PAY-9", "jira", "PAY-9 PAN upload")
	seedNode(t, pool, "gh_pr:org/repo#1", "gh_pr", "PR one")
	seedEdge(t, pool, spRoot, "jira:PAY-9", "REFERENCES")
	seedEdge(t, pool, "gh_pr:org/repo#1", "jira:PAY-9", "REFERENCES")
}

var cardKeys = []string{"thread_root", "root_author", "msg_count", "participants", "participant_count"}

func assertRootCard(t *testing.T, n map[string]any) {
	t.Helper()
	if n["thread_root"] != spRoot {
		t.Errorf("thread_root = %v", n["thread_root"])
	}
	if n["root_author"] != "Alice" {
		t.Errorf("root_author = %v", n["root_author"])
	}
	if n["msg_count"] != float64(6) {
		t.Errorf("msg_count = %v, want 6", n["msg_count"])
	}
	if !reflect.DeepEqual(n["participants"], []any{"Alice", "Bob", "Carol"}) {
		t.Errorf("participants = %v", n["participants"])
	}
	if n["participant_count"] != float64(4) {
		t.Errorf("participant_count = %v, want 4", n["participant_count"])
	}
}

func TestNeighborsCards_SlackRowsCarryThreadFields(t *testing.T) {
	pool := testDB(t)
	seedCardsFixture(t, pool)

	for _, depth := range []string{"1", "2"} {
		rows, _ := spNeighbors(t, pool, "jira:PAY-9", "cards=1&depth="+depth)
		var sawRoot, sawPR bool
		slackRows := 0
		for _, row := range rows {
			switch row.Node["type"] {
			case "gh_pr":
				sawPR = true
				for _, k := range cardKeys {
					if _, ok := row.Node[k]; ok {
						t.Errorf("depth %s: gh_pr row has key %s", depth, k)
					}
				}
			case "slack", "slack_thread":
				slackRows++
				assertRootCard(t, row.Node)
				if row.Node["node_id"] == spRoot {
					sawRoot = true
				}
			}
		}
		if !sawRoot {
			t.Errorf("depth %s: root row missing", depth)
		}
		if !sawPR {
			t.Errorf("depth %s: gh_pr row missing", depth)
		}
		if depth == "2" && slackRows < 2 {
			t.Errorf("depth 2: want root and THREAD siblings, got %d slack rows", slackRows)
		}
	}
}

func TestNeighborsCards_DefaultUnchanged(t *testing.T) {
	pool := testDB(t)
	seedCardsFixture(t, pool)

	rows, top := spNeighbors(t, pool, "jira:PAY-9", "depth=2")
	if len(top) != 1 || top["neighbors"] == nil {
		t.Errorf("top-level keys = %v, want only neighbors", top)
	}
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
	subset := func(what string, got map[string]any, allowed ...string) {
		ok := map[string]bool{}
		for _, k := range allowed {
			ok[k] = true
		}
		for k := range got {
			if !ok[k] {
				t.Errorf("%s has unexpected key %q", what, k)
			}
		}
	}
	for _, row := range rows {
		subset("edge", row.Edge, "kind", "tag", "topic", "why", "confidence", "score", "verdict", "verdict_why")
		subset("node", row.Node, "node_id", "type", "url", "title", "overview", "channel", "thread_ts", "ts_ms",
			"first_ts_ms", "last_ts_ms", "pending_summary", "via")
	}
}

// ---- search ---------------------------------------------------------------

func TestSearch_DefaultModeUnchanged(t *testing.T) {
	pool := testDB(t)
	spNode(t, pool, "jira:PAY-X", "jira", "X", "PAN card upload fails", "", "", "{}", 0, false)
	spNode(t, pool, "jira:PAY-Y", "jira", "Y", "nothing relevant", "", "", "{}", 0, false)
	spEmbed(t, pool, "jira:PAY-Y", spUnitVec())

	h, err := handlers.NewSearchWithEmbedder(pool, fixedSearchEmbedder{vector: spUnitVec()})
	if err != nil {
		t.Fatal(err)
	}
	code, resp, top := spSearch(t, h, "q=PAN", "")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if ids := resultIDs(resp.Results); !reflect.DeepEqual(ids, []string{"jira:PAY-Y"}) {
		t.Errorf("results = %v, want only Y", ids)
	}
	if len(top) != 2 || top["results"] == nil || top["total"] == nil {
		t.Errorf("top-level keys = %v", top)
	}
	want := []string{"created_at", "id", "node_id", "score", "score_breakdown", "summary", "title", "type", "url"}
	if got := keysOf(resp.Results[0]); !reflect.DeepEqual(got, want) {
		t.Errorf("result keys = %v, want %v", got, want)
	}
}

func seedPANBodies(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	spNode(t, pool, "jira:PAY-1", "jira", "one", "PAN card upload fails", "", "", "{}", 0, false)
	spNode(t, pool, "jira:PAY-2", "jira", "two", "pan-india rollout", "", "", "{}", 0, false)
	spNode(t, pool, "jira:PAY-3", "jira", "three", "company settings", "", "", "{}", 0, false)
	spNode(t, pool, "jira:PAY-4", "jira", "four", "#payments channel move", "", "", "{}", 0, false)
}

func TestSearch_HybridKeywordWordBoundary(t *testing.T) {
	pool := testDB(t)
	seedPANBodies(t, pool)
	h, err := handlers.NewSearch(pool)
	if err != nil {
		t.Fatal(err)
	}
	_, resp, _ := spSearch(t, h, "q=PAN&match=hybrid", "")
	if ids := resultIDs(resp.Results); !reflect.DeepEqual(ids, []string{"jira:PAY-1", "jira:PAY-2"}) {
		t.Fatalf("PAN results = %v", ids)
	}
	for _, r := range resp.Results {
		if m := matchOf(r); !reflect.DeepEqual(m, []string{"keyword"}) {
			t.Errorf("match = %v", m)
		}
	}
	_, resp, _ = spSearch(t, h, "q="+url.QueryEscape("#payments")+"&match=hybrid", "")
	if ids := resultIDs(resp.Results); !reflect.DeepEqual(ids, []string{"jira:PAY-4"}) {
		t.Errorf("#payments results = %v", ids)
	}
}

func TestSearch_HybridFoldsRepliesToThreadRoot(t *testing.T) {
	pool := testDB(t)
	spChannel(t, pool, "CSP2", "pan-chat")
	alice := spPerson(t, pool, "Alice", "USPA2", false)
	bob := spPerson(t, pool, "Bob", "USPB2", false)
	const th = "200.000001"
	root := spMsg(t, pool, "CSP2", th, th, "kickoff", alice, "", false)
	spMsg(t, pool, "CSP2", "201.000001", th, "PAN fails again", bob, "", false)
	spMsg(t, pool, "CSP2", "202.000001", th, "still PAN", bob, "", false)
	spThreadSummary(t, pool, "CSP2", th, "PAN thread summary", "PAN overview text")

	h, err := handlers.NewSearch(pool)
	if err != nil {
		t.Fatal(err)
	}
	_, resp, _ := spSearch(t, h, "q=PAN&match=hybrid", "")
	if len(resp.Results) != 1 {
		t.Fatalf("want exactly one result, got %v", resultIDs(resp.Results))
	}
	r := resp.Results[0]
	if r["id"] != root || r["thread_root"] != root {
		t.Errorf("id = %v thread_root = %v, want %s", r["id"], r["thread_root"], root)
	}
	if r["msg_count"] != float64(3) || r["root_author"] != "Alice" {
		t.Errorf("msg_count = %v root_author = %v", r["msg_count"], r["root_author"])
	}
	if r["title"] != "PAN thread summary" || r["summary"] != "PAN overview text" {
		t.Errorf("title = %v summary = %v", r["title"], r["summary"])
	}
	if r["channel"] != "pan-chat" {
		t.Errorf("channel = %v", r["channel"])
	}
	if r["url"] != "https://wego.slack.com/archives/CSP2/p200000001" {
		t.Errorf("url = %v", r["url"])
	}
}

func TestSearch_HybridMergesSemanticAndKeyword(t *testing.T) {
	pool := testDB(t)
	spNode(t, pool, "jira:PAY-Z", "jira", "Z", "PAN handling", "", "", "{}", 0, false)
	spEmbed(t, pool, "jira:PAY-Z", spUnitVec())
	h, err := handlers.NewSearchWithEmbedder(pool, fixedSearchEmbedder{vector: spUnitVec()})
	if err != nil {
		t.Fatal(err)
	}
	_, resp, _ := spSearch(t, h, "q=PAN&match=hybrid", "")
	if len(resp.Results) != 1 || resp.Results[0]["id"] != "jira:PAY-Z" {
		t.Fatalf("results = %v", resultIDs(resp.Results))
	}
	if m := matchOf(resp.Results[0]); !reflect.DeepEqual(m, []string{"keyword", "semantic"}) {
		t.Errorf("match = %v", m)
	}
}

func TestSearch_HybridEmbedFailureFallsBackToKeyword(t *testing.T) {
	pool := testDB(t)
	seedPANBodies(t, pool)
	h, err := handlers.NewSearchWithEmbedder(pool, failingEmbedder{})
	if err != nil {
		t.Fatal(err)
	}
	code, resp, _ := spSearch(t, h, "q=PAN&match=hybrid", "")
	if code != 200 {
		t.Fatalf("hybrid status %d", code)
	}
	if resp.SemErr == "" {
		t.Error("semantic_error empty")
	}
	if ids := resultIDs(resp.Results); !reflect.DeepEqual(ids, []string{"jira:PAY-1", "jira:PAY-2"}) {
		t.Errorf("results = %v", ids)
	}
	if code, _, _ := spSearch(t, h, "q=PAN", ""); code != http.StatusInternalServerError {
		t.Errorf("default mode status %d, want 500", code)
	}
}

func TestSearch_HybridNoEmbedderIsKeywordOnly(t *testing.T) {
	pool := testDB(t)
	seedPANBodies(t, pool)
	h, err := handlers.NewSearch(pool)
	if err != nil {
		t.Fatal(err)
	}
	_, resp, _ := spSearch(t, h, "q=PAN&match=hybrid", "")
	ids := resultIDs(resp.Results)
	found := false
	for _, id := range ids {
		if id == "jira:PAY-1" {
			found = true
			for _, r := range resp.Results {
				if r["id"] == id && !reflect.DeepEqual(matchOf(r), []string{"keyword"}) {
					t.Errorf("match = %v", matchOf(r))
				}
			}
		}
		if id == "jira:PAY-3" {
			t.Error("company settings returned: ILIKE path leaked in")
		}
	}
	if !found {
		t.Errorf("positive control missing, results = %v", ids)
	}
}

func TestSearch_HybridScopeFiltered(t *testing.T) {
	pool := testDB(t)
	author := spPerson(t, pool, "Alice", "USPA3", false)
	id := spMsg(t, pool, "CSPPRIV", "300.000001", "300.000001", "PAN in a private channel", author, "", false)
	// Asker with a people row (eeid 4242) and no memberships: sees only public.
	if _, err := pool.Exec(context.Background(), `
INSERT INTO graph.people (eeid, display_name, slack_user_id, machine_id)
VALUES (4242, 'Asker', 'USPASK1', 'test') ON CONFLICT (eeid) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	h, err := handlers.NewSearch(pool)
	if err != nil {
		t.Fatal(err)
	}
	_, resp, _ := spSearch(t, h, "q=PAN&match=hybrid", "")
	if ids := resultIDs(resp.Results); !reflect.DeepEqual(ids, []string{id}) {
		t.Fatalf("positive control: results = %v", ids)
	}
	_, resp, _ = spSearch(t, h, "q=PAN&match=hybrid", "USPASK1")
	if len(resp.Results) != 0 {
		t.Errorf("private hit leaked: %v", resultIDs(resp.Results))
	}
}
