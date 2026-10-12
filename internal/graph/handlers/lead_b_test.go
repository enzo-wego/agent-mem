package handlers

// Lead mode R1a tests, author B: acceptance 3 (root eligibility), 4 (masking
// before the cap) and 5 (grouping and links). Every name carries the leadB
// prefix. Fixtures are synthetic.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/bfs"
	graphtrace "github.com/agent-mem/agent-mem/internal/graph/trace"
	"github.com/agent-mem/agent-mem/internal/redact"
)

// ---- shared helpers ---------------------------------------------------------

// leadBSetup opens the scratch DB, clears graph rows and the thread summaries
// this file writes, and registers the same cleanup.
func leadBSetup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := openTestDB(t)
	resetEpicACL(t, pool)
	clean := func() { aclExec(t, pool, `DELETE FROM graph.thread_summaries WHERE channel_id LIKE 'LB%'`) }
	clean()
	t.Cleanup(clean)
	return pool
}

type leadBNode struct {
	id, typ, scope, title, body string
	meta                        string // JSON object; "" = {}
	updated                     string // RFC3339
	author                      int64
	deleted                     bool
}

func leadBInsert(t *testing.T, pool *pgxpool.Pool, n leadBNode) {
	t.Helper()
	if n.meta == "" {
		n.meta = `{}`
	}
	if n.updated == "" {
		n.updated = "2026-10-10T00:00:00Z"
	}
	aclExec(t, pool, `INSERT INTO graph.nodes
 (id, type, natural_key, title, body, scope, metadata, updated_at, author_person_id, deleted_at, machine_id)
VALUES ($1,$2,$1,NULLIF($3,''),$4,NULLIF($5,''),$6::jsonb,$7::timestamptz,NULLIF($8::bigint,0),
        CASE WHEN $9::boolean THEN now() END,'test')`,
		n.id, n.typ, n.title, n.body, n.scope, n.meta, n.updated, n.author, n.deleted)
}

// leadBMsg inserts the Slack message node slack:<ch>:<ts> that belongs to the
// thread thread (a root has thread == ts).
func leadBMsg(t *testing.T, pool *pgxpool.Pool, ch, ts, thread, scope, body, updated string, author int64) string {
	t.Helper()
	id := "slack:" + ch + ":" + ts
	leadBInsert(t, pool, leadBNode{
		id: id, typ: "slack", scope: scope, body: body, updated: updated, author: author,
		meta: fmt.Sprintf(`{"ts":%q,"thread_ts":%q}`, ts, thread),
	})
	return id
}

func leadBJira(t *testing.T, pool *pgxpool.Pool, id, body, updated string) {
	t.Helper()
	leadBInsert(t, pool, leadBNode{id: id, typ: "jira", title: id, body: body, updated: updated})
}

func leadBPerson(t *testing.T, pool *pgxpool.Pool, name, slackID string, bot bool) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `
INSERT INTO graph.people (display_name, slack_user_id, is_bot, machine_id)
VALUES ($1, NULLIF($2,''), $3, 'test') RETURNING id`, name, slackID, bot).Scan(&id)
	if err != nil {
		t.Fatalf("leadBPerson %s: %v", name, err)
	}
	return id
}

func leadBEdge(t *testing.T, pool *pgxpool.Pool, from, to, kind, meta string) {
	t.Helper()
	if meta == "" {
		meta = `{}`
	}
	aclExec(t, pool, `INSERT INTO graph.edges (from_node_id, to_node_id, kind, metadata, machine_id)
VALUES ($1,$2,$3,$4::jsonb,'test')`, from, to, kind, meta)
}

func leadBSummary(t *testing.T, pool *pgxpool.Pool, ch, threadTS, summary, overview string) {
	t.Helper()
	aclExec(t, pool, `INSERT INTO graph.thread_summaries (channel_id, thread_ts, signature, summary, overview)
VALUES ($1,$2,'sig',$3,$4)
ON CONFLICT (channel_id, thread_ts) DO UPDATE SET summary = EXCLUDED.summary, overview = EXCLUDED.overview`,
		ch, threadTS, summary, overview)
}

func leadBIndex(t *testing.T, pool *pgxpool.Pool, nodeID, summary string) {
	t.Helper()
	aclExec(t, pool, `INSERT INTO graph.artifact_index (node_id, summary, summary_kind, machine_id)
VALUES ($1,$2,'heuristic','test')
ON CONFLICT (node_id) DO UPDATE SET summary = EXCLUDED.summary`, nodeID, summary)
}

// leadBPinNow fixes the resolve clock so scores are reproducible.
func leadBPinNow(t *testing.T) {
	t.Helper()
	old := nowFunc
	nowFunc = func() time.Time { return time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { nowFunc = old })
}

func leadBNewResolve(t *testing.T, pool *pgxpool.Pool) *Resolve {
	t.Helper()
	h, err := NewResolve(pool)
	if err != nil {
		t.Fatal(err)
	}
	h.enqueueFetchBodyFn = func(context.Context, string) {}
	return h.WithLeadMode(true)
}

func leadBNewSearch(t *testing.T, pool *pgxpool.Pool) *Search {
	t.Helper()
	s, err := NewSearch(pool)
	if err != nil {
		t.Fatal(err)
	}
	return s.WithLeadMode(true)
}

type leadBResp struct {
	code   int
	body   string
	header http.Header
}

// leadBDo serves one request. asker "" is the admin view; otherwise it names a
// filtered asker. lead sets X-Graph-Lead: 1.
func leadBDo(ctx context.Context, h http.Handler, method, target, body, asker string, lead bool) leadBResp {
	r := httptest.NewRequest(method, target, strings.NewReader(body)).WithContext(ctx)
	if asker == "" {
		r.Header.Set(adminHeader, "1")
	} else {
		r.Header.Set("X-Asker-User", asker)
	}
	if lead {
		r.Header.Set(leadHeader, "1")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return leadBResp{code: w.Code, body: w.Body.String(), header: w.Header()}
}

func leadBMap(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("not a JSON object: %v\n%s", err, body)
	}
	return m
}

func leadBRows(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	rs, _ := leadBMap(t, body)["results"].([]any)
	for _, r := range rs {
		out = append(out, r.(map[string]any))
	}
	return out
}

func leadBRowIDs(rows []map[string]any) []string {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r["node_id"].(string))
	}
	return ids
}

func leadBForbid(t *testing.T, label, body string, forbidden ...string) {
	t.Helper()
	for _, f := range forbidden {
		if strings.Contains(body, f) {
			t.Errorf("%s: response contains forbidden %q:\n%s", label, f, body)
		}
	}
}

// ---- resolve response decoding ----------------------------------------------

type leadBLink struct {
	Kind       string  `json:"kind"`
	Tag        string  `json:"tag"`
	Topic      string  `json:"topic"`
	Why        string  `json:"why"`
	Confidence float64 `json:"confidence"`
	From       string  `json:"from"`
	To         string  `json:"to"`
}

type leadBArt struct {
	NodeID         string          `json:"node_id"`
	Type           string          `json:"type"`
	Title          string          `json:"title"`
	Author         string          `json:"author"`
	ThreadTS       string          `json:"thread_ts"`
	Score          float64         `json:"score"`
	ScoreBreakdown json.RawMessage `json:"score_breakdown"`
	Hop            int             `json:"hop"`
	Body           *string         `json:"body"`
	Links          []leadBLink     `json:"links"`
	FocusTS        *string         `json:"focus_ts"`
	LeadSummary    string          `json:"lead_summary"`
	LastActivityAt string          `json:"last_activity_at"`
	AuthorSlackID  string          `json:"author_slack_id"`
	AuthoredByBot  string          `json:"authored_by_bot"`
	BotInThread    string          `json:"bot_in_thread"`
	ChannelID      string          `json:"channel_id"`
}

type leadBResolved struct {
	Artifacts      []leadBArt `json:"artifacts"`
	ArtifactsRaw   string
	AfterACL       int
	AfterThreshold int
}

func leadBDecodeResolve(t *testing.T, resp leadBResp) leadBResolved {
	t.Helper()
	if resp.code != 200 {
		t.Fatalf("resolve status %d: %s", resp.code, resp.body)
	}
	var env struct {
		Artifacts  json.RawMessage `json:"artifacts"`
		GraphTrace struct {
			AfterACL       int `json:"after_acl"`
			AfterThreshold int `json:"after_score_threshold"`
		} `json:"graph_trace"`
	}
	if err := json.Unmarshal([]byte(resp.body), &env); err != nil {
		t.Fatalf("decode resolve: %v\n%s", err, resp.body)
	}
	out := leadBResolved{ArtifactsRaw: string(env.Artifacts), AfterACL: env.GraphTrace.AfterACL, AfterThreshold: env.GraphTrace.AfterThreshold}
	if err := json.Unmarshal(env.Artifacts, &out.Artifacts); err != nil {
		t.Fatalf("decode artifacts: %v\n%s", err, resp.body)
	}
	return out
}

func (r leadBResolved) ids() []string {
	ids := make([]string, 0, len(r.Artifacts))
	for _, a := range r.Artifacts {
		ids = append(ids, a.NodeID)
	}
	return ids
}

func (r leadBResolved) art(t *testing.T, id string) leadBArt {
	t.Helper()
	for _, a := range r.Artifacts {
		if a.NodeID == id {
			return a
		}
	}
	t.Fatalf("artifact %s not in %v", id, r.ids())
	return leadBArt{}
}

func leadBResolveBody(seeds []string, depth int, extra map[string]any) string {
	m := map[string]any{"seeds": seeds, "depth": depth}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// ---- fault injection ----------------------------------------------------------

// leadBFault is a pgx tracer that cancels the request context the moment a
// statement containing marker starts. The statement then fails with a real
// driver error ("context canceled") while every earlier statement of the
// request has run normally, so only the targeted lead lookup fails.
type leadBFault struct {
	mu     sync.Mutex
	marker string
	cancel context.CancelFunc
	hits   int
}

func (f *leadBFault) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.marker != "" && strings.Contains(d.SQL, f.marker) {
		f.hits++
		if f.cancel != nil {
			f.cancel()
		}
	}
	return ctx
}
func (*leadBFault) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (f *leadBFault) arm(marker string) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	f.mu.Lock()
	f.marker, f.cancel, f.hits = marker, cancel, 0
	f.mu.Unlock()
	return ctx
}

func (f *leadBFault) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

func leadBFaultPool(t *testing.T) (*pgxpool.Pool, *leadBFault) {
	t.Helper()
	cfg, err := parseScratchDSN(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	f := &leadBFault{}
	cfg.ConnConfig.Tracer = f
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, f
}

// ============================================================================
// Acceptance 3: root eligibility
// ============================================================================

const (
	leadB3Asker    = "lb-asker@example.com"
	leadB3Sentinel = "LBSENTINEL-7f3a"
	leadB3Hidden   = "ULBHIDDEN1"
	leadB3Why      = "LBLINKWHY"
	leadB3Date     = "2030-03-03" // hidden root's updated_at: an aggregate value that must not leak
	leadB3Channel  = "LBR"
	leadB3RootTS   = "100.000001"
	leadB3ReplyTS  = "101.000001"
	leadB3Seed     = "jira:LBSEED-1"
	leadB3Epic     = "LBEPIC"
)

type leadB3Case struct {
	name        string
	rootScope   string
	noRoot      bool
	rootDeleted bool
	epic        bool // the request carries the epic filter
	rootInEpic  bool
	eligible    bool
	noRootEdge  bool // no seed -> root edge: the root is not reachable on its own
	nodeOK      bool // the node route has no epic filter: the root stays eligible there
}

var leadB3Cases = []leadB3Case{
	{name: "control_eligible", rootScope: "slack:LBR", eligible: true},
	{name: "control_eligible_epic", rootScope: "slack:LBR", epic: true, rootInEpic: true, eligible: true},
	{name: "hidden_root", rootScope: "slack:LBX"},
	{name: "missing_root", noRoot: true},
	{name: "deleted_root", rootScope: "slack:LBR", rootDeleted: true},
	{name: "root_outside_epic", rootScope: "slack:LBR", epic: true, nodeOK: true},
	{name: "mismatched_scope", rootScope: "public", noRootEdge: true},
}

type leadB3IDs struct{ seed, root, reply string }

// leadB3Build writes one thread whose reply is visible to the asker. The root
// (when it exists) carries the sentinel in its title and in its cached thread
// summary and overview, a distinctive author, a bot flag and a far-future
// updated_at.
func leadB3Build(t *testing.T, pool *pgxpool.Pool, c leadB3Case) leadB3IDs {
	t.Helper()
	ids := leadB3IDs{
		seed:  leadB3Seed,
		root:  "slack:" + leadB3Channel + ":" + leadB3RootTS,
		reply: "slack:" + leadB3Channel + ":" + leadB3ReplyTS,
	}
	aclPerson(t, pool, 7001, leadB3Asker, "slack:LBR")
	rootAuthor := leadBPerson(t, pool, "Hidden Root Author", leadB3Hidden, true)
	replyAuthor := leadBPerson(t, pool, "Reply Author", "ULBREPLY1", false)

	if !c.noRoot {
		leadBInsert(t, pool, leadBNode{
			id: ids.root, typ: "slack", scope: c.rootScope, title: leadB3Sentinel + " root title",
			body: "root body", meta: `{"ts":"` + leadB3RootTS + `","thread_ts":"` + leadB3RootTS + `"}`,
			updated: leadB3Date + "T03:03:03Z", author: rootAuthor, deleted: c.rootDeleted,
		})
	}
	leadBSummary(t, pool, leadB3Channel, leadB3RootTS, leadB3Sentinel+" summary", leadB3Sentinel+" overview")
	leadBMsg(t, pool, leadB3Channel, leadB3ReplyTS, leadB3RootTS, "slack:LBR", "lbtoken reply body", "2026-10-10T00:00:00Z", replyAuthor)
	leadBInsert(t, pool, leadBNode{id: ids.seed, typ: "jira", title: ids.seed, body: "seed body"})
	leadBEdge(t, pool, ids.seed, ids.reply, "REFERENCES", `{"why":"`+leadB3Why+` reply edge","tag":"lbtag","confidence":0.9}`)
	if !c.noRoot && !c.eligible && !c.noRootEdge {
		leadBEdge(t, pool, ids.seed, ids.root, "REFERENCES", `{"why":"`+leadB3Why+` root edge"}`)
	}
	if c.epic {
		aclMember(t, pool, ids.reply, leadB3Epic, viaKey)
		if c.rootInEpic {
			aclMember(t, pool, ids.root, leadB3Epic, viaKey)
		}
	}
	return ids
}

func (ids leadB3IDs) forbidden() []string {
	return []string{leadB3Sentinel, leadB3Hidden, leadB3Why, leadB3Date, ids.root, ids.reply}
}

// leadB3Eligible checks the lead fields every route derives from the eligible root.
func leadB3Eligible(t *testing.T, label string, get func(string) any) {
	t.Helper()
	want := map[string]any{
		"lead_summary":     leadB3Sentinel + " summary",
		"author_slack_id":  leadB3Hidden,
		"authored_by_bot":  "bot",
		"bot_in_thread":    "yes",
		"channel_id":       leadB3Channel,
		"last_activity_at": leadB3Date + "T03:03:03Z",
	}
	for k, v := range want {
		if got := get(k); got != v {
			t.Errorf("%s: %s = %v, want %v", label, k, got, v)
		}
	}
}

func TestLeadB3RootEligibility(t *testing.T) {
	for _, route := range []string{"resolve", "node", "search", "hybrid"} {
		for _, c := range leadB3Cases {
			t.Run(route+"/"+c.name, func(t *testing.T) {
				pool := leadBSetup(t)
				ids := leadB3Build(t, pool, c)
				ctx := context.Background()
				switch route {
				case "resolve":
					h := leadBNewResolve(t, pool)
					extra := map[string]any{}
					if c.epic {
						extra["epic"] = []string{leadB3Epic}
					}
					resp := leadBDo(ctx, h, "POST", "/api/graph/resolve", leadBResolveBody([]string{ids.seed}, 2, extra), leadB3Asker, true)
					res := leadBDecodeResolve(t, resp)
					if c.eligible {
						if got := res.ids(); !reflect.DeepEqual(got, []string{ids.seed, ids.root}) {
							t.Fatalf("artifacts = %v, want seed then root", got)
						}
						a := res.art(t, ids.root)
						leadB3Eligible(t, "resolve root", func(k string) any {
							return map[string]any{"lead_summary": a.LeadSummary, "author_slack_id": a.AuthorSlackID,
								"authored_by_bot": a.AuthoredByBot, "bot_in_thread": a.BotInThread,
								"channel_id": a.ChannelID, "last_activity_at": a.LastActivityAt}[k]
						})
						leadBForbid(t, "resolve eligible", res.ArtifactsRaw, ids.reply)
						return
					}
					// Vacuity guard: the reply was an authorized candidate, so the
					// group was dropped by the root rule and not by the walk.
					if res.AfterACL < 2 {
						t.Fatalf("after_acl = %d: the visible reply never became a candidate", res.AfterACL)
					}
					if got := res.ids(); !reflect.DeepEqual(got, []string{ids.seed}) {
						t.Fatalf("artifacts = %v, want only the seed", got)
					}
					leadBForbid(t, "resolve", resp.body, ids.forbidden()...)
				case "node":
					h := NewNode(pool).WithLeadMode(true)
					target := "/api/graph/node?id=" + url.QueryEscape(ids.reply)
					resp := leadBDo(ctx, h, "GET", target, "", leadB3Asker, true)
					plain := leadBDo(ctx, h, "GET", target, "", leadB3Asker, false)
					if plain.code != 200 || !strings.Contains(plain.body, ids.reply) {
						t.Fatalf("control without lead header: %d %s", plain.code, plain.body)
					}
					if c.eligible || c.nodeOK {
						if resp.code != 200 {
							t.Fatalf("status %d: %s", resp.code, resp.body)
						}
						m := leadBMap(t, resp.body)
						if m["node_id"] != ids.reply {
							t.Errorf("node_id = %v, want the reply id (no remapping)", m["node_id"])
						}
						leadB3Eligible(t, "node", func(k string) any { return m[k] })
						for _, k := range []string{"body", "edges_in", "edges_out"} {
							if _, ok := m[k]; ok {
								t.Errorf("lead node carries %q", k)
							}
						}
						return
					}
					if resp.code != 404 {
						t.Fatalf("status %d, want 404: %s", resp.code, resp.body)
					}
					leadBForbid(t, "node", resp.body, ids.forbidden()...)
				case "search", "hybrid":
					h := leadBNewSearch(t, pool)
					q := "/api/graph/search?q=lbtoken&limit=20"
					if route == "hybrid" {
						q += "&match=hybrid"
					} else {
						q += "&arms=keyword"
					}
					if c.epic {
						q += "&epic=" + leadB3Epic
					}
					plain := leadBDo(ctx, h, "GET", q, "", leadB3Asker, false)
					if plain.code != 200 || len(leadBRows(t, plain.body)) != 1 {
						t.Fatalf("control without lead header: %d %s", plain.code, plain.body)
					}
					resp := leadBDo(ctx, h, "GET", q, "", leadB3Asker, true)
					if resp.code != 200 {
						t.Fatalf("status %d: %s", resp.code, resp.body)
					}
					rows := leadBRows(t, resp.body)
					if c.eligible {
						if len(rows) != 1 {
							t.Fatalf("rows = %v, want the one thread row", leadBRowIDs(rows))
						}
						if got := rows[0]["node_id"]; got != ids.reply && got != ids.root {
							t.Errorf("row id = %v", got)
						}
						leadB3Eligible(t, route, func(k string) any { return rows[0][k] })
						return
					}
					if route == "hybrid" && c.name == "mismatched_scope" {
						// Hybrid's thread folding (unchanged by lead mode) promotes a
						// hit to its root row when that row passes the request filter;
						// here the root is public, so the row returned is the root
						// itself, a node the asker may read. The reply id and every
						// value that would come from the thread cache of the
						// mismatched root (summary, overview) must not appear.
						leadBForbid(t, "hybrid mismatch", resp.body, ids.reply, leadB3Sentinel+" summary", leadB3Sentinel+" overview")
						if len(rows) != 1 || rows[0]["node_id"] != ids.root || rows[0]["lead_summary"] != "" {
							t.Errorf("rows = %v, want only the root's own row without a thread summary", leadBRowIDs(rows))
						}
						return
					}
					if len(rows) != 0 {
						t.Fatalf("rows = %v, want the reply dropped", leadBRowIDs(rows))
					}
					if total := leadBMap(t, resp.body)["total"]; total != float64(0) {
						t.Errorf("total = %v, want 0", total)
					}
					leadBForbid(t, route, resp.body, ids.forbidden()...)
				}
			})
		}
	}
}

// One response, two linked threads: the one with an eligible root is returned
// with its own values, the one whose root is hidden leaves no trace.
func TestLeadB3ResolveMixedThreads(t *testing.T) {
	pool := leadBSetup(t)
	aclPerson(t, pool, 7001, leadB3Asker, "slack:LBR")
	botAuthor := leadBPerson(t, pool, "Hidden Root Author", leadB3Hidden, true)
	okAuthor := leadBPerson(t, pool, "Visible Root Author", "ULBVISIBLE1", false)
	seed := "jira:LBSEED-2"
	leadBJira(t, pool, seed, "seed", "2026-10-10T00:00:00Z")
	// Thread X: eligible root (human author, no bot).
	x0 := leadBMsg(t, pool, "LBR", "150.000001", "150.000001", "slack:LBR", "x root", "2026-10-05T00:00:00Z", okAuthor)
	x1 := leadBMsg(t, pool, "LBR", "151.000001", "150.000001", "slack:LBR", "x reply", "2026-10-06T00:00:00Z", okAuthor)
	leadBSummary(t, pool, "LBR", "150.000001", "visible thread summary", "visible overview")
	// Thread Y: the root sits in a scope the asker lacks and carries the
	// sentinel; its reply is readable and linked.
	y0 := "slack:LBR:200.000001"
	leadBInsert(t, pool, leadBNode{id: y0, typ: "slack", scope: "slack:LBX", title: leadB3Sentinel + " y title", body: "y root",
		meta: `{"ts":"200.000001","thread_ts":"200.000001"}`, updated: leadB3Date + "T03:03:03Z", author: botAuthor})
	y1 := leadBMsg(t, pool, "LBR", "201.000001", "200.000001", "slack:LBR", "y reply", "2026-10-07T00:00:00Z", okAuthor)
	leadBSummary(t, pool, "LBR", "200.000001", leadB3Sentinel+" summary", leadB3Sentinel+" overview")
	leadBEdge(t, pool, seed, x1, "REFERENCES", `{"why":"visible why","confidence":0.5}`)
	leadBEdge(t, pool, seed, y1, "REFERENCES", `{"why":"`+leadB3Why+` y","confidence":0.9}`)

	h := leadBNewResolve(t, pool)
	resp := leadBDo(context.Background(), h, "POST", "/api/graph/resolve", leadBResolveBody([]string{seed}, 2, nil), leadB3Asker, true)
	res := leadBDecodeResolve(t, resp)
	if res.AfterACL < 3 {
		t.Fatalf("after_acl = %d: both replies must have been authorized candidates", res.AfterACL)
	}
	if got := res.ids(); !reflect.DeepEqual(got, []string{seed, x0}) {
		t.Fatalf("artifacts = %v, want the seed and the eligible root", got)
	}
	leadBForbid(t, "resolve", resp.body, leadB3Sentinel, leadB3Hidden, leadB3Why, leadB3Date, y0, y1, x1)
	a := res.art(t, x0)
	if a.LeadSummary != "visible thread summary" || a.AuthorSlackID != "ULBVISIBLE1" || a.AuthoredByBot != "human" ||
		a.BotInThread != "no" || a.LastActivityAt != "2026-10-06T00:00:00Z" {
		t.Errorf("eligible group values = %+v", a)
	}
	if len(a.Links) != 1 || a.Links[0].Why != "visible why" || a.Links[0].From != seed || a.Links[0].To != x0 {
		t.Errorf("links = %+v", a.Links)
	}
}

// A root lookup error is a 503 with body lead_lookup_failed on every route,
// never a denial (404, dropped row) and never a response built without the
// lookup. The statement that fails is the root-eligibility statement itself.
func TestLeadB3RootLookupError(t *testing.T) {
	markers := map[string]string{
		"root_rows":   "SELECT n.id, n.type = 'slack', COALESCE(n.scope,'')",
		"root_access": "JOIN graph.nodes r ON r.id = x.id",
	}
	fixtures := []leadB3Case{leadB3Cases[0], leadB3Cases[2]} // eligible root, hidden root
	for _, route := range []string{"resolve", "node", "search", "hybrid"} {
		for mname, marker := range markers {
			for _, c := range fixtures {
				t.Run(route+"/"+mname+"/"+c.name, func(t *testing.T) {
					pool := leadBSetup(t)
					ids := leadB3Build(t, pool, c)
					fpool, fault := leadBFaultPool(t)
					var h http.Handler
					var method, target, body string
					switch route {
					case "resolve":
						h = leadBNewResolve(t, fpool)
						method, target, body = "POST", "/api/graph/resolve", leadBResolveBody([]string{ids.seed}, 2, nil)
					case "node":
						h = NewNode(fpool).WithLeadMode(true)
						method, target = "GET", "/api/graph/node?id="+url.QueryEscape(ids.reply)
					case "search":
						h = leadBNewSearch(t, fpool)
						method, target = "GET", "/api/graph/search?q=lbtoken&arms=keyword"
					case "hybrid":
						h = leadBNewSearch(t, fpool)
						method, target = "GET", "/api/graph/search?q=lbtoken&match=hybrid"
					}
					ctx := fault.arm(marker)
					resp := leadBDo(ctx, h, method, target, body, leadB3Asker, true)
					if fault.count() == 0 {
						t.Fatalf("the targeted statement never ran: fault not exercised (status %d)", resp.code)
					}
					if resp.code != http.StatusServiceUnavailable {
						t.Fatalf("status %d, want 503: %s", resp.code, resp.body)
					}
					if got := strings.TrimSpace(resp.body); got != "lead_lookup_failed" {
						t.Fatalf("body = %q, want lead_lookup_failed", got)
					}
					leadBForbid(t, "503 body", resp.body, ids.forbidden()...)
				})
			}
		}
	}
}

// ============================================================================
// Acceptance 4: masking before the cap, on the mounted routes
// ============================================================================

func TestLeadB4CapRunesBoundaries(t *testing.T) {
	cases := []struct {
		name string
		s    string
		n    int
		want string
	}{
		{"empty", "", 600, ""},
		{"short", "abc", 600, "abc"},
		{"n-1 runes", strings.Repeat("a", 599), 600, strings.Repeat("a", 599)},
		{"exactly n runes", strings.Repeat("a", 600), 600, strings.Repeat("a", 600)},
		{"n+1 runes", strings.Repeat("a", 601), 600, strings.Repeat("a", 599) + "…"},
		{"long", strings.Repeat("a", 5000), 600, strings.Repeat("a", 599) + "…"},
		{"multibyte below", strings.Repeat("é", 600), 600, strings.Repeat("é", 600)},
		{"multibyte over", strings.Repeat("é", 601), 600, strings.Repeat("é", 599) + "…"},
		{"emoji over", strings.Repeat("😀", 601), 600, strings.Repeat("😀", 599) + "…"},
		{"emoji at the cut", strings.Repeat("a", 598) + "😀😀😀", 600, strings.Repeat("a", 598) + "😀" + "…"},
		{"cjk over", strings.Repeat("日", 700), 600, strings.Repeat("日", 599) + "…"},
		{"tiny n", "abcdef", 1, "…"},
		{"tiny n fits", "a", 1, "a"},
		{"n=2", "abc", 2, "a…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := capRunes(tc.s, tc.n)
			if got != tc.want {
				t.Fatalf("capRunes(len %d runes, %d) = %q (%d runes), want %q (%d runes)",
					utf8.RuneCountInString(tc.s), tc.n, got, utf8.RuneCountInString(got), tc.want, utf8.RuneCountInString(tc.want))
			}
			if !utf8.ValidString(got) {
				t.Fatalf("result is not valid UTF-8: %q", got)
			}
			if utf8.RuneCountInString(got) > tc.n {
				t.Fatalf("result has %d runes, cap %d", utf8.RuneCountInString(got), tc.n)
			}
		})
	}
}

func TestLeadB4LeadTextMasksBeforeCap(t *testing.T) {
	const email = "alice.wonder@example.com"
	for _, k := range []int{1, 5, 12, 24} {
		t.Run(fmt.Sprintf("email_cut_at_%d", k), func(t *testing.T) {
			text := leadBStraddle([]rune("lorem "), email, k, 60)
			if utf8.RuneCountInString(text) <= 600 {
				t.Fatal("fixture shorter than the cap")
			}
			got := leadText(false, text)
			if want := leadBWant(text, false); got != want {
				t.Fatalf("leadText(non-admin) = %q\nwant %q", got, want)
			}
			if leadBLeaks(got, email) {
				t.Fatalf("fragment of the email survives: %q", got[len(got)-80:])
			}
			if n := utf8.RuneCountInString(got); n != 600 || !strings.HasSuffix(got, "…") {
				t.Fatalf("got %d runes, suffix …=%v", n, strings.HasSuffix(got, "…"))
			}
			admin := leadText(true, text)
			if want := leadBWant(text, true); admin != want {
				t.Fatalf("leadText(admin) differs from the plain cut of the original")
			}
			if !strings.HasSuffix(admin, email[:k-1]+"…") {
				t.Fatalf("admin text must keep the unmasked leading %d runes of the token before …: %q", k-1, leadBTail(admin))
			}
		})
	}
	t.Run("masked_text_that_fits_is_not_cut", func(t *testing.T) {
		text := email + " " + strings.Repeat("lorem ", 100)[:600-len(email)-1+10] // 610 runes before masking
		if n := utf8.RuneCountInString(text); n <= 600 {
			t.Fatalf("fixture has %d runes", n)
		}
		masked := redact.Mask(text)
		if utf8.RuneCountInString(masked) > 600 {
			t.Fatalf("fixture still over the cap after masking: %d", utf8.RuneCountInString(masked))
		}
		if got := leadText(false, text); got != masked {
			t.Fatalf("masked text that fits must be returned whole:\n got %q\nwant %q", got, masked)
		}
		if got := leadText(true, text); utf8.RuneCountInString(got) != 600 || !strings.HasSuffix(got, "…") {
			t.Fatalf("admin text must be cut: %d runes", utf8.RuneCountInString(got))
		}
	})
}

// leadBCycle repeats unit until exactly n runes.
func leadBCycle(unit []rune, n int) string {
	out := make([]rune, 0, n)
	for len(out) < n {
		out = append(out, unit[len(out)%len(unit)])
	}
	return string(out)
}

// leadBStraddle builds prefix + token + tail so that rune 600 (1-based) of the
// text is the k-th rune of token; the prefix ends in a space.
func leadBStraddle(unit []rune, token string, k, tail int) string {
	prefix := []rune(leadBCycle(unit, 600-k))
	prefix[len(prefix)-1] = ' '
	return string(prefix) + token + " " + leadBCycle(unit, tail-1)
}

// leadBWant is the expected output of the lead text rule, written out without
// capRunes: mask for filtered callers, then keep 599 runes and "…" when the
// result is longer than 600 runes.
func leadBWant(text string, admin bool) string {
	s := text
	if !admin {
		s = redact.Mask(text)
	}
	r := []rune(s)
	if len(r) <= 600 {
		return s
	}
	return string(r[:599]) + "…"
}

// leadBLeaks reports whether s holds a fragment of token: four consecutive
// characters of it, or (for digit tokens) four consecutive digits of it.
func leadBLeaks(s, token string) bool {
	digits := func(v string) string {
		var b strings.Builder
		for _, r := range v {
			if r >= '0' && r <= '9' {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	windows := func(v string, n int) []string {
		r := []rune(v)
		var out []string
		for i := 0; i+n <= len(r); i++ {
			out = append(out, string(r[i:i+n]))
		}
		return out
	}
	if strings.Contains(token, "@") {
		for _, w := range windows(token, 4) {
			if strings.Contains(s, w) {
				return true
			}
		}
		return false
	}
	ds := digits(s)
	for _, w := range windows(digits(token), 4) {
		if strings.Contains(ds, w) {
			return true
		}
	}
	return false
}

type leadB4Variant struct {
	name   string
	texts  [5]string // thread summary, overview, root index summary, jira index summary (+ spare)
	tokens []string
}

const (
	leadB4Channel = "LBM"
	leadB4Asker   = "lb-m-asker@example.com"
	leadB4RootTS  = "300.000001"
	leadB4Root    = "slack:LBM:300.000001"
	leadB4Jira    = "jira:LBM-1"
)

// leadB4Fixture mounts the real routes (masking and trace wrapped, lead mode on)
// over one Slack thread and one Jira issue that both match the query "lbmtoken".
func leadB4Fixture(t *testing.T) (http.Handler, *pgxpool.Pool) {
	t.Helper()
	pool := leadBSetup(t)
	aclPerson(t, pool, 7010, leadB4Asker, "slack:LBM")
	leadBMsg(t, pool, leadB4Channel, leadB4RootTS, leadB4RootTS, "slack:LBM", "lbmtoken root body", "2026-10-10T00:00:00Z", 0)
	leadBMsg(t, pool, leadB4Channel, "301.000001", leadB4RootTS, "slack:LBM", "reply body", "2026-10-10T00:00:00Z", 0)
	leadBJira(t, pool, leadB4Jira, "lbmtoken jira body", "2026-10-10T00:00:00Z")
	leadBEdge(t, pool, leadB4Root, leadB4Jira, "REFERENCES", `{}`)
	leadBSummary(t, pool, leadB4Channel, leadB4RootTS, "initial", "initial")
	leadBIndex(t, pool, leadB4Root, "initial")
	leadBIndex(t, pool, leadB4Jira, "initial")

	recorder := graphtrace.NewRecorder(discardTraceStore{}, graphtrace.Options{})
	t.Cleanup(func() { recorder.Close(time.Second) })
	r := chi.NewRouter()
	Mount(r, Deps{DB: pool, TraceRecorder: recorder, LeadModeEnabled: true})
	return r, pool
}

func leadB4Apply(t *testing.T, pool *pgxpool.Pool, v leadB4Variant) {
	t.Helper()
	aclExec(t, pool, `UPDATE graph.thread_summaries SET summary=$1, overview=$2 WHERE channel_id=$3 AND thread_ts=$4`,
		v.texts[0], v.texts[1], leadB4Channel, leadB4RootTS)
	aclExec(t, pool, `UPDATE graph.artifact_index SET summary=$2 WHERE node_id=$1`, leadB4Root, v.texts[2])
	aclExec(t, pool, `UPDATE graph.artifact_index SET summary=$2 WHERE node_id=$1`, leadB4Jira, v.texts[3])
}

func leadB4Variants() []leadB4Variant {
	var out []leadB4Variant
	mk := func(name string, units [4][]rune, build func(unit []rune, i int) string, tokens ...string) {
		v := leadB4Variant{name: name, tokens: tokens}
		for i := range 4 {
			v.texts[i] = build(units[i], i)
		}
		out = append(out, v)
	}
	asciiUnits := [4][]rune{[]rune("lorem "), []rune("ipsum "), []rune("dolor "), []rune("sit ")}
	uniUnits := [4][]rune{[]rune("é日😀 "), []rune("ñ語🙂 "), []rune("ü語😀 "), []rune("ö日🙂 ")}
	tokens := []struct{ name, token string }{
		{"email", "alice.wonder@example.com"},
		{"phone", "+65 9123 4567"},
		{"card", "4111 1111 1111 1111"},
	}
	for _, tk := range tokens {
		L := utf8.RuneCountInString(tk.token)
		for _, k := range []int{1, L/2 + 1, L} {
			token := tk.token
			mk(fmt.Sprintf("%s_cut_at_%d", tk.name, k), asciiUnits, func(unit []rune, i int) string {
				return leadBStraddle(unit, token, k, 40+10*i)
			}, token)
		}
	}
	emailTok := "alice.wonder@example.com"
	mk("email_unicode_cut_mid", uniUnits, func(unit []rune, i int) string {
		return leadBStraddle(unit, emailTok, 13, 40+10*i)
	}, emailTok)
	for _, n := range []int{599, 600, 601} {
		n := n
		mk(fmt.Sprintf("plain_ascii_%d_runes", n), asciiUnits, func(unit []rune, i int) string { return leadBCycle(unit, n) })
		mk(fmt.Sprintf("plain_unicode_%d_runes", n), uniUnits, func(unit []rune, i int) string { return leadBCycle(unit, n) })
	}
	// Emoji exactly on the cut: runes 598-600 are 4-byte runes.
	mk("emoji_on_the_cut", asciiUnits, func(unit []rune, i int) string {
		return leadBCycle(unit, 597) + "😀😀😀" + "tail"
	})
	// An email ending exactly at rune 600 and one ending exactly at rune 599.
	mk("email_ends_at_600", asciiUnits, func(unit []rune, i int) string {
		p := []rune(leadBCycle(unit, 600-len(emailTok)))
		p[len(p)-1] = ' '
		return string(p) + emailTok + " " + leadBCycle(unit, 30)
	}, emailTok)
	// Masking shortens the text under the cap: 610 runes before, fewer after.
	mk("mask_shrinks_under_cap", asciiUnits, func(unit []rune, i int) string {
		return emailTok + " " + leadBCycle(unit, 600+10-len(emailTok)-1)
	}, emailTok)
	return out
}

// leadB4Check compares one field with the exact expected value and checks the
// invariants: valid UTF-8, at most 600 runes, no fragment of a sensitive token
// for filtered callers.
func leadB4Check(t *testing.T, label, got, text string, admin bool, tokens []string) {
	t.Helper()
	want := leadBWant(text, admin)
	if got != want {
		t.Errorf("%s (admin=%v): got %d runes %q\nwant %d runes %q", label, admin,
			utf8.RuneCountInString(got), leadBTail(got), utf8.RuneCountInString(want), leadBTail(want))
	}
	if !utf8.ValidString(got) {
		t.Errorf("%s: invalid UTF-8", label)
	}
	n := utf8.RuneCountInString(got)
	if n > 600 {
		t.Errorf("%s: %d runes exceeds the cap", label, n)
	}
	if utf8.RuneCountInString(text) > 600 && !admin && utf8.RuneCountInString(redact.Mask(text)) > 600 {
		if n != 600 || !strings.HasSuffix(got, "…") {
			t.Errorf("%s: cut text must be exactly 600 runes ending in …, got %d", label, n)
		}
	}
	if !admin {
		for _, tok := range tokens {
			if leadBLeaks(got, tok) {
				t.Errorf("%s: fragment of %q survives: ...%s", label, tok[:3], leadBTail(got))
			}
		}
	}
}

func leadBTail(s string) string {
	r := []rune(s)
	if len(r) > 40 {
		r = r[len(r)-40:]
	}
	return string(r)
}

func TestLeadB4MaskingBeforeCapMountedRoutes(t *testing.T) {
	h, pool := leadB4Fixture(t)
	callers := []struct {
		name, asker string
		admin       bool
	}{{"admin", "", true}, {"filtered", leadB4Asker, false}}
	for _, v := range leadB4Variants() {
		// A precondition on the fixture: the sample really is sensitive text for
		// the masker, so a leak could not hide behind an unmasked sample.
		for _, tok := range v.tokens {
			for i := range 4 {
				if m := redact.Mask(v.texts[i]); leadBLeaks(m, tok) || m == v.texts[i] {
					t.Fatalf("%s: fixture text %d is not masked by redact.Mask", v.name, i)
				}
			}
		}
		leadB4Apply(t, pool, v)
		for _, caller := range callers {
			t.Run(v.name+"/"+caller.name, func(t *testing.T) {
				check := func(label string, got any, text string) {
					s, ok := got.(string)
					if !ok {
						t.Errorf("%s: field missing or not a string: %v", label, got)
						return
					}
					leadB4Check(t, label, s, text, caller.admin, v.tokens)
				}
				ctx := context.Background()

				// resolve: lead_summary of the Slack root and of the Jira issue.
				body := leadBResolveBody([]string{leadB4Root}, 1, nil)
				res := leadBDecodeResolve(t, leadBDo(ctx, h, "POST", "/api/graph/resolve", body, caller.asker, true))
				check("resolve slack lead_summary", res.art(t, leadB4Root).LeadSummary, v.texts[0])
				check("resolve jira lead_summary", res.art(t, leadB4Jira).LeadSummary, v.texts[3])

				// node: summary (artifact_index) and lead_summary.
				for _, n := range []struct{ id, summary, lead string }{
					{leadB4Root, v.texts[2], v.texts[0]},
					{leadB4Jira, v.texts[3], v.texts[3]},
				} {
					resp := leadBDo(ctx, h, "GET", "/api/graph/node?id="+url.QueryEscape(n.id), "", caller.asker, true)
					if resp.code != 200 {
						t.Fatalf("node %s: %d %s", n.id, resp.code, resp.body)
					}
					m := leadBMap(t, resp.body)
					check("node "+n.id+" summary", m["summary"], n.summary)
					check("node "+n.id+" lead_summary", m["lead_summary"], n.lead)
				}

				// default search: summary (artifact_index) and lead_summary.
				resp := leadBDo(ctx, h, "GET", "/api/graph/search?q=lbmtoken&arms=keyword&limit=20", "", caller.asker, true)
				if resp.code != 200 {
					t.Fatalf("search: %d %s", resp.code, resp.body)
				}
				rows := leadBRows(t, resp.body)
				byID := map[string]map[string]any{}
				for _, r := range rows {
					byID[r["node_id"].(string)] = r
				}
				for _, n := range []struct{ id, summary, lead string }{
					{leadB4Root, v.texts[2], v.texts[0]},
					{leadB4Jira, v.texts[3], v.texts[3]},
				} {
					r, ok := byID[n.id]
					if !ok {
						t.Fatalf("search lacks %s: %v", n.id, leadBRowIDs(rows))
					}
					check("search "+n.id+" summary", r["summary"], n.summary)
					check("search "+n.id+" lead_summary", r["lead_summary"], n.lead)
				}

				// hybrid search: summary is the thread overview for the Slack row.
				resp = leadBDo(ctx, h, "GET", "/api/graph/search?q=lbmtoken&match=hybrid&limit=20", "", caller.asker, true)
				if resp.code != 200 {
					t.Fatalf("hybrid: %d %s", resp.code, resp.body)
				}
				byID = map[string]map[string]any{}
				for _, r := range leadBRows(t, resp.body) {
					byID[r["node_id"].(string)] = r
				}
				for _, n := range []struct{ id, summary, lead string }{
					{leadB4Root, v.texts[1], v.texts[0]},
					{leadB4Jira, v.texts[3], v.texts[3]},
				} {
					r, ok := byID[n.id]
					if !ok {
						t.Fatalf("hybrid lacks %s", n.id)
					}
					check("hybrid "+n.id+" summary", r["summary"], n.summary)
					check("hybrid "+n.id+" lead_summary", r["lead_summary"], n.lead)
				}
			})
		}
	}
}

// ============================================================================
// Acceptance 5: grouping and links
// ============================================================================

const leadB5Asker = "lb5-asker@example.com"

func leadB5Setup(t *testing.T) (*pgxpool.Pool, *Resolve) {
	t.Helper()
	leadBPinNow(t)
	pool := leadBSetup(t)
	aclPerson(t, pool, 7005, leadB5Asker, "slack:LBA", "slack:LBB", "slack:LBC")
	return pool, leadBNewResolve(t, pool)
}

func leadB5Resolve(t *testing.T, h *Resolve, seeds []string, depth int, extra map[string]any) leadBResolved {
	t.Helper()
	resp := leadBDo(context.Background(), h, "POST", "/api/graph/resolve", leadBResolveBody(seeds, depth, extra), leadB5Asker, true)
	return leadBDecodeResolve(t, resp)
}

type leadBMember struct {
	id  string
	hop int
}

// leadBBest is the expected group representative: highest score, lowest id.
func leadBBest(t *testing.T, h *Resolve, members ...leadBMember) scoredCand {
	t.Helper()
	var best scoredCand
	for i, m := range members {
		out := h.scoreAll(context.Background(), resolveRequest{}, []bfs.Candidate{{NodeID: m.id, Hop: m.hop}})
		if len(out) != 1 {
			t.Fatalf("scoreAll(%s) returned %d rows", m.id, len(out))
		}
		c := out[0]
		if i == 0 || c.Score > best.Score || (c.Score == best.Score && c.NodeID < best.NodeID) {
			best = c
		}
	}
	return best
}

func leadBScoreOf(t *testing.T, h *Resolve, id string, hop int) float64 {
	t.Helper()
	return leadBBest(t, h, leadBMember{id, hop}).Score
}

func leadBClose(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func leadBFocus(a leadBArt) string {
	if a.FocusTS == nil {
		return "<absent>"
	}
	return *a.FocusTS
}

// leadBAssertArt checks node id, hop, score and focus_ts of one artifact.
func leadBAssertArt(t *testing.T, a leadBArt, hop int, score float64, focus string) {
	t.Helper()
	if a.Hop != hop {
		t.Errorf("%s: hop = %d, want %d", a.NodeID, a.Hop, hop)
	}
	if !leadBClose(a.Score, score) {
		t.Errorf("%s: score = %v, want %v", a.NodeID, a.Score, score)
	}
	if got := leadBFocus(a); got != focus {
		t.Errorf("%s: focus_ts = %q, want %q", a.NodeID, got, focus)
	}
}

// leadBAssertShape checks the invariants every lead response keeps: no body,
// no self-links, links carry root ids, and none of replyIDs appears anywhere in
// the artifacts (including links).
func leadBAssertShape(t *testing.T, res leadBResolved, replyIDs ...string) {
	t.Helper()
	leadBForbid(t, "artifacts", res.ArtifactsRaw, replyIDs...)
	seen := map[string]bool{}
	for _, a := range res.Artifacts {
		if seen[a.NodeID] {
			t.Errorf("artifact %s twice", a.NodeID)
		}
		seen[a.NodeID] = true
		if a.Body != nil {
			t.Errorf("%s carries a body", a.NodeID)
		}
		for _, l := range a.Links {
			if l.From == "" || l.To == "" {
				t.Errorf("%s: link %+v lacks from/to", a.NodeID, l)
			}
			if l.From == l.To {
				t.Errorf("%s: self-link %+v", a.NodeID, l)
			}
			if l.Kind == "THREAD" {
				t.Errorf("%s: THREAD link in lead mode", a.NodeID)
			}
			if l.From != a.NodeID && l.To != a.NodeID {
				// A link on a non-seed artifact names that artifact on one side.
				t.Errorf("%s: link %+v does not touch the artifact", a.NodeID, l)
			}
		}
	}
	for _, a := range res.Artifacts {
		for _, l := range a.Links {
			if !seen[l.From] || !seen[l.To] {
				t.Errorf("%s: link %+v points outside the artifacts %v", a.NodeID, l, res.ids())
			}
		}
	}
}

func leadBJSONEqual(t *testing.T, label string, got json.RawMessage, want any) {
	t.Helper()
	wb, _ := json.Marshal(want)
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	_ = json.Unmarshal(wb, &w)
	if !reflect.DeepEqual(g, w) {
		t.Errorf("%s: got %s want %s", label, got, wb)
	}
}

// Thread A (seed root a0, reply a1) is joined to a reply (b1) of thread B by a
// cross-thread edge. B's group is keyed by its root and the link names roots.
func TestLeadB5CrossThreadEdgeToReply(t *testing.T) {
	pool, h := leadB5Setup(t)
	a0 := leadBMsg(t, pool, "LBA", "10.000001", "10.000001", "slack:LBA", "a root", "2026-10-10T00:00:00Z", 0)
	a1 := leadBMsg(t, pool, "LBA", "11.000001", "10.000001", "slack:LBA", "a reply", "2026-10-10T00:00:00Z", 0)
	b0 := leadBMsg(t, pool, "LBB", "20.000001", "20.000001", "slack:LBB", "b root", "2026-10-10T00:00:00Z", 0)
	b1 := leadBMsg(t, pool, "LBB", "21.000001", "20.000001", "slack:LBB", "b reply", "2026-10-10T00:00:00Z", 0)
	leadBEdge(t, pool, a0, b1, "REFERENCES", `{"tag":"t-ref","topic":"topic-x","why":"because-x","confidence":0.8}`)

	res := leadB5Resolve(t, h, []string{a0}, 2, nil)
	if got := res.ids(); !reflect.DeepEqual(got, []string{a0, b0}) {
		t.Fatalf("artifacts = %v, want [seed root, other root]", got)
	}
	leadBAssertShape(t, res, a1, b1)

	seedBest := leadBBest(t, h, leadBMember{a0, 0}, leadBMember{a1, 1})
	if seedBest.NodeID != a0 {
		t.Fatalf("fixture: the seed root must be its group's best, got %s", seedBest.NodeID)
	}
	leadBAssertArt(t, res.art(t, a0), 0, seedBest.Score, "11.000001")
	otherBest := leadBBest(t, h, leadBMember{b1, 1}, leadBMember{b0, 2})
	if otherBest.NodeID != b1 {
		t.Fatalf("fixture: the reply must out-score its root, got %s", otherBest.NodeID)
	}
	leadBAssertArt(t, res.art(t, b0), 1, otherBest.Score, "21.000001")
	leadBJSONEqual(t, "score_breakdown of "+b0, res.art(t, b0).ScoreBreakdown, otherBest.Breakdown)

	if l := res.art(t, a0).Links; len(l) != 0 {
		t.Errorf("seed artifact has links: %+v", l)
	}
	want := []leadBLink{{Kind: "REFERENCES", Tag: "t-ref", Topic: "topic-x", Why: "because-x", Confidence: 0.8, From: a0, To: b0}}
	if got := res.art(t, b0).Links; !reflect.DeepEqual(got, want) {
		t.Errorf("links of %s = %+v, want %+v", b0, got, want)
	}
}

// An edge from a reply of the seed thread: the seed side of the link is the
// seed root.
func TestLeadB5EdgeFromSeedThreadReply(t *testing.T) {
	pool, h := leadB5Setup(t)
	a0 := leadBMsg(t, pool, "LBA", "10.000001", "10.000001", "slack:LBA", "a root", "2026-10-10T00:00:00Z", 0)
	a1 := leadBMsg(t, pool, "LBA", "11.000001", "10.000001", "slack:LBA", "a reply", "2026-10-10T00:00:00Z", 0)
	leadBJira(t, pool, "jira:LBJ-1", "j body", "2026-10-10T00:00:00Z")
	leadBEdge(t, pool, a1, "jira:LBJ-1", "REFERENCES", `{"tag":"from-reply","confidence":0.7}`)

	res := leadB5Resolve(t, h, []string{a0}, 2, nil)
	if got := res.ids(); !reflect.DeepEqual(got, []string{a0, "jira:LBJ-1"}) {
		t.Fatalf("artifacts = %v", got)
	}
	leadBAssertShape(t, res, a1)
	leadBAssertArt(t, res.art(t, a0), 0, leadBBest(t, h, leadBMember{a0, 0}, leadBMember{a1, 1}).Score, "11.000001")
	j := res.art(t, "jira:LBJ-1")
	leadBAssertArt(t, j, 2, leadBScoreOf(t, h, "jira:LBJ-1", 2), "<absent>")
	want := []leadBLink{{Kind: "REFERENCES", Tag: "from-reply", Confidence: 0.7, From: a0, To: "jira:LBJ-1"}}
	if !reflect.DeepEqual(j.Links, want) {
		t.Errorf("links = %+v, want %+v", j.Links, want)
	}
	if j.BotInThread != "" {
		t.Errorf("non-Slack artifact has bot_in_thread %q", j.BotInThread)
	}
}

// Two members of one group joined to the seed by different kinds give one
// link per kind with the winning edge's metadata; for the same kind the more
// confident edge wins.
func TestLeadB5TwoMembersDifferentKinds(t *testing.T) {
	pool, h := leadB5Setup(t)
	j := "jira:LBJ-2"
	leadBJira(t, pool, j, "j body", "2026-10-10T00:00:00Z")
	b0 := leadBMsg(t, pool, "LBB", "20.000001", "20.000001", "slack:LBB", "b root", "2026-10-01T00:00:00Z", 0)
	b1 := leadBMsg(t, pool, "LBB", "21.000001", "20.000001", "slack:LBB", "b reply", "2026-10-11T00:00:00Z", 0)
	leadBEdge(t, pool, j, b1, "REFERENCES", `{"tag":"win-tag","why":"win-why","confidence":0.8}`)
	leadBEdge(t, pool, j, b0, "REFERENCES", `{"tag":"lose-tag","why":"lose-why","confidence":0.2}`)
	leadBEdge(t, pool, b0, j, "SAME_TOPIC", `{"topic":"tp-same","why":"same-why","confidence":0.6}`)

	res := leadB5Resolve(t, h, []string{j}, 2, nil)
	if got := res.ids(); !reflect.DeepEqual(got, []string{j, b0}) {
		t.Fatalf("artifacts = %v", got)
	}
	leadBAssertShape(t, res, b1)
	best := leadBBest(t, h, leadBMember{b0, 1}, leadBMember{b1, 1})
	if best.NodeID != b1 {
		t.Fatalf("fixture: the newer reply must be the group's best, got %s", best.NodeID)
	}
	leadBAssertArt(t, res.art(t, b0), 1, best.Score, "21.000001")
	want := []leadBLink{
		{Kind: "REFERENCES", Tag: "win-tag", Why: "win-why", Confidence: 0.8, From: j, To: b0},
		{Kind: "SAME_TOPIC", Topic: "tp-same", Why: "same-why", Confidence: 0.6, From: b0, To: j},
	}
	if got := res.art(t, b0).Links; !reflect.DeepEqual(got, want) {
		t.Errorf("links = %+v\nwant   %+v", got, want)
	}
}

// Equal scores order by node id ascending, in the full list and under
// max_items, and the group representative is its lowest-id member.
func TestLeadB5EqualScoresOrderByNodeID(t *testing.T) {
	pool, h := leadB5Setup(t)
	j := "jira:LBJ-3"
	leadBJira(t, pool, j, "seed", "2026-10-10T00:00:00Z")
	for _, id := range []string{"jira:LBE-3", "jira:LBE-1", "jira:LBE-2"} {
		leadBJira(t, pool, id, "e", "2026-10-10T00:00:00Z")
	}
	p0 := leadBMsg(t, pool, "LBA", "50.000001", "50.000001", "slack:LBA", "p", "2026-10-10T00:00:00Z", 0)
	q0 := leadBMsg(t, pool, "LBB", "60.000001", "60.000001", "slack:LBB", "q", "2026-10-10T00:00:00Z", 0)
	// A group of two equal replies, linked in descending id order; the root is
	// not reachable at depth 1.
	r0 := "slack:LBC:70.000001"
	leadBMsg(t, pool, "LBC", "70.000001", "70.000001", "slack:LBC", "r root", "2026-10-10T00:00:00Z", 0)
	r2 := leadBMsg(t, pool, "LBC", "72.000001", "70.000001", "slack:LBC", "r reply 2", "2026-10-10T00:00:00Z", 0)
	r1 := leadBMsg(t, pool, "LBC", "71.000001", "70.000001", "slack:LBC", "r reply 1", "2026-10-10T00:00:00Z", 0)
	for _, id := range []string{r2, "jira:LBE-3", q0, "jira:LBE-1", r1, p0, "jira:LBE-2"} {
		leadBEdge(t, pool, j, id, "REFERENCES", `{"confidence":0.5}`)
	}

	want := []string{j, "jira:LBE-1", "jira:LBE-2", "jira:LBE-3", p0, q0, r0}
	var first string
	for i := range 5 {
		res := leadB5Resolve(t, h, []string{j}, 1, nil)
		if got := res.ids(); !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: artifacts = %v\nwant %v", i, got, want)
		}
		leadBAssertShape(t, res, r1, r2)
		score := leadBScoreOf(t, h, "jira:LBE-1", 1)
		for _, a := range res.Artifacts[1:] {
			if !leadBClose(a.Score, score) {
				t.Fatalf("fixture: %s score %v differs from %v: the scores must tie", a.NodeID, a.Score, score)
			}
		}
		rep := res.art(t, r0)
		leadBAssertArt(t, rep, 1, leadBBest(t, h, leadBMember{r1, 1}, leadBMember{r2, 1}).Score, "71.000001")
		if i == 0 {
			first = res.ArtifactsRaw
		} else if res.ArtifactsRaw != first {
			t.Fatalf("run %d: artifacts differ between runs", i)
		}
	}
	capped := leadB5Resolve(t, h, []string{j}, 1, map[string]any{"max_items": 3})
	if got, want := capped.ids(), []string{j, "jira:LBE-1", "jira:LBE-2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("max_items=3: artifacts = %v, want %v", got, want)
	}
}

// A seed root stays hop 0 even when a reply of its thread scores higher; the
// group's score and breakdown are that reply's.
func TestLeadB5SeedRootWithHigherScoringMember(t *testing.T) {
	pool, h := leadB5Setup(t)
	a0 := leadBMsg(t, pool, "LBA", "10.000001", "10.000001", "slack:LBA", "old root", "2026-01-01T00:00:00Z", 0)
	a1 := leadBMsg(t, pool, "LBA", "11.000001", "10.000001", "slack:LBA", "fresh reply", "2026-10-11T00:00:00Z", 0)

	res := leadB5Resolve(t, h, []string{a0}, 1, nil)
	if got := res.ids(); !reflect.DeepEqual(got, []string{a0}) {
		t.Fatalf("artifacts = %v", got)
	}
	leadBAssertShape(t, res, a1)
	rootScore, replyBest := leadBScoreOf(t, h, a0, 0), leadBBest(t, h, leadBMember{a1, 1})
	if !(replyBest.Score > rootScore) {
		t.Fatalf("fixture: reply score %v must exceed the root's %v", replyBest.Score, rootScore)
	}
	a := res.art(t, a0)
	leadBAssertArt(t, a, 0, replyBest.Score, "11.000001")
	leadBJSONEqual(t, "score_breakdown", a.ScoreBreakdown, replyBest.Breakdown)
}

// A reply seed: the root is the artifact, the focus is the seed reply even
// when another reply scores higher; without a reply seed the focus is the
// best-scoring reply.
func TestLeadB5ReplySeedWithDifferentBestReply(t *testing.T) {
	pool, h := leadB5Setup(t)
	a0 := leadBMsg(t, pool, "LBA", "10.000001", "10.000001", "slack:LBA", "old root", "2026-01-01T00:00:00Z", 0)
	a1 := leadBMsg(t, pool, "LBA", "11.000001", "10.000001", "slack:LBA", "old reply", "2025-12-01T00:00:00Z", 0)
	a2 := leadBMsg(t, pool, "LBA", "12.000001", "10.000001", "slack:LBA", "fresh reply", "2026-10-11T00:00:00Z", 0)

	best := leadBBest(t, h, leadBMember{a0, 0}, leadBMember{a1, 1}, leadBMember{a2, 1})
	if best.NodeID != a2 {
		t.Fatalf("fixture: the fresh reply must be the best member, got %s", best.NodeID)
	}

	res := leadB5Resolve(t, h, []string{a1}, 1, nil)
	if got := res.ids(); !reflect.DeepEqual(got, []string{a0}) {
		t.Fatalf("artifacts = %v, want the root only", got)
	}
	leadBAssertShape(t, res, a1, a2)
	leadBAssertArt(t, res.art(t, a0), 0, best.Score, "11.000001")

	res = leadB5Resolve(t, h, []string{a0}, 1, nil)
	leadBAssertShape(t, res, a1, a2)
	leadBAssertArt(t, res.art(t, a0), 0, best.Score, "12.000001")
}

// A reply that is a candidate while its root is not (depth 1): the group is
// keyed by the root, and every thread-derived value comes from the root.
func TestLeadB5RootAbsentFromCandidates(t *testing.T) {
	pool, h := leadB5Setup(t)
	rootAuthor := leadBPerson(t, pool, "Root Author", "ULBROOT5", false)
	replyAuthor := leadBPerson(t, pool, "Reply Author", "ULBREPLY5", true)
	j := "jira:LBJ-4"
	leadBJira(t, pool, j, "seed", "2026-10-10T00:00:00Z")
	b0 := leadBMsg(t, pool, "LBB", "20.000001", "20.000001", "slack:LBB", "b root", "2026-10-02T00:00:00Z", rootAuthor)
	b1 := leadBMsg(t, pool, "LBB", "21.000001", "20.000001", "slack:LBB", "b reply", "2026-10-09T00:00:00Z", replyAuthor)
	leadBMsg(t, pool, "LBB", "22.000001", "20.000001", "slack:LBB", "b other reply", "2026-10-03T00:00:00Z", 0)
	aclExec(t, pool, `UPDATE graph.nodes SET title='Root Title' WHERE id=$1`, b0)
	leadBSummary(t, pool, "LBB", "20.000001", "root thread summary", "root overview")
	leadBEdge(t, pool, j, b1, "REFERENCES", `{"tag":"only-reply","confidence":0.4}`)

	res := leadB5Resolve(t, h, []string{j}, 1, nil)
	if got := res.ids(); !reflect.DeepEqual(got, []string{j, b0}) {
		t.Fatalf("artifacts = %v", got)
	}
	leadBAssertShape(t, res, b1, "slack:LBB:22.000001")
	a := res.art(t, b0)
	leadBAssertArt(t, a, 1, leadBScoreOf(t, h, b1, 1), "21.000001")
	if a.Title != "Root Title" || a.LeadSummary != "root thread summary" {
		t.Errorf("title %q lead_summary %q must come from the root", a.Title, a.LeadSummary)
	}
	if a.AuthorSlackID != "ULBROOT5" || a.AuthoredByBot != "human" || a.ChannelID != "LBB" {
		t.Errorf("author fields %q/%q/%q must be the root's", a.AuthorSlackID, a.AuthoredByBot, a.ChannelID)
	}
	if a.BotInThread != "yes" {
		t.Errorf("bot_in_thread = %q, want yes (the thread has a bot reply)", a.BotInThread)
	}
	if want := "2026-10-09T00:00:00Z"; a.LastActivityAt != want {
		t.Errorf("last_activity_at = %q, want %q (newest message of the thread)", a.LastActivityAt, want)
	}
	want := []leadBLink{{Kind: "REFERENCES", Tag: "only-reply", Confidence: 0.4, From: j, To: b0}}
	if !reflect.DeepEqual(a.Links, want) {
		t.Errorf("links = %+v, want %+v", a.Links, want)
	}
}

// No self-links: an edge inside a seed thread, an edge between two replies of
// one group, and an edge between two non-seed groups produce no link; the one
// seed-to-group edge does, between roots.
func TestLeadB5NoSelfLinks(t *testing.T) {
	pool, h := leadB5Setup(t)
	a0 := leadBMsg(t, pool, "LBA", "10.000001", "10.000001", "slack:LBA", "a root", "2026-10-10T00:00:00Z", 0)
	a1 := leadBMsg(t, pool, "LBA", "11.000001", "10.000001", "slack:LBA", "a reply", "2026-10-10T00:00:00Z", 0)
	b0 := leadBMsg(t, pool, "LBB", "20.000001", "20.000001", "slack:LBB", "b root", "2026-10-10T00:00:00Z", 0)
	b1 := leadBMsg(t, pool, "LBB", "21.000001", "20.000001", "slack:LBB", "b reply 1", "2026-10-10T00:00:00Z", 0)
	b2 := leadBMsg(t, pool, "LBB", "22.000001", "20.000001", "slack:LBB", "b reply 2", "2026-10-10T00:00:00Z", 0)
	c0 := leadBMsg(t, pool, "LBC", "40.000001", "40.000001", "slack:LBC", "c root", "2026-10-10T00:00:00Z", 0)
	c1 := leadBMsg(t, pool, "LBC", "41.000001", "40.000001", "slack:LBC", "c reply", "2026-10-10T00:00:00Z", 0)
	leadBEdge(t, pool, a1, a0, "REFERENCES", `{"tag":"inside-seed"}`)
	leadBEdge(t, pool, b1, b2, "REFERENCES", `{"tag":"inside-group"}`)
	leadBEdge(t, pool, b1, c1, "REFERENCES", `{"tag":"between-others"}`)
	leadBEdge(t, pool, a0, b2, "REFERENCES", `{"tag":"seed-to-group","confidence":0.5}`)

	res := leadB5Resolve(t, h, []string{a0}, 3, nil)
	got := res.ids()
	if !reflect.DeepEqual(got, []string{a0, b0, c0}) {
		t.Fatalf("artifacts = %v", got)
	}
	leadBAssertShape(t, res, a1, b1, b2, c1)
	if l := res.art(t, a0).Links; len(l) != 0 {
		t.Errorf("seed links = %+v", l)
	}
	if l := res.art(t, c0).Links; len(l) != 0 {
		t.Errorf("a group joined only to another non-seed group has links: %+v", l)
	}
	want := []leadBLink{{Kind: "REFERENCES", Tag: "seed-to-group", Confidence: 0.5, From: a0, To: b0}}
	if l := res.art(t, b0).Links; !reflect.DeepEqual(l, want) {
		t.Errorf("links of %s = %+v, want %+v", b0, l, want)
	}
}
