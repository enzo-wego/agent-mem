package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/handlers"
	"github.com/agent-mem/agent-mem/internal/graph/scoring"
)

// shRaw runs a search and returns the status plus the decoded top-level object.
func shRaw(t *testing.T, h *handlers.Search, rawQuery, asker string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/graph/search?"+rawQuery, nil)
	if asker != "" {
		r.Header.Set("X-Asker-User", asker)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var top map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &top)
	return w.Code, top
}

func shResults(top map[string]any) []map[string]any {
	var out []map[string]any
	for _, r := range top["results"].([]any) {
		out = append(out, r.(map[string]any))
	}
	return out
}

func shOrdered(rs []map[string]any) []string {
	var ids []string
	for _, r := range rs {
		ids = append(ids, r["id"].(string))
	}
	return ids
}

func shRank(r map[string]any, arm string) int {
	ranks := r["score_breakdown"].(map[string]any)["ranks"].(map[string]any)
	v, ok := ranks[arm]
	if !ok {
		return 0
	}
	return int(v.(float64))
}

// shSetAlphas writes the four boost alphas and restores the defaults after.
func shSetAlphas(t *testing.T, pool *pgxpool.Pool, a scoring.BoostAlphas) {
	t.Helper()
	del := func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM public.settings WHERE key LIKE 'graph.boost.alpha.%'`); err != nil {
			t.Fatal(err)
		}
	}
	del()
	t.Cleanup(del)
	for i, v := range []float64{a.Rec, a.Team, a.Temporal, a.Auth} {
		if _, err := pool.Exec(context.Background(), `INSERT INTO public.settings(key,value) VALUES ($1,$2)`,
			scoring.BoostAlphaKeys[i], fmt.Sprint(v)); err != nil {
			t.Fatal(err)
		}
	}
}

func shSetUpdated(t *testing.T, pool *pgxpool.Pool, id, ts string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE graph.nodes SET updated_at = $2::timestamptz WHERE id = $1`, id, ts); err != nil {
		t.Fatal(err)
	}
}

func TestSearch_HybridEnvelope(t *testing.T) {
	pool := testDB(t)
	seedJiraPRs(t, pool)
	spChannel(t, pool, "CENV", "env-chat")
	alice := spPerson(t, pool, "Alice", "UENV1", false)
	const th = "500.000001"
	root := spMsg(t, pool, "CENV", th, th, "kickoff for zebracrossing", alice, "", false)
	spMsg(t, pool, "CENV", "501.000001", th, "zebracrossing fails again", alice, "", false)
	spThreadSummary(t, pool, "CENV", th, "Zebra thread summary", "Zebra overview")
	if _, err := pool.Exec(context.Background(), `
UPDATE graph.thread_summaries SET decisions = $3 WHERE channel_id = $1 AND thread_ts = $2`, "CENV", th,
		`[{"text":"Use zebra","by":"Alice","date":"2026-10-03","ts":"501.000001"}]`); err != nil {
		t.Fatal(err)
	}
	h, err := handlers.NewSearch(pool)
	if err != nil {
		t.Fatal(err)
	}
	code, top := shRaw(t, h, "q=zebracrossing&match=hybrid", "")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	var keys []string
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if want := []string{"arm_errors", "arms", "query", "results", "semantic_error", "total"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("envelope keys = %v, want %v", keys, want)
	}
	rs := shResults(top)
	if len(rs) != 2 || top["total"] != float64(2) {
		t.Fatalf("results = %v, want the thread (one row) and the Jira issue", shOrdered(rs))
	}
	for _, r := range rs {
		if !reflect.DeepEqual(r["match"], []any{"keyword"}) {
			t.Errorf("%v match = %v", r["id"], r["match"])
		}
		wantKeys := map[string]bool{}
		for _, k := range []string{"node_id", "id", "type", "title", "url", "summary", "score", "score_breakdown",
			"created_at", "match", "first_ts_ms", "last_ts_ms", "thread_root", "channel", "root_author", "msg_count",
			"participants", "participant_count", "decisions", "open_questions", "pr_count", "prs", "author"} {
			wantKeys[k] = true
		}
		for k := range r {
			if !wantKeys[k] {
				t.Errorf("%v has unexpected key %q", r["id"], k)
			}
		}
		switch r["id"] {
		case root:
			decisions, _ := r["decisions"].([]any)
			if r["thread_root"] != root || r["title"] != "Zebra thread summary" || len(decisions) != 1 || r["msg_count"] != float64(2) {
				t.Errorf("thread row = %v", r)
			}
		case "jira:JP-1":
			if prs, _ := r["prs"].([]any); len(prs) == 0 || r["pr_count"] == nil {
				t.Errorf("jira row has no prs: %v", r)
			}
		default:
			t.Errorf("unexpected row %v", r["id"])
		}
	}
}

func TestSearch_HybridFoldsPerArm(t *testing.T) {
	pool := testDB(t)
	spChannel(t, pool, "CFLD", "fold-chat")
	alice := spPerson(t, pool, "Alice", "UFLD1", false)
	const th = "600.000001"
	root := spMsg(t, pool, "CFLD", th, th, "kickoff", alice, "", false)
	spMsg(t, pool, "CFLD", "601.000001", th, "narwhalpan fails", alice, "", false)
	spEmbed(t, pool, root, spUnitVec()) // the root matches by semantic only
	alphas := scoring.BoostAlphas{Rec: 0.3, Team: 0.25, Temporal: 0.15, Auth: 0.35}
	shSetAlphas(t, pool, alphas)

	h, err := handlers.NewSearchWithEmbedder(pool, fixedSearchEmbedder{vector: spUnitVec()})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	handlers.SetSearchNow(h, func() time.Time { return now })
	code, top := shRaw(t, h, "q=narwhalpan&match=hybrid", "")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	rs := shResults(top)
	if len(rs) != 1 || rs[0]["id"] != root {
		t.Fatalf("results = %v, want only the root", shOrdered(rs))
	}
	r := rs[0]
	if m := matchOf(r); !reflect.DeepEqual(m, []string{"keyword", "semantic"}) {
		t.Fatalf("match = %v", m)
	}
	bd := r["score_breakdown"].(map[string]any)
	wantRRF := 1/float64(scoring.RRFK+1) + 1/float64(scoring.RRFK+1)
	if got := bd["rrf"].(float64); math.Abs(got-wantRRF) > 1e-12 {
		t.Fatalf("rrf = %v, want keyword + semantic terms = %v", got, wantRRF)
	}
	var updated time.Time
	if err := pool.QueryRow(context.Background(), `SELECT updated_at FROM graph.nodes WHERE id = $1`, root).Scan(&updated); err != nil {
		t.Fatal(err)
	}
	if got, want := bd["rec"].(float64), scoring.Recency(updated, now, 30*24*time.Hour); math.Abs(got-want) > 1e-9 {
		t.Fatalf("rec = %v, want %v at the pinned now", got, want)
	}
	want := scoring.Boost(wantRRF, alphas, bd["rec"].(float64), bd["team"].(float64), bd["temporal"].(float64), bd["auth"].(float64))
	if got := r["score"].(float64); math.Abs(got-want) > 1e-12 || got == wantRRF {
		t.Fatalf("score = %v, want boosted %v (rrf %v)", got, want, wantRRF)
	}
}

func TestSearch_HybridRanksCompacted(t *testing.T) {
	pool := testDB(t)
	spChannel(t, pool, "CRNK", "rank-chat")
	alice := spPerson(t, pool, "Alice", "URNK1", false)
	const th = "700.000001"
	// Raw keyword hits, best first: reply-A1, root-A, reply-A2, B, C.
	a1 := spMsg(t, pool, "CRNK", "701.000001", th, "quillfeather reply one", alice, "", false)
	rootA := spMsg(t, pool, "CRNK", th, th, "quillfeather root", alice, "", false)
	a2 := spMsg(t, pool, "CRNK", "702.000001", th, "quillfeather reply two", alice, "", false)
	spNode(t, pool, "jira:RNK-B", "jira", "", "quillfeather B", "", "", "{}", 0, false)
	spNode(t, pool, "jira:RNK-C", "jira", "", "quillfeather C", "", "", "{}", 0, false)
	shSetUpdated(t, pool, a1, "2026-09-05")
	shSetUpdated(t, pool, rootA, "2026-09-04")
	shSetUpdated(t, pool, a2, "2026-09-03")
	shSetUpdated(t, pool, "jira:RNK-B", "2026-09-02")
	shSetUpdated(t, pool, "jira:RNK-C", "2026-09-01")
	h, err := handlers.NewSearch(pool)
	if err != nil {
		t.Fatal(err)
	}
	_, top := shRaw(t, h, "q=quillfeather&match=hybrid", "")
	got := map[string]int{}
	for _, r := range shResults(top) {
		got[r["id"].(string)] = shRank(r, "keyword")
	}
	if want := map[string]int{rootA: 1, "jira:RNK-B": 2, "jira:RNK-C": 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("keyword ranks = %v, want %v", got, want)
	}
}

func TestSearch_HybridRootIneligible(t *testing.T) {
	const th = "1.000001"
	reply := "slack:CINE:2.000001"
	root := "slack:CINE:" + th
	seedReply := func(t *testing.T, pool *pgxpool.Pool) {
		spNode(t, pool, reply, "slack", "", "ineligtoken reply", "slack:CINE", "", `{"ts":"2.000001","thread_ts":"1.000001"}`, 0, false)
	}
	cases := []struct {
		name  string
		query string
		asker string
		setup func(t *testing.T, pool *pgxpool.Pool)
		want  string
	}{
		{"eligible_control", "", "", func(t *testing.T, pool *pgxpool.Pool) {
			spNode(t, pool, root, "slack", "", "ineligtoken root", "slack:CINE", "", `{"ts":"1.000001","thread_ts":"1.000001"}`, 0, false)
		}, root},
		{"root_missing", "", "", func(t *testing.T, pool *pgxpool.Pool) {}, reply},
		{"root_deleted", "", "", func(t *testing.T, pool *pgxpool.Pool) {
			spNode(t, pool, root, "slack", "", "ineligtoken root", "slack:CINE", "", `{"ts":"1.000001","thread_ts":"1.000001"}`, 0, true)
		}, reply},
		{"root_private", "", "UINEASK", func(t *testing.T, pool *pgxpool.Pool) {
			spNode(t, pool, root, "slack", "", "ineligtoken root", "slack:CSECRET", "", `{"ts":"1.000001","thread_ts":"1.000001"}`, 0, false)
			if _, err := pool.Exec(context.Background(), `
INSERT INTO graph.people (eeid, display_name, slack_user_id, machine_id) VALUES (4343, 'Asker', 'UINEASK', 'test')`); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(context.Background(), `INSERT INTO graph.member_scopes (eeid, scope) VALUES (4343, 'slack:CINE')`); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				pool.Exec(context.Background(), `DELETE FROM graph.member_scopes WHERE eeid = 4343`)
			})
		}, reply},
		{"root_outside_types", "&types=slack", "", func(t *testing.T, pool *pgxpool.Pool) {
			spNode(t, pool, root, "slack_thread", "", "ineligtoken root", "slack:CINE", "", `{"ts":"1.000001","thread_ts":"1.000001"}`, 0, false)
		}, reply},
		{"root_outside_epic", "&epic=PAY-1", "", func(t *testing.T, pool *pgxpool.Pool) {
			spNode(t, pool, root, "slack", "", "ineligtoken root", "slack:CINE", "", `{"ts":"1.000001","thread_ts":"1.000001"}`, 0, false)
			seedMembership(t, pool, reply, "PAY-1", "key")
		}, reply},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool := testDB(t)
			seedReply(t, pool)
			c.setup(t, pool)
			h, err := handlers.NewSearch(pool)
			if err != nil {
				t.Fatal(err)
			}
			code, top := shRaw(t, h, "q=ineligtoken&match=hybrid"+c.query, c.asker)
			if code != 200 {
				t.Fatalf("status %d", code)
			}
			if got := shOrdered(shResults(top)); !reflect.DeepEqual(got, []string{c.want}) {
				t.Fatalf("results = %v, want [%s]", got, c.want)
			}
		})
	}
}

func TestSearch_HybridEpicFilter(t *testing.T) {
	pool := testDB(t)
	spChannel(t, pool, "CEPI", "epic-chat")
	alice := spPerson(t, pool, "Alice", "UEPI1", false)
	inRoot := spMsg(t, pool, "CEPI", "800.000001", "800.000001", "gnuepic kickoff", alice, "", false)
	inReply := spMsg(t, pool, "CEPI", "801.000001", "800.000001", "gnuepic reply", alice, "", false)
	spMsg(t, pool, "CEPI", "810.000001", "810.000001", "gnuepic outside thread", alice, "", false)
	spNode(t, pool, "jira:EPI-OUT", "jira", "", "gnuepic outside ticket", "", "", "{}", 0, false)
	spNode(t, pool, "jira:EPI-IN", "jira", "", "gnuepic inside ticket", "", "", "{}", 0, false)
	for _, id := range []string{inRoot, inReply, "jira:EPI-IN"} {
		seedMembership(t, pool, id, "PAY-2307", "key")
	}
	h, err := handlers.NewSearch(pool)
	if err != nil {
		t.Fatal(err)
	}
	code, top := shRaw(t, h, "q=gnuepic&match=hybrid&epic=PAY-2307", "")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	got := shOrdered(shResults(top))
	sort.Strings(got)
	if want := []string{"jira:EPI-IN", inRoot}; !reflect.DeepEqual(got, want) {
		t.Fatalf("results = %v, want %v", got, want)
	}
}

func TestSearch_HybridLimitClamp(t *testing.T) {
	pool := testDB(t)
	if _, err := pool.Exec(context.Background(), `
INSERT INTO graph.nodes (id, type, natural_key, body, metadata, machine_id)
SELECT 'jira:CL-' || i, 'jira', 'jira:CL-' || i, 'zorblaxclamp ticket ' || i, '{}', 'test'
FROM generate_series(1, 120) i`); err != nil {
		t.Fatal(err)
	}
	h, err := handlers.NewSearch(pool)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		query    string
		want     int
		hybrid   bool
		errsOnly []string // the only arm_errors keys allowed
	}{
		{"q=zorblaxclamp&match=hybrid&limit=500", 100, true, []string{"semantic"}},
		{"q=zorblaxclamp&match=hybrid&limit=100", 100, true, []string{"semantic"}},
		{"q=zorblaxclamp&match=hybrid", 50, true, []string{"semantic"}},
		{"q=zorblaxclamp&limit=500", 50, false, []string{"semantic", "temporal", "graph"}},
	}
	for _, c := range cases {
		code, top := shRaw(t, h, c.query, "")
		if code != 200 {
			t.Fatalf("%s: status %d", c.query, code)
		}
		if n := len(shResults(top)); n != c.want {
			t.Errorf("%s: %d results, want %d", c.query, n, c.want)
		}
		allowed := map[string]bool{}
		for _, k := range c.errsOnly {
			allowed[k] = true
		}
		errs, _ := top["arm_errors"].(map[string]any)
		for arm, msg := range errs {
			if !allowed[arm] {
				t.Errorf("%s: unexpected arm error %s: %v", c.query, arm, msg)
			}
		}
		if c.hybrid && (errs["semantic"] == nil || len(errs) != 1) {
			t.Errorf("%s: arm_errors = %v, want exactly semantic", c.query, errs)
		}
	}
}

func TestSearch_HybridArmsExclusive(t *testing.T) {
	pool := testDB(t)
	h, err := handlers.NewSearch(pool)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := shRaw(t, h, "q=x&match=hybrid&arms=keyword", ""); code != 400 {
		t.Fatalf("status %d, want 400", code)
	}
}

func TestSearch_HybridThreadFloodAtLimit(t *testing.T) {
	for _, limit := range []int{20, 50} {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			pool := testDB(t)
			spChannel(t, pool, "CFLO", "flood-chat")
			author := spPerson(t, pool, "Alice", "UFLO1", false)
			want := map[string]bool{}
			for i := 1; i <= 5; i++ {
				ts := fmt.Sprintf("%d.000001", i+1000)
				root := spMsg(t, pool, "CFLO", ts, ts, "FLOODTERM other thread", author, "", false)
				shSetUpdated(t, pool, root, "2025-01-01")
				want[root] = true
			}
			const th = "100.000001"
			want[spMsg(t, pool, "CFLO", th, th, "FLOODTERM flood root", author, "", false)] = true
			// More replies than 3 x limit, all newer than the other threads, so
			// the unfolded keyword list is entirely this one thread.
			if _, err := pool.Exec(context.Background(), `
INSERT INTO graph.nodes (id, type, natural_key, body, scope, metadata, author_person_id, updated_at, machine_id)
SELECT 'slack:CFLO:' || (200 + r)::text || '.000001', 'slack', 'slack:CFLO:' || (200 + r)::text || '.000001',
       'FLOODTERM reply', 'slack:CFLO', jsonb_build_object('thread_ts', $1::text), $2,
       TIMESTAMPTZ '2026-09-01' + r * INTERVAL '1 second', 'test'
FROM generate_series(1, $3::int) AS r`, th, author, 3*limit+1); err != nil {
				t.Fatal(err)
			}
			h, err := handlers.NewSearch(pool)
			if err != nil {
				t.Fatal(err)
			}
			_, top := shRaw(t, h, fmt.Sprintf("q=FLOODTERM&match=hybrid&limit=%d", limit), "")
			got := map[string]bool{}
			for _, r := range shResults(top) {
				got[r["id"].(string)] = true
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("threads = %v, want %v", got, want)
			}
		})
	}
}

func TestSearch_HybridKeywordTitleBoostOutranksSemantic(t *testing.T) {
	pool := testDB(t)
	spNode(t, pool, "jira:PAY-A", "jira", "Zebra refund", "zebra refund broke in prod", "", "", "{}", 0, false)
	spNode(t, pool, "jira:PAY-B", "jira", "Unrelated ledger", "ledger export", "", "", "{}", 0, false)
	spEmbed(t, pool, "jira:PAY-B", spUnitVec())
	v := make([]float32, handlers.GraphEmbeddingDims)
	c := handlers.SemanticMinCosine + 0.1
	v[0], v[1] = float32(c), float32(math.Sqrt(1-c*c))
	spEmbed(t, pool, "jira:PAY-A", v)
	alphas := scoring.BoostAlphas{Rec: 0.2, Team: 0.2, Temporal: 0.2, Auth: 0.1}
	shSetAlphas(t, pool, alphas)
	h, err := handlers.NewSearchWithEmbedder(pool, fixedSearchEmbedder{vector: spUnitVec()})
	if err != nil {
		t.Fatal(err)
	}
	query := "q=zebra+refund&match=hybrid"
	check := func(t *testing.T) []float64 {
		code, top := shRaw(t, h, query, "")
		if code != 200 {
			t.Fatalf("status %d", code)
		}
		rs := shResults(top)
		if got, want := shOrdered(rs), []string{"jira:PAY-A", "jira:PAY-B"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("ids = %v, want %v (keyword + semantic outranks semantic only)", got, want)
		}
		var scores []float64
		for i, r := range rs {
			wantMatch, wantRRF := []string{"semantic"}, 1/float64(scoring.RRFK+1)
			if i == 0 {
				wantMatch, wantRRF = []string{"keyword", "semantic"}, 1/float64(scoring.RRFK+1)+1/float64(scoring.RRFK+2)
			}
			if got := matchOf(r); !reflect.DeepEqual(got, wantMatch) {
				t.Fatalf("%s match = %v, want %v", r["id"], got, wantMatch)
			}
			bd := r["score_breakdown"].(map[string]any)
			if got, want := keysOf(bd), []string{"auth", "edge", "ranks", "rec", "rrf", "sem", "team", "temporal"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("%s breakdown keys = %v, want %v", r["id"], got, want)
			}
			if math.Abs(bd["rrf"].(float64)-wantRRF) > 1e-12 {
				t.Fatalf("%s rrf = %v, want %v", r["id"], bd["rrf"], wantRRF)
			}
			want := scoring.Boost(wantRRF, alphas, bd["rec"].(float64), bd["team"].(float64), bd["temporal"].(float64), bd["auth"].(float64))
			if got := r["score"].(float64); math.Abs(got-want) > 1e-12 {
				t.Fatalf("%s score = %v, want boosted rrf %v", r["id"], got, want)
			}
			scores = append(scores, r["score"].(float64))
		}
		return scores
	}
	before := check(t)
	// The retired hybrid weights are inert: writing them changes nothing.
	if _, err := pool.Exec(context.Background(), `INSERT INTO public.settings(key,value) VALUES
		('graph.weights.kw','0'), ('graph.weights.title','0'), ('graph.weights.hybrid_rec','0')
		ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM public.settings WHERE key IN ('graph.weights.kw','graph.weights.title','graph.weights.hybrid_rec')`)
	})
	after := check(t)
	for i := range before {
		if math.Abs(before[i]-after[i]) > 1e-9 {
			t.Fatalf("score %d changed with the old weights set: %v -> %v", i, before[i], after[i])
		}
	}
}
