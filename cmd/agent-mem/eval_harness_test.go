package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

type testCaseSpec struct {
	ID         string
	Query      string
	Seeds      []string
	LabelledBy string
	Must       []string
	May        []string
	MustNot    []string
}

func jsonList(l []string) string {
	if l == nil {
		l = []string{}
	}
	b, _ := json.Marshal(l)
	return string(b)
}

// writeTestCase writes a case file with the three variants.
func writeTestCase(t *testing.T, dataDir string, s testCaseSpec) string {
	t.Helper()
	if s.LabelledBy == "" {
		s.LabelledBy = "none"
	}
	q, _ := json.Marshal(s.Query)
	seedURL := "slack:CSEED:1.0"
	if len(s.Seeds) > 0 {
		seedURL = s.Seeds[0]
	}
	body := fmt.Sprintf(`id: %s
use_case: UC1
source_thread: https://example.invalid/thread
asker_slack_id: U0
asker_eeid: 1
needs_context: yes
canonical_seeds: []
requests:
  resolve-admin: {body: {seeds: [%q], query: %s, asker_eeid: 0}, headers: {}}
  resolve-asker: {body: {seeds: [%q], query: %s, asker_eeid: 1}, headers: {}}
  search-hybrid: {params: {q: %s, match: hybrid, limit: 10}, headers: {}}
labels:
  resolve: {must: %s, may: %s, must_not: %s}
  search: {must: %s, may: %s, must_not: %s}
label_reasons: {}
labelled_by: %s
checked_by: null
notes: ""
`, s.ID, seedURL, q, seedURL, q, q, jsonList(s.Must), jsonList(s.May), jsonList(s.MustNot), jsonList(s.Must), jsonList(s.May), jsonList(s.MustNot), s.LabelledBy)
	dir := filepath.Join(dataDir, "cases")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, s.ID+".yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEvalCaseStateValidation(t *testing.T) {
	enzo, other := "enzo", "bob"
	valid := evalCase{ID: "ok", LabelledBy: "fable-draft", CheckedBy: &enzo, CanonicalSeeds: []string{"slack:C:1.0"},
		Labels: map[string]evalLabels{"resolve": {Must: []string{"a"}, May: []string{"b"}, MustNot: []string{"c"}}}}
	if err := validateEvalCaseState(valid); err != nil {
		t.Fatalf("valid labelled+checked case rejected: %v", err)
	}
	overlap := valid
	overlap.Labels = map[string]evalLabels{"resolve": {Must: []string{"a"}, MustNot: []string{"a"}}}
	seedInMust := valid
	seedInMust.Labels = map[string]evalLabels{"search": {Must: []string{"slack:C:1.0"}}}
	noneWithLabels := valid
	noneWithLabels.LabelledBy = "none"
	badChecker := valid
	badChecker.CheckedBy = &other
	badLabeller := valid
	badLabeller.LabelledBy = "robot"
	for name, c := range map[string]evalCase{"overlap": overlap, "seed in must": seedInMust, "none with labels": noneWithLabels, "bad checked_by": badChecker, "bad labelled_by": badLabeller} {
		if err := validateEvalCaseState(c); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestRewriteCanonicalSeedsKeepsAnchors(t *testing.T) {
	raw := []byte("id: x\ncanonical_seeds: []\nnotes: &q \"hi\"\n")
	out, err := rewriteCanonicalSeeds(raw, []string{"slack:C:1.0", "jira:PAY-1"})
	if err != nil {
		t.Fatal(err)
	}
	var c evalCase
	if err := yaml.Unmarshal(out, &c); err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.CanonicalSeeds, ",") != "slack:C:1.0,jira:PAY-1" || !strings.Contains(string(out), "&q") {
		t.Fatalf("rewrite lost data: %s", out)
	}
}

func TestReadDotEnvAndKeyPrecedence(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envFile, []byte("# c\nexport AGENT_MEM_API_KEY=\"from-file\"\nDATABASE_URL=postgres://x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_MEM_API_KEY", "")
	if got := evalAPIKey(envFile); got != "from-file" {
		t.Fatalf("key=%q", got)
	}
	t.Setenv("AGENT_MEM_API_KEY", "from-env")
	if got := evalAPIKey(envFile); got != "from-env" {
		t.Fatalf("env must win, got %q", got)
	}
}

// relevanceServer scripts per-case behaviour keyed by the request query.
type relevanceServer struct {
	mu     sync.Mutex
	calls  map[string]int
	auth   string
	nocase map[string]bool
}

func (s *relevanceServer) handler(w http.ResponseWriter, r *http.Request) {
	var q, eeid string
	if r.URL.Path == "/api/graph/resolve" {
		var b struct {
			Query     string
			AskerEEID int `json:"asker_eeid"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		q, eeid = b.Query, fmt.Sprint(b.AskerEEID)
	} else {
		q = r.URL.Query().Get("q")
	}
	if r.Header.Get("X-Graph-No-Side-Effects") != "1" || r.Header.Get("X-Graph-Caller") != "eval" {
		http.Error(w, "missing eval headers", 400)
		return
	}
	s.mu.Lock()
	s.auth = r.Header.Get("Authorization")
	key := r.URL.Path + "|" + eeid + "|" + q + "|" + r.Header.Get("X-Graph-Lead")
	s.calls[key]++
	n := s.calls[key]
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Graph-Build-Rev", "rev-test")
	w.Header().Set("X-Graph-Request-Id", fmt.Sprintf("req-%s-%d", q, n))
	list := func(ids ...string) {
		resolve := r.URL.Path == "/api/graph/resolve"
		var parts, leadIDs []string
		for i, id := range ids {
			if resolve && r.Header.Get("X-Graph-Lead") == "1" {
				if nid := strings.TrimPrefix(id, "L:"); !slices.Contains(leadIDs, nid) {
					leadIDs = append(leadIDs, nid)
				}
			} else if resolve {
				parts = append(parts, fmt.Sprintf(`{"node_id":%q}`, strings.TrimPrefix(id, "L:")))
			} else if strings.HasPrefix(id, "L:") {
				parts = append(parts, fmt.Sprintf(`{"node_id":%q,"linked_via":"jira:X-1"}`, id[2:]))
			} else {
				_ = i
				parts = append(parts, fmt.Sprintf(`{"node_id":%q}`, id))
			}
		}
		arm := ""
		if q == "degraded" && !resolve {
			arm = `,"arm_errors":{"semantic":"boom"}`
		}
		field := "artifacts"
		if !resolve {
			field = "results"
		}
		if resolve && r.Header.Get("X-Graph-Lead") == "1" {
			fmt.Fprintf(w, `{"artifacts":%s}`, testLeadItemsJSON(leadIDs...))
			return
		}
		fmt.Fprintf(w, `{"%s":[%s]%s}`, field, strings.Join(parts, ","), arm)
	}
	switch q {
	case "ok", "degraded":
		list("seed", "must1", "may1", "bad1", "reply1", "L:unk1", "must1")
	case "firstfail":
		// call 1 is the warm-up (different list), call 2 is the first measured run (fails).
		switch {
		case n == 1:
			list("warmup-only")
		case n == 2:
			http.Error(w, "boom", 500)
		default:
			list("must1", "reply1")
		}
	case "unstable":
		if n%2 == 0 {
			list("must1", "may1")
		} else {
			list("may1", "must1")
		}
	case "alwaysfail":
		http.Error(w, "boom", 500)
	case "unlabelled":
		list("reply1", "may1")
	default:
		http.Error(w, "unknown case", 404)
	}
}

func relevanceFixture(t *testing.T) (dataDir string) {
	t.Helper()
	dataDir = filepath.Join(t.TempDir(), "gc")
	lbl := func(id string) testCaseSpec {
		return testCaseSpec{ID: id, Query: id, Seeds: []string{"seed"}, LabelledBy: "fable-draft", Must: []string{"must1", "must2"}, May: []string{"may1"}, MustNot: []string{"bad1"}}
	}
	for _, id := range []string{"ok", "firstfail", "unstable", "degraded", "alwaysfail"} {
		s := lbl(id)
		writeTestCase(t, dataDir, s)
	}
	writeTestCase(t, dataDir, testCaseSpec{ID: "unlabelled", Query: "unlabelled", Seeds: []string{"seed"}})
	// canonical seed for every case is "seed"
	cases, _, err := loadEvalCases(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		out, err := rewriteCanonicalSeeds(c.raw, []string{"seed"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(c.path, out, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	meta := map[string]nodeMeta{
		"seed":   {Type: "slack", Exists: true},
		"must1":  {Type: "slack", Exists: true},
		"may1":   {Type: "slack", Exists: true},
		"bad1":   {Type: "jira", Exists: true},
		"reply1": {Type: "slack", Exists: true, IsReply: true, ThreadRoot: "slack:C:1.0"},
	}
	data, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(dataDir, "node_meta.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return dataDir
}

func TestRelevanceMetricsOnFixedServer(t *testing.T) {
	dataDir := relevanceFixture(t)
	envFile := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envFile, []byte("AGENT_MEM_API_KEY=file-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_MEM_API_KEY", "")
	rs := &relevanceServer{calls: map[string]int{}}
	server := httptest.NewServer(http.HandlerFunc(rs.handler))
	defer server.Close()
	out := filepath.Join(t.TempDir(), "result.json")
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	if err := runRelevance(cmd, relevanceOpts{BaseURL: server.URL, JSONPath: out, DataDir: dataDir, EnvPath: envFile}); err != nil {
		t.Fatalf("%v\n%s", err, buf.String())
	}
	if rs.auth != "Bearer file-key" {
		t.Fatalf("api key not read from .env: %q", rs.auth)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		BuildRev   string   `json:"build_rev"`
		RequestIDs []string `json:"request_ids"`
		PerCase    []struct {
			CaseID      string   `json:"case_id"`
			Variant     string   `json:"variant"`
			Failed      bool     `json:"failed"`
			Degraded    bool     `json:"degraded"`
			Unstable    int      `json:"unstable_distinct_lists"`
			Unlabelled  bool     `json:"unlabelled"`
			Returned    int      `json:"returned"`
			Primary     int      `json:"primary"`
			Linked      int      `json:"linked"`
			NonSeed     []string `json:"non_seed_ids"`
			Precision   *float64 `json:"precision_at_10"`
			Unl10       *int     `json:"unlabelled_at_10"`
			Recall      *float64 `json:"recall"`
			Harmful     *int     `json:"harmful"`
			DupReplies  int      `json:"duplicate_replies"`
			UnknownMeta int      `json:"unknown_meta"`
			Runs        []struct {
				OK bool `json:"ok"`
			} `json:"runs"`
		} `json:"per_case"`
		Aggregate map[string]variantAgg `json:"aggregate"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.PerCase) != 24 {
		t.Fatalf("per_case=%d want 24", len(result.PerCase))
	}
	get := func(id, variant string) int {
		for i, p := range result.PerCase {
			if p.CaseID == id && p.Variant == variant {
				return i
			}
		}
		t.Fatalf("no entry %s/%s", id, variant)
		return -1
	}
	// ok / resolve-asker: seed excluded, duplicate must1 collapsed.
	ok := result.PerCase[get("ok", "resolve-asker")]
	if strings.Join(ok.NonSeed, ",") != "must1,may1,bad1,reply1,unk1" {
		t.Fatalf("non-seed ids=%v", ok.NonSeed)
	}
	// precision = 1 / |top10 \ may| = 1/4; recall = 1/2; harmful 1; unlabelled@10 = reply1, unk1.
	if ok.Precision == nil || *ok.Precision != 0.25 || ok.Recall == nil || *ok.Recall != 0.5 || ok.Harmful == nil || *ok.Harmful != 1 || ok.Unl10 == nil || *ok.Unl10 != 2 {
		t.Fatalf("metrics wrong: %+v", ok)
	}
	if ok.DupReplies != 1 || ok.UnknownMeta != 1 {
		t.Fatalf("dup=%d unknown=%d", ok.DupReplies, ok.UnknownMeta)
	}
	if len(ok.Runs) != 10 {
		t.Fatalf("measured runs=%d", len(ok.Runs))
	}
	// search variant: linked row counted separately, still scored.
	s := result.PerCase[get("ok", "search-hybrid")]
	if s.Primary != 6 || s.Linked != 1 || s.Returned != 7 {
		t.Fatalf("primary=%d linked=%d returned=%d", s.Primary, s.Linked, s.Returned)
	}
	// first measured run failed: ranking uses the first successful measured run, never the warm-up.
	ff := result.PerCase[get("firstfail", "resolve-admin")]
	if ff.Failed || strings.Join(ff.NonSeed, ",") != "must1,reply1" {
		t.Fatalf("firstfail: failed=%v ids=%v", ff.Failed, ff.NonSeed)
	}
	if ff.Runs[0].OK || !ff.Runs[1].OK || ff.Unstable != 0 {
		t.Fatalf("firstfail runs/unstable wrong: %+v unstable=%d", ff.Runs[0], ff.Unstable)
	}
	// unstable: two distinct ordered lists.
	if u := result.PerCase[get("unstable", "resolve-admin")]; u.Unstable != 2 {
		t.Fatalf("unstable=%d want 2", u.Unstable)
	}
	// degraded: only the search variant has arm_errors.
	if !result.PerCase[get("degraded", "search-hybrid")].Degraded || result.PerCase[get("degraded", "resolve-asker")].Degraded {
		t.Fatal("degraded flag wrong")
	}
	// always failing: failed, never scored as empty.
	af := result.PerCase[get("alwaysfail", "resolve-admin")]
	if !af.Failed || af.Precision != nil || af.Recall != nil || af.Returned != 0 {
		t.Fatalf("alwaysfail: %+v", af)
	}
	// unlabelled: no relevance metrics, duplicate replies still counted.
	ul := result.PerCase[get("unlabelled", "resolve-admin")]
	if !ul.Unlabelled || ul.Precision != nil || ul.Recall != nil || ul.Harmful != nil || ul.DupReplies != 1 {
		t.Fatalf("unlabelled: %+v", ul)
	}
	agg := result.Aggregate["resolve-asker"]
	if agg.Cases != 6 || agg.Failed != 1 || agg.Unlabelled != 1 || agg.Scored != 4 || agg.Unstable != 1 {
		t.Fatalf("aggregate: %+v", agg)
	}
	if agg.Samples != 4*10+9 || agg.LowSample || agg.P50Ms == nil || agg.P95Ms == nil {
		t.Fatalf("latency aggregate: samples=%d low=%v", agg.Samples, agg.LowSample)
	}
	if len(result.RequestIDs) < 24*10 || result.BuildRev != "rev-test" {
		t.Fatalf("request ids=%d rev=%s", len(result.RequestIDs), result.BuildRev)
	}
	text := buf.String()
	for _, want := range []string{"FAILED", "unlabelled", "unstable(2 distinct)", "degraded", "failed runs"} {
		if !strings.Contains(text, want) {
			t.Errorf("output missing %q", want)
		}
	}
}

func TestRelevanceRequiresOracleAndReachableServer(t *testing.T) {
	dataDir := relevanceFixture(t)
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	if err := runRelevance(cmd, relevanceOpts{BaseURL: url, DataDir: dataDir, EnvPath: "/nonexistent"}); err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("unreachable server must be a harness error, got %v", err)
	}
	if err := os.Remove(filepath.Join(dataDir, "node_meta.json")); err != nil {
		t.Fatal(err)
	}
	if err := runRelevance(cmd, relevanceOpts{BaseURL: url, DataDir: dataDir, EnvPath: "/nonexistent"}); err == nil || !strings.Contains(err.Error(), "oracle") {
		t.Fatalf("missing oracle file must be a harness error, got %v", err)
	}
}

func TestNearestRank(t *testing.T) {
	s := make([]float64, 20)
	for i := range s {
		s[i] = float64(i + 1)
	}
	if nearestRank(s, 50) != 10 || nearestRank(s, 95) != 19 || nearestRank(s[:1], 95) != 1 {
		t.Fatal("nearest rank wrong")
	}
}

func evalTestDB(t *testing.T) (*pgxpool.Pool, string) {
	return traceTestPool(t)
}

func TestPrepareTxIsReadOnly(t *testing.T) {
	pool, _ := evalTestDB(t)
	ctx := context.Background()
	tx, err := beginEvalTx(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `INSERT INTO graph.nodes(id,type,natural_key,title,scope,metadata,machine_id) VALUES ('slack:CRO:1.0','slack','slack:CRO:1.0','','slack:CRO','{}','test')`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "25006" {
		t.Fatalf("write through prepare tx must fail with 25006, got %v", err)
	}
}

func seedEvalNodes(t *testing.T, pool *pgxpool.Pool, channel string) {
	t.Helper()
	ctx := context.Background()
	clean := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM graph.artifact_bodies WHERE node_id LIKE $1`, "slack:"+channel+":%")
		_, _ = pool.Exec(ctx, `DELETE FROM graph.nodes WHERE id LIKE $1`, "slack:"+channel+":%")
	}
	clean()
	t.Cleanup(clean)
	for _, n := range []struct{ id, title, ts, body string }{
		{"slack:" + channel + ":1.000000", "root title", "1.000000", "root body"},
		{"slack:" + channel + ":2.000000", "reply title", "1.000000", strings.Repeat("r", 400)},
		{"slack:" + channel + ":3.000000", "other thread", "3.000000", "other body"},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO graph.nodes(id,type,natural_key,title,scope,metadata,machine_id) VALUES ($1,'slack',$1,$2,$3,$4::jsonb,'test')`,
			n.id, n.title, "slack:"+channel, fmt.Sprintf(`{"thread_ts":%q}`, n.ts)); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO graph.artifact_bodies(node_id,body_full,machine_id) VALUES ($1,$2,'test')`, n.id, n.body); err != nil {
			t.Fatal(err)
		}
	}
}

// prepareServer returns node ids by case query; swap replaces them later.
type prepareServer struct {
	mu   sync.Mutex
	byQ  map[string][]string
	fail map[string]bool
	// leadRoot, per query, is the id the lead response lists first (its hop-0 item).
	leadRoot map[string]string
}

// testLeadItemsJSON renders a valid lead-mode artifacts array: the first id is
// a seed (hop 0), the rest hop 1; every item has the required lead fields.
func testLeadItemsJSON(ids ...string) string {
	parts := make([]string, 0, len(ids))
	for i, id := range ids {
		hop := 1
		if i == 0 {
			hop = 0
		}
		parts = append(parts, fmt.Sprintf(`{"node_id":%q,"hop":%d,"links":[],"authored_by_bot":"human","title":"title of %s","lead_summary":"summary of %s","author":"","author_slack_id":"","last_activity_at":"2026-10-01T00:00:00Z"}`, id, hop, id, id))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func (p *prepareServer) handler(w http.ResponseWriter, r *http.Request) {
	var q string
	resolve := r.URL.Path == "/api/graph/resolve"
	if resolve {
		var b struct{ Query string }
		_ = json.NewDecoder(r.Body).Decode(&b)
		q = b.Query
	} else {
		q = r.URL.Query().Get("q")
	}
	p.mu.Lock()
	ids, fail := p.byQ[q], p.fail[q+"|"+r.URL.Path]
	leadIDs := ids
	if first := p.leadRoot[q]; first != "" {
		leadIDs = []string{first}
		for _, id := range ids {
			if id != first {
				leadIDs = append(leadIDs, id)
			}
		}
	}
	p.mu.Unlock()
	if fail {
		http.Error(w, "boom", 500)
		return
	}
	w.Header().Set("X-Graph-Request-Id", "req-"+q)
	w.Header().Set("X-Graph-Build-Rev", "rev-prep")
	var parts []string
	for _, id := range ids {
		parts = append(parts, fmt.Sprintf(`{"node_id":%q}`, id))
	}
	if resolve && r.Header.Get("X-Graph-Lead") == "1" {
		fmt.Fprintf(w, `{"artifacts":%s}`, testLeadItemsJSON(leadIDs...))
	} else if resolve {
		fmt.Fprintf(w, `{"artifacts":[%s]}`, strings.Join(parts, ","))
	} else {
		fmt.Fprintf(w, `{"results":[%s]}`, strings.Join(parts, ","))
	}
}

func TestPrepareWritesOraclesAndLabelSheetsUseSavedReplay(t *testing.T) {
	pool, _ := evalTestDB(t)
	seedEvalNodes(t, pool, "CPREP")
	root, reply, other := "slack:CPREP:1.000000", "slack:CPREP:2.000000", "slack:CPREP:3.000000"
	ps := &prepareServer{byQ: map[string][]string{
		"qa": {reply, root, "jira:GONE-1"},
		"qb": {other, root},
	}, fail: map[string]bool{}, leadRoot: map[string]string{"qa": root}}
	server := httptest.NewServer(http.HandlerFunc(ps.handler))
	defer server.Close()

	dataDir := filepath.Join(t.TempDir(), "gc")
	// case qa's seed is the reply: canonicalisation must promote it to the root.
	writeTestCase(t, dataDir, testCaseSpec{ID: "qa", Query: "qa", Seeds: []string{reply}})
	writeTestCase(t, dataDir, testCaseSpec{ID: "qb", Query: "qb", Seeds: []string{other}})
	t.Setenv("AGENT_MEM_API_KEY", "")
	var buf bytes.Buffer
	if err := runEvalPrepare(context.Background(), &buf, evalPrepareOpts{Snapshot: filepath.Join(t.TempDir(), "snap"), BaseURL: server.URL, DataDir: dataDir, EnvPath: "/nonexistent", Pool: pool}); err != nil {
		t.Fatalf("%v\n%s", err, buf.String())
	}

	cases, sum, err := loadEvalCases(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cases[0].CanonicalSeeds, ",") != root || strings.Join(cases[1].CanonicalSeeds, ",") != other {
		t.Fatalf("canonical seeds: %v / %v", cases[0].CanonicalSeeds, cases[1].CanonicalSeeds)
	}
	var meta map[string]nodeMeta
	data, err := os.ReadFile(filepath.Join(dataDir, "node_meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	if !meta[reply].IsReply || meta[reply].ThreadRoot != root || meta[root].IsReply || !meta[root].Exists || meta["jira:GONE-1"].Exists || meta["jira:GONE-1"].Type != "jira" {
		t.Fatalf("node_meta wrong: %+v", meta)
	}
	var replay replayFile
	data, _ = os.ReadFile(filepath.Join(dataDir, "replay.json"))
	if err := json.Unmarshal(data, &replay); err != nil {
		t.Fatal(err)
	}
	if replay.CasesSHA256 != sum || replay.BuildRev != "rev-prep" || strings.Join(replay.Cases["qa"].Variants["search-hybrid"].Returned, ",") != reply+","+root+",jira:GONE-1" {
		t.Fatalf("replay wrong: %+v", replay)
	}

	// Later responses change; sheets must still list the saved replay ids.
	ps.mu.Lock()
	ps.byQ["qa"], ps.byQ["qb"] = []string{"jira:NEW-1"}, []string{"jira:NEW-2"}
	ps.mu.Unlock()
	outDir := filepath.Join(t.TempDir(), "sheets")
	if err := runLabelSheet(context.Background(), &buf, labelSheetOpts{All: true, OutDir: outDir, DataDir: dataDir, EnvPath: "/nonexistent", Pool: pool}); err != nil {
		t.Fatal(err)
	}
	qa, _ := os.ReadFile(filepath.Join(outDir, "qa.md"))
	qb, _ := os.ReadFile(filepath.Join(outDir, "qb.md"))
	if !strings.Contains(string(qa), "## [r1] "+reply) || strings.Contains(string(qa), "] "+root) || !strings.Contains(string(qa), "## [r2] jira:GONE-1") || strings.Contains(string(qa), "NEW-") {
		t.Fatalf("qa sheet wrong:\n%s", qa)
	}
	if !strings.Contains(string(qa), "is_reply: true") || !strings.Contains(string(qa), "reply title") || strings.Contains(string(qa), strings.Repeat("r", 301)) || !strings.Contains(string(qa), strings.Repeat("r", 300)) {
		t.Fatalf("qa sheet content wrong:\n%s", qa)
	}
	if !strings.Contains(string(qb), "## [r1] "+root) || strings.Contains(string(qb), "] "+other) || strings.Contains(string(qb), "NEW-") {
		t.Fatalf("qb sheet wrong:\n%s", qb)
	}

	// Refusals.
	if err := runLabelSheet(context.Background(), &buf, labelSheetOpts{All: true, OutDir: filepath.Join(".", "sheets-in-repo"), DataDir: dataDir, Pool: pool}); err == nil || !strings.Contains(err.Error(), "outside the repo") {
		t.Fatalf("out inside repo must be refused: %v", err)
	}
	if err := runLabelSheet(context.Background(), &buf, labelSheetOpts{CaseID: "nope", OutDir: outDir, DataDir: dataDir, Pool: pool}); err == nil {
		t.Fatal("unknown case must be refused")
	}
	var rf replayFile
	data, _ = os.ReadFile(filepath.Join(dataDir, "replay.json"))
	_ = json.Unmarshal(data, &rf)
	delete(rf.Cases, "qb")
	trimmed, _ := json.Marshal(rf)
	if err := os.WriteFile(filepath.Join(dataDir, "replay.json"), trimmed, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runLabelSheet(context.Background(), &buf, labelSheetOpts{CaseID: "qb", OutDir: outDir, DataDir: dataDir, Pool: pool}); err == nil || !strings.Contains(err.Error(), "absent") {
		t.Fatalf("case absent from replay must be refused: %v", err)
	}
	// Mismatched hash: edit a case file after prepare.
	if err := os.WriteFile(cases[0].path, append(cases[0].raw, []byte("# edited\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runLabelSheet(context.Background(), &buf, labelSheetOpts{CaseID: "qa", OutDir: outDir, DataDir: dataDir, Pool: pool}); err == nil || !strings.Contains(err.Error(), "cases_sha256") {
		t.Fatalf("mismatched cases_sha256 must be refused: %v", err)
	}
	// Missing replay.json.
	if err := os.Remove(filepath.Join(dataDir, "replay.json")); err != nil {
		t.Fatal(err)
	}
	if err := runLabelSheet(context.Background(), &buf, labelSheetOpts{All: true, OutDir: outDir, DataDir: dataDir, Pool: pool}); err == nil || !strings.Contains(err.Error(), "replay.json") {
		t.Fatalf("missing replay.json must be refused: %v", err)
	}
}

func snapshotDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			b, _ := os.ReadFile(p)
			out[p] = string(b)
		}
		return nil
	})
	return out
}

func sameSnapshot(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestPrepareNeverWritesPartialFiles(t *testing.T) {
	pool, dsn := evalTestDB(t)
	seedEvalNodes(t, pool, "CPART")
	root := "slack:CPART:1.000000"
	ps := &prepareServer{byQ: map[string][]string{"qa": {root}, "qb": {root}}, fail: map[string]bool{"qb|/api/graph/search": true}}
	server := httptest.NewServer(http.HandlerFunc(ps.handler))
	defer server.Close()
	dataDir := filepath.Join(t.TempDir(), "gc")
	writeTestCase(t, dataDir, testCaseSpec{ID: "qa", Query: "qa", Seeds: []string{root}})
	writeTestCase(t, dataDir, testCaseSpec{ID: "qb", Query: "qb", Seeds: []string{root}})
	before := snapshotDir(t, dataDir)
	t.Setenv("AGENT_MEM_API_KEY", "")

	// A replay failing after the first case finished (and after DB work).
	err := runEvalPrepare(context.Background(), &bytes.Buffer{}, evalPrepareOpts{Snapshot: filepath.Join(t.TempDir(), "snap"), BaseURL: server.URL, DataDir: dataDir, EnvPath: "/nonexistent", Pool: pool})
	if err == nil {
		t.Fatal("replay failure must fail prepare")
	}
	if !sameSnapshot(before, snapshotDir(t, dataDir)) {
		t.Fatalf("files changed after failed replay: %v", snapshotDir(t, dataDir))
	}

	// A database error (closed pool) also leaves everything untouched.
	ps.mu.Lock()
	ps.fail = map[string]bool{}
	ps.mu.Unlock()
	closed, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	if err := runEvalPrepare(context.Background(), &bytes.Buffer{}, evalPrepareOpts{Snapshot: filepath.Join(t.TempDir(), "snap"), BaseURL: server.URL, DataDir: dataDir, EnvPath: "/nonexistent", Pool: closed}); err == nil {
		t.Fatal("database error must fail prepare")
	}
	if !sameSnapshot(before, snapshotDir(t, dataDir)) {
		t.Fatalf("files changed after database error: %v", snapshotDir(t, dataDir))
	}
	if _, err := os.Stat(filepath.Join(dataDir, "node_meta.json")); !os.IsNotExist(err) {
		t.Fatal("node_meta.json must not exist")
	}
}

func TestPrepareRefreshAddsMissingIDs(t *testing.T) {
	pool, _ := evalTestDB(t)
	seedEvalNodes(t, pool, "CREF")
	root, other := "slack:CREF:1.000000", "slack:CREF:3.000000"
	ps := &prepareServer{byQ: map[string][]string{"qa": {root}}, fail: map[string]bool{}}
	server := httptest.NewServer(http.HandlerFunc(ps.handler))
	defer server.Close()
	dataDir := filepath.Join(t.TempDir(), "gc")
	writeTestCase(t, dataDir, testCaseSpec{ID: "qa", Query: "qa", Seeds: []string{root}})
	t.Setenv("AGENT_MEM_API_KEY", "")
	run := func(refresh bool) {
		if err := runEvalPrepare(context.Background(), &bytes.Buffer{}, evalPrepareOpts{Snapshot: filepath.Join(t.TempDir(), "snap"), BaseURL: server.URL, Refresh: refresh, DataDir: dataDir, EnvPath: "/nonexistent", Pool: pool}); err != nil {
			t.Fatal(err)
		}
	}
	run(false)
	// Plant a stale entry; refresh must keep it and add the new returned id.
	metaPath := filepath.Join(dataDir, "node_meta.json")
	var meta map[string]nodeMeta
	data, _ := os.ReadFile(metaPath)
	_ = json.Unmarshal(data, &meta)
	meta[root] = nodeMeta{Type: "stale", Exists: true}
	data, _ = json.Marshal(meta)
	_ = os.WriteFile(metaPath, data, 0o644)
	ps.mu.Lock()
	ps.byQ["qa"] = []string{root, other}
	ps.mu.Unlock()
	run(true)
	data, _ = os.ReadFile(metaPath)
	meta = map[string]nodeMeta{}
	_ = json.Unmarshal(data, &meta)
	if meta[root].Type != "stale" || !meta[other].Exists {
		t.Fatalf("refresh must keep existing and add missing: %+v", meta)
	}
}

func TestRelevanceMarksLowSample(t *testing.T) {
	dataDir := relevanceFixture(t)
	for _, id := range []string{"firstfail", "unstable", "degraded", "alwaysfail", "unlabelled"} {
		if err := os.Remove(filepath.Join(dataDir, "cases", id+".yaml")); err != nil {
			t.Fatal(err)
		}
	}
	rs := &relevanceServer{calls: map[string]int{}}
	server := httptest.NewServer(http.HandlerFunc(rs.handler))
	defer server.Close()
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	if err := runRelevance(cmd, relevanceOpts{BaseURL: server.URL, DataDir: dataDir, EnvPath: "/nonexistent"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(buf.String(), "samples=10 low sample"); got != 4 {
		t.Fatalf("low sample marks=%d want 4\n%s", got, buf.String())
	}
}
