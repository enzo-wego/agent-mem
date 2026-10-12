package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/gemini"
	graphtrace "github.com/agent-mem/agent-mem/internal/graph/trace"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// R1a acceptance 1 (flag off / no header behave exactly like main) and 2
// (no-trace switch). Prefix: leadA.
//
// Goldens: the eight resolve/search goldens recorded by TestResolveSearchCompat-
// ibility are reused as they are. Every other golden is named lead_a_*.golden
// .json and was recorded from main (8e97d74) with UPDATE_GOLDEN=1 against this
// same fixture; the fixture here only adds rows that no existing golden touches.

// leadAGemini satisfies GeminiClient for Deps; only embedding is used by the
// read routes.
type leadAGemini struct{ GeminiClient }

func (leadAGemini) Embed(context.Context, string) ([]float32, error) {
	return make([]float32, GraphEmbeddingDims), nil
}
func (leadAGemini) EmbedWithOptions(context.Context, string, gemini.EmbedOptions) ([]float32, error) {
	return make([]float32, GraphEmbeddingDims), nil
}

// leadAQueryCounter counts, by SQL text, the queries that only lead mode runs.
type leadAQueryCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

// Every lead-only SQL carries a /* lead:<name> */ tag (aggregate, roots,
// roots_eligible, epic, enrich, misses, display, links); the counter counts each
// tag by name. The two table/shape markers stay as an independent check.
var leadAMarkers = []string{"graph.answer_bots", "unnest($1::text[], $2::text[])"}

var leadATagRe = regexp.MustCompile(`/\* (lead:[a-z_]+) \*/`)

func (c *leadAQueryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range leadAMarkers {
		if strings.Contains(d.SQL, m) {
			c.counts[m]++
		}
	}
	for _, m := range leadATagRe.FindAllStringSubmatch(d.SQL, -1) {
		c.counts[m[1]]++
	}
	return ctx
}
func (*leadAQueryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *leadAQueryCounter) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts = map[string]int{}
}

func (c *leadAQueryCounter) snapshot() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]int{}
	for k, v := range c.counts {
		out[k] = v
	}
	return out
}

const (
	leadAMember = "lead-a-member@example.com"
	leadAPlain  = "lead-a-plain@example.com"
)

var leadAAskers = map[string]string{"member": leadAMember, "plain": leadAPlain}

// leadAEnv is the loaded fixture plus a traced pool and one mounted router per
// flag value (Mount, exactly as the worker wires it).
type leadAEnv struct {
	pool    *pgxpool.Pool
	counter *leadAQueryCounter
	routers map[bool]chi.Router
}

func leadASetup(t *testing.T) *leadAEnv {
	t.Helper()
	base := compatibilityPool(t) // skips without DATABASE_URL; requires agentmem_test
	resetEpicACL(t, base)
	fixture, err := os.ReadFile(compatibilityFixture)
	if err != nil {
		t.Fatal(err)
	}
	aclExec(t, base, string(fixture))
	aclExec(t, base, `DELETE FROM public.settings WHERE key LIKE 'graph.weights.%' OR key LIKE 'graph.boost.alpha.%' OR key = 'graph.search.graph_arm_weight'`)
	// Rows that no pre-existing golden touches: an epic with a private and a
	// public member (epic/node/neighbors goldens) and two askers.
	aclNode(t, base, "jira:LEADA-EPIC", "jira", "", `{}`, "2026-07-01")
	aclMember(t, base, "jira:LEADA-EPIC", "LEADA-EPIC", viaEpicSelf)
	aclNode(t, base, "jira:LEADA-2", "jira", "", `{}`, "2026-09-10")
	aclMember(t, base, "jira:LEADA-2", "LEADA-EPIC", viaEpicSelf)
	aclNode(t, base, "slack:CLEADA:1.0", "slack", "slack:CLEADA", `{"thread_ts":"1.0"}`, "2026-07-01")
	aclMember(t, base, "slack:CLEADA:1.0", "LEADA-EPIC", viaKey)
	aclNode(t, base, "slack:CLEADA:1.5", "slack", "slack:CLEADA", `{"thread_ts":"1.0"}`, "2026-07-02")
	aclMember(t, base, "slack:CLEADA:1.5", "LEADA-EPIC", viaKey)
	aclPerson(t, base, 7101, leadAMember, "slack:CGC0", "slack:CLEADA")
	aclPerson(t, base, 7102, leadAPlain)
	// aclNode leaves updated_at/first_seen_at at now(); pin them so the goldens are stable.
	aclExec(t, base, `UPDATE graph.nodes SET updated_at = created_at, first_seen_at = created_at WHERE id LIKE '%LEADA%'`)

	oldLocal := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = oldLocal })
	fixed := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	oldNow := nowFunc
	nowFunc = func() time.Time { return fixed }
	t.Cleanup(func() { nowFunc = oldNow })

	cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	counter := &leadAQueryCounter{counts: map[string]int{}}
	cfg.ConnConfig.Tracer = counter
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	env := &leadAEnv{pool: pool, counter: counter, routers: map[bool]chi.Router{}}
	for _, enabled := range []bool{false, true} {
		recorder := graphtrace.NewRecorder(discardTraceStore{}, graphtrace.Options{})
		t.Cleanup(func() { recorder.Close(time.Second) })
		r := chi.NewRouter()
		Mount(r, Deps{DB: pool, Gemini: leadAGemini{}, TraceRecorder: recorder, BuildRev: "lead-a", LeadModeEnabled: enabled})
		env.routers[enabled] = r
	}
	return env
}

// leadACase is one request replayed for every caller and every flag/header combination.
type leadACase struct {
	name, route, method, target, body string
	callers                           []string
	existing                          bool // golden already recorded by TestResolveSearchCompatibility
}

// traced routes carry X-Graph-Request-Id and a request_id member; the others never do.
func (c leadACase) traced() bool { return c.route == "resolve" || c.route == "search" }

func (c leadACase) goldenName(caller string) string {
	if c.existing {
		return c.name
	}
	return "lead_a_" + c.name + "-" + caller
}

func leadACases() []leadACase {
	admin := []string{"admin"}
	adminMember := []string{"admin", "member"}
	all := []string{"admin", "member", "plain"}
	const hit = `{"seeds":["slack:CGC0:100.000001"],"query":"phase zero","depth":2`
	return []leadACase{
		{"resolve-hit", "resolve", "POST", "/api/graph/resolve", hit + `,"include_bodies":true}`, admin, true},
		{"resolve-reply-seed", "resolve", "POST", "/api/graph/resolve", `{"seeds":["slack:CGC0:101.000001"],"depth":2}`, admin, true},
		{"resolve-empty", "resolve", "POST", "/api/graph/resolve", `{"seeds":["jira:UNKNOWN"],"depth":2}`, admin, true},
		{"resolve-cache-miss", "resolve", "POST", "/api/graph/resolve", `{"seeds":["jira:GC0-MISS"],"depth":2}`, admin, true},
		{"resolve-invalid", "resolve", "POST", "/api/graph/resolve", `{}`, admin, true},
		{"search-ordinary-hit", "search", "GET", "/api/graph/search?q=phase+zero&limit=10", "", admin, true},
		{"search-hybrid-hit", "search", "GET", "/api/graph/search?q=phase+zero&match=hybrid&limit=10", "", admin, true},
		{"search-empty", "search", "GET", "/api/graph/search?q=unfindabletoken&limit=10", "", admin, true},

		{"resolve-hit-bodies", "resolve", "POST", "/api/graph/resolve", hit + `,"include_bodies":true}`, []string{"member"}, false},
		{"resolve-hit-nobodies", "resolve", "POST", "/api/graph/resolve", hit + `}`, adminMember, false},
		{"resolve-reply-seed", "resolve", "POST", "/api/graph/resolve", `{"seeds":["slack:CGC0:101.000001"],"depth":2}`, []string{"member"}, false},
		{"resolve-cache-miss", "resolve", "POST", "/api/graph/resolve", `{"seeds":["jira:GC0-MISS"],"depth":2}`, []string{"member"}, false},
		{"search-ordinary-hit", "search", "GET", "/api/graph/search?q=phase+zero&limit=10", "", []string{"member"}, false},
		{"search-hybrid-hit", "search", "GET", "/api/graph/search?q=phase+zero&match=hybrid&limit=10", "", []string{"member"}, false},

		{"node-root", "node", "GET", "/api/graph/node?id=slack:CGC0:100.000001", "", adminMember, false},
		{"node-reply", "node", "GET", "/api/graph/node?id=slack:CGC0:101.000001", "", adminMember, false},
		{"node-jira", "node", "GET", "/api/graph/node?id=jira:GC0-1", "", adminMember, false},
		{"node-private-reply", "node", "GET", "/api/graph/node?id=slack:CLEADA:1.5", "", all, false},
		{"node-missing", "node", "GET", "/api/graph/node?id=slack:CGC0:999.000001", "", adminMember, false},

		{"epic", "epic", "GET", "/api/graph/epic/LEADA-EPIC", "", all, false},
		{"epic-unknown", "epic", "GET", "/api/graph/epic/NOPE-1", "", adminMember, false},

		{"neighbors-root", "neighbors", "GET", "/api/graph/node/slack:CGC0:100.000001/neighbors", "", adminMember, false},
		{"neighbors-jira", "neighbors", "GET", "/api/graph/node/jira:GC0-1/neighbors", "", adminMember, false},
		{"neighbors-private", "neighbors", "GET", "/api/graph/node/slack:CLEADA:1.0/neighbors", "", all, false},
	}
}

func leadAGoldenPath(name string) string {
	return filepath.Join("testdata", "compatibility", name+".golden.json")
}

// leadAOne performs one request against the mounted router for enabled.
func leadAOne(t *testing.T, env *leadAEnv, enabled bool, tc leadACase, caller string, extraHeaders map[string]string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := adminRequest(tc.method, tc.target, bytes.NewBufferString(body))
	if caller != "admin" {
		setAsker(req, leadAAskers[caller])
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	env.routers[enabled].ServeHTTP(w, req)
	return w
}

// leadAHeaderSig is the response header set with the two intended variables
// factored out: the request id value (only its presence matters) and
// X-Graph-Lead (asserted on its own).
func leadAHeaderSig(h http.Header) string {
	var lines []string
	for k, vs := range h {
		switch http.CanonicalHeaderKey(k) {
		case "X-Graph-Lead":
			continue
		case "X-Graph-Request-Id":
			lines = append(lines, "X-Graph-Request-Id: <present>")
			continue
		}
		for _, v := range vs {
			lines = append(lines, k+": "+v)
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func leadAEnvelope(t *testing.T, tc leadACase, w *httptest.ResponseRecorder) compatibilityEnvelope {
	t.Helper()
	body := w.Body.String()
	if tc.traced() {
		body = normalizeCompatibilityBody(t, w.Body.Bytes())
	}
	return compatibilityEnvelope{Status: w.Code, ContentType: w.Header().Get("Content-Type"), Body: body}
}

// leadARequestID asserts the request-id contract of w: traced routes carry a
// ctx_ id in the header, and a 2xx JSON body carries the same id as
// request_id; untraced routes carry neither.
func leadARequestID(t *testing.T, tc leadACase, w *httptest.ResponseRecorder) {
	t.Helper()
	id := w.Header().Get("X-Graph-Request-Id")
	if !tc.traced() {
		if id != "" || strings.Contains(w.Body.String(), "request_id") {
			t.Fatalf("%s: untraced route carries a request id: header=%q body=%s", tc.name, id, w.Body.String())
		}
		return
	}
	if !strings.HasPrefix(id, "ctx_") {
		t.Fatalf("%s: X-Graph-Request-Id=%q", tc.name, id)
	}
	if w.Code >= 200 && w.Code < 300 && json.Valid(w.Body.Bytes()) {
		var top map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &top); err != nil || top["request_id"] != id {
			t.Fatalf("%s: body request_id=%v header=%q", tc.name, top["request_id"], id)
		}
	} else if strings.Contains(w.Body.String(), "request_id") {
		t.Fatalf("%s: error body carries request_id: %s", tc.name, w.Body.String())
	}
}

type leadAConfig struct {
	enabled bool
	header  string // "" = absent
}

func (c leadAConfig) String() string {
	return fmt.Sprintf("env=%v/header=%q", c.enabled, c.header)
}

// leadAInactiveConfigs: every flag/header combination except the one that
// activates lead mode (env true + header 1).
func leadAInactiveConfigs() []leadAConfig {
	return []leadAConfig{
		{false, ""}, {false, "1"}, {false, "0"}, {false, "true"}, {false, "2"},
		{true, ""}, {true, "0"}, {true, "true"}, {true, "2"},
	}
}

func (c leadAConfig) headers() map[string]string {
	if c.header == "" {
		return nil
	}
	return map[string]string{"X-Graph-Lead": c.header}
}

// leadHasMode: resolve, node and search have a lead mode; epic and neighbors do
// not, so they never answer X-Graph-Lead: off and ignore the header entirely.
func (c leadACase) leadHasMode() bool {
	return c.route == "resolve" || c.route == "node" || c.route == "search"
}

func (c leadAConfig) wantLeadHeader(tc leadACase) string {
	if tc.leadHasMode() && !c.enabled && c.header == "1" {
		return "off"
	}
	return ""
}

// configs returns the flag/header combinations a case must answer exactly like
// main for: every inactive one, plus env true with X-Graph-Lead: 1 for the
// routes that have no lead mode.
func (c leadACase) configs() []leadAConfig {
	cfgs := leadAInactiveConfigs()
	if !c.leadHasMode() {
		cfgs = append(cfgs, leadAConfig{true, "1"})
	}
	return cfgs
}

func leadAReadGolden(t *testing.T, name string) compatibilityEnvelope {
	t.Helper()
	data, err := os.ReadFile(leadAGoldenPath(name))
	if err != nil {
		t.Fatalf("golden %s: %v (record from main with UPDATE_GOLDEN=1)", name, err)
	}
	var want compatibilityEnvelope
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	return want
}

// TestLeadAInactiveRequestsMatchMain: with the flag off, and with the flag on
// but no (or another) X-Graph-Lead value, every route answers exactly as main
// did: status, content type, raw JSON after the compatibility normalisation
// (traced routes only), headers, and no lead-only query runs. With the flag off
// and X-Graph-Lead: 1 the only difference is the response header
// X-Graph-Lead: off.
func TestLeadAInactiveRequestsMatchMain(t *testing.T) {
	env := leadASetup(t)
	update := os.Getenv("UPDATE_GOLDEN") == "1"
	for _, tc := range leadACases() {
		for _, caller := range tc.callers {
			t.Run(tc.name+"/"+caller, func(t *testing.T) {
				if update && !tc.existing {
					w := leadAOne(t, env, false, tc, caller, nil, tc.body)
					got := leadAEnvelope(t, tc, w)
					data, _ := json.MarshalIndent(got, "", "  ")
					if err := os.WriteFile(leadAGoldenPath(tc.goldenName(caller)), append(data, '\n'), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				want := leadAReadGolden(t, tc.goldenName(caller))
				assertNoResolveScoreTies(t, want.Body)
				var baseSig string
				for i, cfg := range tc.configs() {
					env.counter.reset()
					w := leadAOne(t, env, cfg.enabled, tc, caller, cfg.headers(), tc.body)
					if got := leadAEnvelope(t, tc, w); got != want {
						t.Fatalf("%v: response differs from main\ngot:  %s\nwant: %s", cfg, compatibilityJSON(got), compatibilityJSON(want))
					}
					if got := w.Header().Get("X-Graph-Lead"); got != cfg.wantLeadHeader(tc) {
						t.Fatalf("%v: X-Graph-Lead=%q want %q", cfg, got, cfg.wantLeadHeader(tc))
					}
					leadARequestID(t, tc, w)
					if i == 0 {
						baseSig = leadAHeaderSig(w.Header())
					} else if sig := leadAHeaderSig(w.Header()); sig != baseSig {
						t.Fatalf("%v: headers differ from the no-header baseline\ngot:\n%s\nwant:\n%s", cfg, sig, baseSig)
					}
					if c := env.counter.snapshot(); len(c) != 0 {
						t.Fatalf("%v: lead-only queries ran on an inactive request: %v", cfg, c)
					}
				}
			})
		}
	}
}

// TestLeadAActiveRequestsRunLeadQueries is the positive control for the query
// counter: with env true and X-Graph-Lead: 1 the lead-only queries do run.
func TestLeadAActiveRequestsRunLeadQueries(t *testing.T) {
	env := leadASetup(t)
	for _, tc := range leadACases() {
		switch tc.name {
		case "resolve-hit-nobodies", "search-ordinary-hit", "search-hybrid-hit", "node-root":
		default:
			continue
		}
		if tc.existing {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			env.counter.reset()
			w := leadAOne(t, env, true, tc, "admin", map[string]string{"X-Graph-Lead": "1"}, tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if got := w.Header().Get("X-Graph-Lead"); got != "" {
				t.Fatalf("active request carries X-Graph-Lead=%q", got)
			}
			c := env.counter.snapshot()
			for _, m := range leadAMarkers {
				if c[m] == 0 {
					t.Errorf("active %s did not run a query containing %q (counts %v)", tc.name, m, c)
				}
			}
			for _, tag := range []string{"lead:roots", "lead:enrich"} {
				if c[tag] == 0 {
					t.Errorf("active %s did not run %s (counts %v)", tc.name, tag, c)
				}
			}
			if tc.name == "resolve-hit-nobodies" {
				for _, tag := range []string{"lead:misses", "lead:display"} {
					if c[tag] == 0 {
						t.Errorf("active %s did not run %s (counts %v)", tc.name, tag, c)
					}
				}
			}
			if c["lead:aggregate"] != 1 {
				t.Errorf("aggregate queries = %d, want exactly 1", c["lead:aggregate"])
			}
			if !strings.Contains(w.Body.String(), `"last_activity_at"`) {
				t.Errorf("active response has no lead fields: %s", w.Body.String())
			}
		})
	}
}

// leadAResolveBody is the lead-hit resolve body with extra raw JSON members.
func leadAResolveBody(extra string) string {
	return `{"seeds":["slack:CGC0:100.000001"],"query":"phase zero","depth":2` + extra + `}`
}

// TestLeadAMaxItemsMatrix: junk max_items is accepted unchanged on every
// inactive request (it is not even decoded) and only env true + X-Graph-Lead: 1
// validates it.
func TestLeadAMaxItemsMatrix(t *testing.T) {
	env := leadASetup(t)
	resolve := leadACase{name: "resolve-max-items", route: "resolve", method: "POST", target: "/api/graph/resolve"}
	junk := []string{`0`, `-1`, `101`, `"x"`, `1.5`, `true`, `"5"`, `[1]`, `{}`, `null`, `1`, `100`}
	for _, caller := range []string{"admin", "member"} {
		t.Run("inactive/"+caller, func(t *testing.T) {
			base := leadAOne(t, env, false, resolve, caller, nil, leadAResolveBody(""))
			if base.Code != http.StatusOK {
				t.Fatalf("baseline status=%d body=%s", base.Code, base.Body.String())
			}
			want := leadAEnvelope(t, resolve, base)
			for _, v := range junk {
				for _, cfg := range leadAInactiveConfigs() {
					env.counter.reset()
					w := leadAOne(t, env, cfg.enabled, resolve, caller, cfg.headers(), leadAResolveBody(`,"max_items":`+v))
					if got := leadAEnvelope(t, resolve, w); got != want {
						t.Fatalf("max_items=%s %v: inactive request differs from main (junk must be ignored)\ngot:  %s\nwant: %s", v, cfg, compatibilityJSON(got), compatibilityJSON(want))
					}
					if got := w.Header().Get("X-Graph-Lead"); got != cfg.wantLeadHeader(resolve) {
						t.Fatalf("max_items=%s %v: X-Graph-Lead=%q want %q", v, cfg, got, cfg.wantLeadHeader(resolve))
					}
					leadARequestID(t, resolve, w)
					if c := env.counter.snapshot(); len(c) != 0 {
						t.Fatalf("max_items=%s %v: lead-only queries ran: %v", v, cfg, c)
					}
				}
			}
		})
	}

	lead := map[string]string{"X-Graph-Lead": "1"}
	for _, caller := range []string{"admin", "member"} {
		t.Run("active/"+caller, func(t *testing.T) {
			for _, v := range []string{`0`, `-1`, `101`, `"x"`, `1.5`, `true`, `"5"`, `[1]`, `{}`} {
				w := leadAOne(t, env, true, resolve, caller, lead, leadAResolveBody(`,"max_items":`+v))
				if w.Code != http.StatusBadRequest || strings.TrimSpace(w.Body.String()) != "bad_max_items" {
					t.Fatalf("max_items=%s: status=%d body=%q, want 400 bad_max_items", v, w.Code, w.Body.String())
				}
			}
			bodyOf := func(raw string) (compatibilityEnvelope, map[string]any) {
				t.Helper()
				w := leadAOne(t, env, true, resolve, caller, lead, raw)
				if w.Code != http.StatusOK {
					t.Fatalf("%s: status=%d body=%s", raw, w.Code, w.Body.String())
				}
				leadARequestID(t, resolve, w)
				var top map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &top); err != nil {
					t.Fatal(err)
				}
				return leadAEnvelope(t, resolve, w), top
			}
			omitted, topOmitted := bodyOf(leadAResolveBody(""))
			arts, _ := topOmitted["artifacts"].([]any)
			if len(arts) == 0 {
				t.Fatalf("lead resolve returned no artifacts: %s", omitted.Body)
			}
			for _, a := range arts {
				m := a.(map[string]any)
				for _, f := range []string{"lead_summary", "last_activity_at", "author_slack_id", "authored_by_bot", "channel_id"} {
					if _, ok := m[f]; !ok {
						t.Fatalf("artifact %v lacks lead field %q", m["node_id"], f)
					}
				}
			}
			for _, v := range []string{`null`, `100`} {
				got, _ := bodyOf(leadAResolveBody(`,"max_items":` + v))
				if got != omitted {
					t.Fatalf("max_items=%s differs from omitted\ngot:  %s\nwant: %s", v, compatibilityJSON(got), compatibilityJSON(omitted))
				}
			}
			_, topOne := bodyOf(leadAResolveBody(`,"max_items":1`))
			if one, _ := topOne["artifacts"].([]any); len(one) != 1 {
				t.Fatalf("max_items=1 returned %d artifacts", len(one))
			}
		})
	}

	// The gate runs before the body is decoded: a malformed body is rejected the
	// same way in every configuration (inactive: exactly as main).
	for _, cfg := range append(leadAInactiveConfigs(), leadAConfig{true, "1"}) {
		w := leadAOne(t, env, cfg.enabled, resolve, "admin", cfg.headers(), `{"seeds":`)
		if w.Code != http.StatusBadRequest || strings.TrimSpace(w.Body.String()) != "bad request" {
			t.Fatalf("%v: malformed body status=%d body=%q", cfg, w.Code, w.Body.String())
		}
	}
}

// leadAStore is a graphtrace.Store that counts what reaches the database layer.
type leadAStore struct {
	mu   sync.Mutex
	recs []graphtrace.Record
}

func (s *leadAStore) Write(_ context.Context, r graphtrace.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs = append(s.recs, r)
	return nil
}
func (*leadAStore) Prune(context.Context, time.Time) (int64, error) { return 0, nil }
func (s *leadAStore) records() []graphtrace.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]graphtrace.Record(nil), s.recs...)
}

type leadATraceRig struct {
	handler  http.Handler
	store    *leadAStore
	recorder *graphtrace.Recorder
	mu       sync.Mutex
	enqueued []string
}

// leadATraceChain wires the production chain withContextTrace(maskGraphResponse(h))
// for resolve or search, with a store spy behind the recorder and, for resolve,
// an enqueue spy.
func leadATraceChain(t *testing.T, pool *pgxpool.Pool, route string, leadEnabled bool) *leadATraceRig {
	t.Helper()
	rig := &leadATraceRig{store: &leadAStore{}}
	rig.recorder = graphtrace.NewRecorder(rig.store, graphtrace.Options{})
	var inner http.Handler
	switch route {
	case "resolve":
		h, err := NewResolve(pool)
		if err != nil {
			t.Fatal(err)
		}
		h.WithLeadMode(leadEnabled)
		h.enqueueFetchBodyFn = func(_ context.Context, id string) {
			rig.mu.Lock()
			defer rig.mu.Unlock()
			rig.enqueued = append(rig.enqueued, id)
		}
		inner = h
	case "search":
		h, err := NewSearchWithEmbedder(pool, leadAGemini{})
		if err != nil {
			t.Fatal(err)
		}
		h.WithLeadMode(leadEnabled)
		inner = h
	default:
		t.Fatalf("route %q", route)
	}
	rig.handler = withContextTrace(maskGraphResponse(inner), route, "lead-a", rig.recorder)
	return rig
}

// TestLeadANoTraceSwitch: X-Graph-No-Side-Effects 1/true (any case) writes no
// trace and enqueues no fetch_body; absent writes one trace; a body-only flag
// still writes the trace but skips fetch_body; header 0 overrides body true;
// request id header and JSON request_id are present and equal throughout.
func TestLeadANoTraceSwitch(t *testing.T) {
	env := leadASetup(t)
	const miss = `{"seeds":["jira:GC0-MISS"]`
	type tcase struct {
		name         string
		route, lead  string // lead = X-Graph-Lead header value; "" = absent
		leadEnabled  bool
		method, path string
		body         string
		header       string // X-Graph-No-Side-Effects; "" = absent
		wantRecords  int
		wantEnqueue  []string
	}
	missing := []string{"jira:GC0-MISS"}
	var cases []tcase
	for _, lead := range []struct {
		enabled bool
		header  string
	}{{false, ""}, {true, "1"}} {
		sfx := fmt.Sprintf("/lead=%v", lead.enabled)
		for _, h := range []string{"1", "TRUE", "true", "TrUe"} {
			cases = append(cases, tcase{"header " + h + sfx, "resolve", lead.header, lead.enabled, "POST", "/api/graph/resolve", miss + `}`, h, 0, nil})
		}
		cases = append(cases,
			tcase{"absent" + sfx, "resolve", lead.header, lead.enabled, "POST", "/api/graph/resolve", miss + `}`, "", 1, missing},
			tcase{"body-only" + sfx, "resolve", lead.header, lead.enabled, "POST", "/api/graph/resolve", miss + `,"no_side_effects":true}`, "", 1, nil},
			tcase{"header 0 over body true" + sfx, "resolve", lead.header, lead.enabled, "POST", "/api/graph/resolve", miss + `,"no_side_effects":true}`, "0", 1, missing},
			tcase{"header false over body true" + sfx, "resolve", lead.header, lead.enabled, "POST", "/api/graph/resolve", miss + `,"no_side_effects":true}`, "false", 1, missing},
			tcase{"header 1 over body false" + sfx, "resolve", lead.header, lead.enabled, "POST", "/api/graph/resolve", miss + `,"no_side_effects":false}`, "1", 0, nil},
		)
		for _, h := range []string{"1", "TRUE", "true"} {
			cases = append(cases, tcase{"search header " + h + sfx, "search", lead.header, lead.enabled, "GET", "/api/graph/search?q=phase+zero&match=hybrid", "", h, 0, nil})
		}
		cases = append(cases,
			tcase{"search absent" + sfx, "search", lead.header, lead.enabled, "GET", "/api/graph/search?q=phase+zero", "", "", 1, nil},
			tcase{"search hybrid absent" + sfx, "search", lead.header, lead.enabled, "GET", "/api/graph/search?q=phase+zero&match=hybrid", "", "", 1, nil},
			tcase{"search header 0" + sfx, "search", lead.header, lead.enabled, "GET", "/api/graph/search?q=phase+zero", "", "0", 1, nil},
		)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := leadATraceChain(t, env.pool, tc.route, tc.leadEnabled)
			req := adminRequest(tc.method, tc.path, bytes.NewBufferString(tc.body))
			if tc.header != "" {
				req.Header.Set("X-Graph-No-Side-Effects", tc.header)
			}
			if tc.lead != "" {
				req.Header.Set("X-Graph-Lead", tc.lead)
			}
			w := httptest.NewRecorder()
			rig.handler.ServeHTTP(w, req)
			rig.recorder.Close(5 * time.Second)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			id := w.Header().Get("X-Graph-Request-Id")
			var top map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &top); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(id, "ctx_") || top["request_id"] != id {
				t.Fatalf("request id header=%q body=%v", id, top["request_id"])
			}
			recs := rig.store.records()
			if len(recs) != tc.wantRecords {
				t.Fatalf("trace records written=%d want %d", len(recs), tc.wantRecords)
			}
			if st := rig.recorder.Stats(); int(st.Attempted) != tc.wantRecords {
				t.Fatalf("recorder attempted=%d want %d (the wrapper must not enqueue when the header asks for no side effects)", st.Attempted, tc.wantRecords)
			}
			if tc.wantRecords == 1 && (recs[0].ID != id || recs[0].Route != tc.route) {
				t.Fatalf("record id=%q route=%q, header id=%q", recs[0].ID, recs[0].Route, id)
			}
			rig.mu.Lock()
			got := append([]string(nil), rig.enqueued...)
			rig.mu.Unlock()
			if strings.Join(got, ",") != strings.Join(tc.wantEnqueue, ",") {
				t.Fatalf("fetch_body enqueues=%v want %v", got, tc.wantEnqueue)
			}
		})
	}
}

// TestLeadANoTraceSwitchMounted drives the same switch through Mount (the
// production wiring, with the flag on and off) and counts what lands in
// graph.jobs instead of using the enqueue spy.
func TestLeadANoTraceSwitchMounted(t *testing.T) {
	env := leadASetup(t)
	ctx := context.Background()
	jobCount := func() int {
		var n int
		if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM graph.jobs WHERE type='fetch_body'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, enabled := range []bool{false, true} {
		store := &leadAStore{}
		recorder := graphtrace.NewRecorder(store, graphtrace.Options{})
		r := chi.NewRouter()
		Mount(r, Deps{DB: env.pool, Gemini: leadAGemini{}, TraceRecorder: recorder, BuildRev: "lead-a", LeadModeEnabled: enabled})
		do := func(method, target, body, header string) *httptest.ResponseRecorder {
			req := adminRequest(method, target, bytes.NewBufferString(body))
			if header != "" {
				req.Header.Set("X-Graph-No-Side-Effects", header)
			}
			if enabled {
				req.Header.Set("X-Graph-Lead", "1")
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			return w
		}
		aclExec(t, env.pool, `DELETE FROM graph.jobs`)
		for _, h := range []string{"1", "TRUE", "true"} {
			for _, w := range []*httptest.ResponseRecorder{
				do("POST", "/api/graph/resolve", `{"seeds":["jira:GC0-MISS"]}`, h),
				do("GET", "/api/graph/search?q=phase+zero", "", h),
			} {
				if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("X-Graph-Request-Id"), "ctx_") {
					t.Fatalf("env=%v header=%s: status=%d id=%q", enabled, h, w.Code, w.Header().Get("X-Graph-Request-Id"))
				}
			}
		}
		if n := jobCount(); n != 0 {
			t.Fatalf("env=%v: %d fetch_body jobs enqueued despite the no-side-effects header", enabled, n)
		}
		w := do("POST", "/api/graph/resolve", `{"seeds":["jira:GC0-MISS"],"no_side_effects":true}`, "")
		if w.Code != http.StatusOK || jobCount() != 0 {
			t.Fatalf("env=%v: body-only flag: status=%d jobs=%d", enabled, w.Code, jobCount())
		}
		w = do("POST", "/api/graph/resolve", `{"seeds":["jira:GC0-MISS"],"no_side_effects":true}`, "0")
		if w.Code != http.StatusOK || jobCount() != 1 {
			t.Fatalf("env=%v: header 0 must override the body flag: status=%d jobs=%d", enabled, w.Code, jobCount())
		}
		recorder.Close(5 * time.Second)
		// Traces: body-only (1) + header 0 (1); the six header-flagged requests write none.
		if n := len(store.records()); n != 2 {
			t.Fatalf("env=%v: trace records=%d want 2", enabled, n)
		}
	}
}
