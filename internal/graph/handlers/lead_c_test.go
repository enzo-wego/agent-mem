package handlers

// R1a lead mode, acceptance items 6-9: search fields, selection and cache
// misses, the thread aggregate (and its index), bot fields. Every fixture is
// synthetic. Names carry the leadC prefix.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const leadCEnzoBot = "U0AG1DJP9K9"

// ---- fixtures ----------------------------------------------------------------

// leadCSetup opens the scratch DB, wipes graph rows and removes the rows lead
// fixtures add outside resetEpicACL's reach (thread summaries of LC* channels,
// extra answer-list bots). The migration seed row stays.
func leadCSetup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := openTestDB(t)
	resetEpicACL(t, pool)
	clean := func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM graph.thread_summaries WHERE channel_id LIKE 'LC%'`); err != nil {
			t.Errorf("clean thread_summaries: %v", err)
		}
		if _, err := pool.Exec(context.Background(), `DELETE FROM graph.answer_bots WHERE slack_user_id <> $1`, leadCEnzoBot); err != nil {
			t.Errorf("clean answer_bots: %v", err)
		}
	}
	clean()
	t.Cleanup(clean)
	return pool
}

func leadCPerson(t *testing.T, pool *pgxpool.Pool, name, slack string, bot bool) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `
INSERT INTO graph.people (display_name, slack_user_id, is_bot, machine_id)
VALUES ($1, NULLIF($2,''), $3, 'test') RETURNING id`, name, slack, bot).Scan(&id)
	if err != nil {
		t.Fatalf("leadCPerson %s: %v", name, err)
	}
	return id
}

func leadCAnswerBot(t *testing.T, pool *pgxpool.Pool, slack string) {
	t.Helper()
	aclExec(t, pool, `INSERT INTO graph.answer_bots (slack_user_id, note) VALUES ($1,'leadC test') ON CONFLICT DO NOTHING`, slack)
}

type leadCNodeSpec struct {
	id, typ, scope, title, body, meta, updated string
	author                                     int64
	deleted                                    bool
}

func leadCPut(t *testing.T, pool *pgxpool.Pool, n leadCNodeSpec) {
	t.Helper()
	if n.meta == "" {
		n.meta = "{}"
	}
	if n.updated == "" {
		n.updated = "2026-01-01T00:00:00Z"
	}
	aclExec(t, pool, `INSERT INTO graph.nodes (id,type,natural_key,title,body,scope,metadata,author_person_id,updated_at,created_at,deleted_at,machine_id)
VALUES ($1,$2,$1,NULLIF($3,''),NULLIF($4,''),NULLIF($5,''),$6::jsonb,NULLIF($7::bigint,0),$8::timestamptz,$8::timestamptz,CASE WHEN $9::boolean THEN now() END,'test')`,
		n.id, n.typ, n.title, n.body, n.scope, n.meta, n.author, n.updated, n.deleted)
}

func leadCMsgX(t *testing.T, pool *pgxpool.Pool, ch, ts, thread string, author int64, updated, body string, deleted bool) string {
	t.Helper()
	meta := map[string]string{"ts": ts}
	if thread != "" {
		meta["thread_ts"] = thread
	}
	mb, _ := json.Marshal(meta)
	id := "slack:" + ch + ":" + ts
	leadCPut(t, pool, leadCNodeSpec{id: id, typ: "slack", scope: "slack:" + ch, body: body, meta: string(mb), updated: updated, author: author, deleted: deleted})
	return id
}

func leadCMsg(t *testing.T, pool *pgxpool.Pool, ch, ts, thread string, author int64, updated, body string) string {
	t.Helper()
	return leadCMsgX(t, pool, ch, ts, thread, author, updated, body, false)
}

func leadCJira(t *testing.T, pool *pgxpool.Pool, key, title, updated string, author int64) string {
	t.Helper()
	id := "jira:" + key
	leadCPut(t, pool, leadCNodeSpec{id: id, typ: "jira", scope: "public", title: title, updated: updated, author: author})
	return id
}

func leadCIndex(t *testing.T, pool *pgxpool.Pool, id, summary string) {
	t.Helper()
	aclExec(t, pool, `INSERT INTO graph.artifact_index (node_id, summary, summary_kind, machine_id) VALUES ($1,$2,'heuristic','test')
ON CONFLICT (node_id) DO UPDATE SET summary = EXCLUDED.summary`, id, summary)
}

func leadCBody(t *testing.T, pool *pgxpool.Pool, id, body string) {
	t.Helper()
	aclExec(t, pool, `INSERT INTO graph.artifact_bodies (node_id, body_full, machine_id) VALUES ($1,$2,'test')
ON CONFLICT (node_id) DO UPDATE SET body_full = EXCLUDED.body_full`, id, body)
}

func leadCThreadSummary(t *testing.T, pool *pgxpool.Pool, ch, ts, summary, overview string) {
	t.Helper()
	aclExec(t, pool, `INSERT INTO graph.thread_summaries (channel_id, thread_ts, signature, summary, overview) VALUES ($1,$2,'sig',$3,$4)
ON CONFLICT (channel_id, thread_ts) DO UPDATE SET summary = EXCLUDED.summary, overview = EXCLUDED.overview`, ch, ts, summary, overview)
}

func leadCEdge(t *testing.T, pool *pgxpool.Pool, from, to, kind string) {
	t.Helper()
	aclExec(t, pool, `INSERT INTO graph.edges (from_node_id,to_node_id,kind,metadata,machine_id) VALUES ($1,$2,$3,'{}','test')`, from, to, kind)
}

// ---- requests ------------------------------------------------------------------

// leadCReq builds a lead-mode request (header X-Graph-Lead: 1) as admin, or as
// the filtered asker when asker is set. hdr is key,value pairs; an empty value
// removes the header.
func leadCReq(method, target, body, asker string, hdr ...string) *http.Request {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := adminRequest(method, target, rd)
	if asker != "" {
		setAsker(r, asker)
	}
	r.Header.Set(leadHeader, "1")
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i+1] == "" {
			r.Header.Del(hdr[i])
		} else {
			r.Header.Set(hdr[i], hdr[i+1])
		}
	}
	return r
}

func leadCServe(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func leadCDo(h http.Handler, method, target, body, asker string, hdr ...string) *httptest.ResponseRecorder {
	return leadCServe(h, leadCReq(method, target, body, asker, hdr...))
}

func leadCDecode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %q", w.Code, w.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return m
}

func leadCList(t *testing.T, top map[string]any, key string) []map[string]any {
	t.Helper()
	raw, ok := top[key].([]any)
	if !ok {
		t.Fatalf("%s is %T, want array (top=%v)", key, top[key], top)
	}
	out := make([]map[string]any, len(raw))
	for i, v := range raw {
		out[i] = v.(map[string]any)
	}
	return out
}

func leadCIDs(items []map[string]any, idKey string) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i], _ = it[idKey].(string)
	}
	return out
}

func leadCByID(items []map[string]any, idKey string) map[string]map[string]any {
	out := make(map[string]map[string]any, len(items))
	for _, it := range items {
		id, _ := it[idKey].(string)
		out[id] = it
	}
	return out
}

func leadCStr(t *testing.T, it map[string]any, key string) string {
	t.Helper()
	v, ok := it[key]
	if !ok {
		t.Errorf("%v: key %q missing", it["node_id"], key)
		return ""
	}
	s, ok := v.(string)
	if !ok {
		t.Errorf("%v: key %q is %T, want string", it["node_id"], key, v)
	}
	return s
}

// leadCCheckFields asserts the A4 contract on one item: every field present
// with its documented type and value domain; bot_in_thread only for Slack.
func leadCCheckFields(t *testing.T, it map[string]any, slackMessage bool) {
	t.Helper()
	id, _ := it["node_id"].(string)
	if s := leadCStr(t, it, "lead_summary"); utf8.RuneCountInString(s) > leadCCap {
		t.Errorf("%s: lead_summary has %d runes, want <= %d", id, utf8.RuneCountInString(s), leadCCap)
	}
	last := leadCStr(t, it, "last_activity_at")
	if ts, err := time.Parse(time.RFC3339, last); err != nil || !strings.HasSuffix(last, "Z") || ts.IsZero() {
		t.Errorf("%s: last_activity_at %q is not RFC3339 UTC (%v)", id, last, err)
	}
	leadCStr(t, it, "author_slack_id")
	leadCStr(t, it, "channel_id")
	switch v := leadCStr(t, it, "authored_by_bot"); v {
	case "bot", "human", "unknown":
	default:
		t.Errorf("%s: authored_by_bot %q outside bot/human/unknown", id, v)
	}
	if slackMessage {
		switch v := leadCStr(t, it, "bot_in_thread"); v {
		case "yes", "no", "unknown":
		default:
			t.Errorf("%s: bot_in_thread %q outside yes/no/unknown", id, v)
		}
	} else if v, ok := it["bot_in_thread"]; ok {
		t.Errorf("%s: non-Slack item carries bot_in_thread=%v", id, v)
	}
}

const leadCCap = 600

// ---- restricted / traced pools -------------------------------------------------

// leadCTracer records every query (text and args) and optionally calls hook at
// query start, so a test can cancel a request at an exact query.
type leadCTracer struct {
	mu      sync.Mutex
	queries []leadCQuery
	hook    func(sql string)
}

type leadCQuery struct {
	sql  string
	args []any
}

func (c *leadCTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	c.mu.Lock()
	c.queries = append(c.queries, leadCQuery{sql: d.SQL, args: append([]any(nil), d.Args...)})
	hook := c.hook
	c.mu.Unlock()
	if hook != nil {
		hook(d.SQL)
	}
	return ctx
}

func (*leadCTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (c *leadCTracer) with(substr string) []leadCQuery {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []leadCQuery
	for _, q := range c.queries {
		if strings.Contains(q.sql, substr) {
			out = append(out, q)
		}
	}
	return out
}

func (c *leadCTracer) reset() {
	c.mu.Lock()
	c.queries = nil
	c.mu.Unlock()
}

// leadCRole creates a NOLOGIN role that can read the graph and public schemas
// except the denied tables, and drops it again. Handlers run against a pool
// that does SET ROLE to it, so a denied table makes exactly the queries that
// touch it fail: a deterministic loader error that nothing in the handler can
// route around.
func leadCRole(t *testing.T, admin *pgxpool.Pool, denied ...string) string {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("leadc_r_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{name}.Sanitize()
	aclExec(t, admin, `CREATE ROLE `+quoted+` NOLOGIN`)
	t.Cleanup(func() {
		for _, q := range []string{`DROP OWNED BY ` + quoted, `DROP ROLE ` + quoted} {
			if _, err := admin.Exec(ctx, q); err != nil {
				t.Errorf("%s: %v", q, err)
			}
		}
	})
	aclExec(t, admin, `GRANT USAGE ON SCHEMA graph, public TO `+quoted)
	aclExec(t, admin, `GRANT SELECT ON ALL TABLES IN SCHEMA graph, public TO `+quoted)
	for _, table := range denied {
		aclExec(t, admin, `REVOKE SELECT ON `+table+` FROM `+quoted)
	}
	return name
}

// leadCPool opens another pool on the scratch DB with an optional tracer and
// an optional SET ROLE. It closes before the role is dropped (LIFO cleanup).
func leadCPool(t *testing.T, tracer pgx.QueryTracer, role string) *pgxpool.Pool {
	t.Helper()
	cfg, err := parseScratchDSN(os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	if tracer != nil {
		cfg.ConnConfig.Tracer = tracer
	}
	if role != "" {
		quoted := pgx.Identifier{role}.Sanitize()
		cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
			_, err := c.Exec(ctx, `SET ROLE `+quoted)
			return err
		}
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// ---- handlers --------------------------------------------------------------------

type leadCSpy struct {
	mu  sync.Mutex
	ids []string
}

func (s *leadCSpy) record(_ context.Context, id string) {
	s.mu.Lock()
	s.ids = append(s.ids, id)
	s.mu.Unlock()
}

func (s *leadCSpy) drain() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]string(nil), s.ids...)
	s.ids = nil
	sort.Strings(out)
	return out
}

func leadCResolveH(t *testing.T, pool *pgxpool.Pool) (*Resolve, *leadCSpy) {
	t.Helper()
	h, err := NewResolve(pool)
	if err != nil {
		t.Fatal(err)
	}
	spy := &leadCSpy{}
	h.enqueueFetchBodyFn = spy.record
	return h.WithLeadMode(true), spy
}

func leadCNodeH(pool *pgxpool.Pool) *Node { return NewNode(pool).WithLeadMode(true) }

func leadCSearchH(t *testing.T, pool *pgxpool.Pool) *Search {
	t.Helper()
	s, err := NewSearch(pool)
	if err != nil {
		t.Fatal(err)
	}
	return s.WithLeadMode(true)
}

func leadCNodeTarget(id string) string {
	return "/api/graph/node?id=" + strings.ReplaceAll(id, ":", "%3A")
}

func leadCResolveBody(seeds []string, extra string) string {
	b, _ := json.Marshal(seeds)
	if extra != "" {
		extra = "," + extra
	}
	return `{"seeds":` + string(b) + extra + `}`
}

// leadCText builds a summary whose email crosses rune 600 unmasked but sits
// whole before the cut once masked, followed by tail text so both variants
// exceed 600 runes.
func leadCText() string {
	return strings.Repeat("a", 585) + " leadcmask@example.com " + strings.Repeat("b", 40)
}

// ======================================================================================
// Item 6: search in lead mode
// ======================================================================================

const leadCAsker = "leadc-asker@example.com"

type leadCSearchIDs struct {
	rootA, replyA, rootB, replyB, rootC, rootM string
	replyD, rootD                              string
	jira1, jira2, file                         string
	hiddenRoot, hiddenReply                    string
}

func leadCSearchFixture(t *testing.T, pool *pgxpool.Pool) leadCSearchIDs {
	t.Helper()
	const tok = "leadcsrch"
	hana := leadCPerson(t, pool, "Lead C Hana", "ULCHANA", false)
	enzo := leadCPerson(t, pool, "Lead C EnzoBot", leadCEnzoBot, false)
	gen := leadCPerson(t, pool, "Lead C Generic Bot", "ULCGEN", true)
	var id leadCSearchIDs

	// Thread A: lead_summary is the root's artifact_index summary, never the
	// thread-summary topic; the bot reply is the latest message.
	id.rootA = leadCMsg(t, pool, "LCS1", "2000.000001", "2000.000001", hana, "2026-03-01T10:00:00Z", tok+" alpha root")
	id.replyA = leadCMsg(t, pool, "LCS1", "2000.000002", "2000.000001", enzo, "2026-03-02T11:30:00Z", tok+" alpha reply")
	leadCThreadSummary(t, pool, "LCS1", "2000.000001", "LCSA thread summary", "LCSA thread overview")
	leadCIndex(t, pool, id.rootA, "LCSA root index summary")
	leadCIndex(t, pool, id.replyA, "LCSA reply index summary")

	// Thread B: no thread summary; lead_summary is still the root's artifact_index summary.
	id.rootB = leadCMsg(t, pool, "LCS1", "3000.000001", "3000.000001", hana, "2026-03-05T08:00:00Z", tok+" bravo root")
	id.replyB = leadCMsg(t, pool, "LCS1", "3000.000002", "3000.000001", hana, "2026-03-05T09:00:00Z", tok+" bravo reply")
	leadCIndex(t, pool, id.rootB, "LCSB fallback index summary")

	// Thread C: neither source, lead_summary is empty.
	id.rootC = leadCMsg(t, pool, "LCS1", "4000.000001", "4000.000001", hana, "2026-03-06T08:00:00Z", tok+" charlie root")

	// Thread M: long text with an email crossing rune 600 in every source.
	id.rootM = leadCMsg(t, pool, "LCS1", "7000.000001", "7000.000001", hana, "2026-03-07T08:00:00Z", tok+" mask root")
	long := leadCText()
	leadCThreadSummary(t, pool, "LCS1", "7000.000001", long, long)
	leadCIndex(t, pool, id.rootM, long)

	// Thread D: root deleted, the reply is a member-only row.
	id.rootD = leadCMsgX(t, pool, "LCS1", "5000.000001", "5000.000001", hana, "2026-03-08T08:00:00Z", tok+" delta root", true)
	id.replyD = leadCMsg(t, pool, "LCS1", "5000.000002", "5000.000001", hana, "2026-03-08T09:00:00Z", tok+" delta reply")

	// Non-Slack items and a Slack file.
	id.jira1 = leadCJira(t, pool, "LCS-1", tok+" jira one", "2026-03-09T00:00:00Z", 0)
	leadCIndex(t, pool, id.jira1, "LCSJ jira index summary")
	id.jira2 = leadCJira(t, pool, "LCS-2", tok+" jira two", "2026-03-10T00:00:00Z", gen)
	id.file = "slack_file:FLCS1"
	leadCPut(t, pool, leadCNodeSpec{id: id.file, typ: "slack_file", scope: "slack:LCS1", title: tok + " file", updated: "2026-03-11T00:00:00Z"})
	leadCEdge(t, pool, id.rootA, id.file, "REFERENCES")

	// A channel the filtered asker is not in.
	id.hiddenRoot = leadCMsg(t, pool, "LCH1", "8000.000001", "8000.000001", hana, "2026-03-12T08:00:00Z", tok+" hidden root")
	id.hiddenReply = leadCMsg(t, pool, "LCH1", "8000.000002", "8000.000001", hana, "2026-03-12T09:00:00Z", tok+" hidden reply")
	leadCThreadSummary(t, pool, "LCH1", "8000.000001", "LCHSENTINEL hidden summary", "LCHSENTINEL hidden overview")

	aclPerson(t, pool, 6101, leadCAsker, "slack:LCS1")
	return id
}

func leadCCheckSearchRows(t *testing.T, rows []map[string]any, id leadCSearchIDs, who string, hybrid bool) {
	t.Helper()
	by := leadCByID(rows, "node_id")
	need := func(nodeID string) map[string]any {
		it, ok := by[nodeID]
		if !ok {
			t.Fatalf("%s: %s missing from results %v", who, nodeID, leadCIDs(rows, "node_id"))
		}
		return it
	}
	for _, it := range rows {
		typ, _ := it["type"].(string)
		leadCCheckFields(t, it, typ == "slack")
	}

	// Thread A (the bot reply is the latest message).
	a := need(id.rootA)
	if got := a["lead_summary"]; got != "LCSA root index summary" {
		t.Errorf("%s root A lead_summary = %v, want the root's artifact_index summary (not the thread-summary topic)", who, got)
	}
	for _, k := range []string{"author_slack_id", "authored_by_bot", "bot_in_thread", "channel_id", "last_activity_at"} {
		want := map[string]string{"author_slack_id": "ULCHANA", "authored_by_bot": "human", "bot_in_thread": "yes", "channel_id": "LCS1", "last_activity_at": "2026-03-02T11:30:00Z"}[k]
		if a[k] != want {
			t.Errorf("%s root A %s = %v, want %s", who, k, a[k], want)
		}
	}
	if hybrid {
		if a["summary"] != "LCSA thread overview" {
			t.Errorf("%s hybrid root A summary = %v, want the overview", who, a["summary"])
		}
	} else {
		if a["summary"] != "LCSA root index summary" {
			t.Errorf("%s default root A summary = %v, want the index summary", who, a["summary"])
		}
		r := need(id.replyA)
		// A reply keeps its own id but every thread value comes from the root.
		if r["lead_summary"] != "LCSA root index summary" || r["author_slack_id"] != "ULCHANA" || r["authored_by_bot"] != "human" ||
			r["bot_in_thread"] != "yes" || r["last_activity_at"] != "2026-03-02T11:30:00Z" || r["channel_id"] != "LCS1" {
			t.Errorf("%s reply A fields not taken from the root: %v", who, r)
		}
		if r["summary"] != "LCSA reply index summary" {
			t.Errorf("%s reply A summary = %v, want its own index summary", who, r["summary"])
		}
	}

	// Thread B: artifact_index fallback of the root.
	b := need(id.rootB)
	if b["lead_summary"] != "LCSB fallback index summary" || b["bot_in_thread"] != "no" || b["last_activity_at"] != "2026-03-05T09:00:00Z" {
		t.Errorf("%s root B: %v", who, b)
	}
	if !hybrid {
		if r := need(id.replyB); r["lead_summary"] != "LCSB fallback index summary" {
			t.Errorf("%s reply B lead_summary = %v, want the root's index summary", who, r["lead_summary"])
		}
	}

	// Thread C: no summary anywhere.
	if c := need(id.rootC); c["lead_summary"] != "" {
		t.Errorf("%s root C lead_summary = %q, want empty", who, c["lead_summary"])
	}

	// Jira items: own node only.
	j1 := need(id.jira1)
	if j1["lead_summary"] != "LCSJ jira index summary" || j1["authored_by_bot"] != "unknown" || j1["author_slack_id"] != "" ||
		j1["channel_id"] != "" || j1["last_activity_at"] != "2026-03-09T00:00:00Z" {
		t.Errorf("%s jira 1: %v", who, j1)
	}
	j2 := need(id.jira2)
	if j2["lead_summary"] != "" || j2["authored_by_bot"] != "bot" || j2["author_slack_id"] != "ULCGEN" {
		t.Errorf("%s jira 2: %v", who, j2)
	}
	if f := need(id.file); f["channel_id"] != "LCS1" || f["lead_summary"] != "" {
		t.Errorf("%s slack file: %v", who, f)
	}

	// Member-only rows: the root is deleted, so the reply is dropped.
	for _, gone := range []string{id.replyD, id.rootD} {
		if _, ok := by[gone]; ok {
			t.Errorf("%s: %s must be dropped in lead mode", who, gone)
		}
	}
}

func leadCCheckMasked(t *testing.T, rows []map[string]any, rootM, who string, admin bool) {
	t.Helper()
	m := leadCByID(rows, "node_id")[rootM]
	if m == nil {
		t.Fatalf("%s: masked root missing", who)
	}
	for _, k := range []string{"summary", "lead_summary"} {
		s, _ := m[k].(string)
		if n := utf8.RuneCountInString(s); n != leadCCap || !strings.HasSuffix(s, "…") {
			t.Errorf("%s %s: %d runes, suffix ok=%v; want exactly %d ending in …", who, k, n, strings.HasSuffix(s, "…"), leadCCap)
		}
		if admin {
			if !strings.Contains(s, "leadcmask@") || strings.Contains(s, "[email]") {
				t.Errorf("%s %s: admin text must stay unmasked", who, k)
			}
			continue
		}
		if !strings.Contains(s, "[email]") || strings.Contains(s, "@") || strings.Contains(s, "example") || strings.Contains(s, "leadcmask") {
			t.Errorf("%s %s: not masked before the cut: %q", who, k, s[570:])
		}
	}
}

func TestLeadC_SearchDefaultFields(t *testing.T) {
	pool := leadCSetup(t)
	id := leadCSearchFixture(t, pool)
	h := leadCSearchH(t, pool)
	target := "/api/graph/search?q=leadcsrch&limit=50"

	// Control: without the header the member-only reply is returned and no lead field exists.
	ctl := leadCDecode(t, leadCDo(h, "GET", target, "", "", leadHeader, ""))
	ctlRows := leadCList(t, ctl, "results")
	if _, ok := leadCByID(ctlRows, "node_id")[id.replyD]; !ok {
		t.Fatalf("control: reply of the deleted root should be a plain search hit, got %v", leadCIDs(ctlRows, "node_id"))
	}
	for _, it := range ctlRows {
		if _, ok := it["lead_summary"]; ok {
			t.Fatalf("lead field on a non-lead request: %v", it)
		}
	}

	for _, who := range []struct{ name, asker string }{{"admin", ""}, {"filtered", leadCAsker}} {
		t.Run(who.name, func(t *testing.T) {
			top := leadCDecode(t, leadCDo(h, "GET", target, "", who.asker))
			rows := leadCList(t, top, "results")
			leadCCheckSearchRows(t, rows, id, who.name, false)
			leadCCheckMasked(t, rows, id.rootM, who.name, who.asker == "")
			by := leadCByID(rows, "node_id")
			if who.asker == "" {
				if _, ok := by[id.hiddenRoot]; !ok {
					t.Errorf("admin must see the hidden-channel root")
				}
				return
			}
			raw, _ := json.Marshal(top)
			if s := string(raw); strings.Contains(s, "LCH1") || strings.Contains(s, "LCHSENTINEL") {
				t.Errorf("filtered asker saw the hidden channel: %s", s)
			}
		})
	}
}

func TestLeadC_SearchHybridFields(t *testing.T) {
	pool := leadCSetup(t)
	id := leadCSearchFixture(t, pool)
	h := leadCSearchH(t, pool)
	target := "/api/graph/search?q=leadcsrch&match=hybrid&limit=50"

	// Control: hybrid's member fallback keeps the reply of a deleted root today.
	ctl := leadCDecode(t, leadCDo(h, "GET", target, "", "", leadHeader, ""))
	ctlRows := leadCList(t, ctl, "results")
	if _, ok := leadCByID(ctlRows, "node_id")[id.replyD]; !ok {
		t.Fatalf("control: hybrid member fallback row %s missing: %v", id.replyD, leadCIDs(ctlRows, "node_id"))
	}
	for _, it := range ctlRows {
		if _, ok := it["lead_summary"]; ok {
			t.Fatalf("lead field on a non-lead hybrid request: %v", it)
		}
	}

	for _, who := range []struct{ name, asker string }{{"admin", ""}, {"filtered", leadCAsker}} {
		t.Run(who.name, func(t *testing.T) {
			top := leadCDecode(t, leadCDo(h, "GET", target, "", who.asker))
			rows := leadCList(t, top, "results")
			leadCCheckSearchRows(t, rows, id, who.name, true)
			leadCCheckMasked(t, rows, id.rootM, who.name, who.asker == "")
			if got, want := top["total"], float64(len(rows)); got != want {
				t.Errorf("total = %v, want %v", got, want)
			}
			// Hybrid folds a thread to one row: root A once, no reply row.
			if _, ok := leadCByID(rows, "node_id")[id.replyA]; ok {
				t.Errorf("hybrid returned the reply row of a thread whose root is eligible")
			}
			if who.asker != "" {
				raw, _ := json.Marshal(top)
				if s := string(raw); strings.Contains(s, "LCH1") || strings.Contains(s, "LCHSENTINEL") {
					t.Errorf("filtered asker saw the hidden channel: %s", s)
				}
			}
		})
	}
}

// ======================================================================================
// Item 7: selection and misses
// ======================================================================================

func TestLeadC_ResolveBodylessAndOversizeSelected(t *testing.T) {
	pool := leadCSetup(t)
	seed := leadCJira(t, pool, "LCB-SEED", "bodyless seed", "2026-03-01T00:00:00Z", 0)
	nobody := leadCJira(t, pool, "LCB-NOBODY", "no cached body", "2026-03-01T00:00:00Z", 0)
	huge := leadCJira(t, pool, "LCB-HUGE", "oversize body", "2026-03-01T00:00:00Z", 0)
	small := leadCJira(t, pool, "LCB-SMALL", "small body", "2026-03-01T00:00:00Z", 0)
	leadCIndex(t, pool, huge, "huge item summary")
	leadCBody(t, pool, seed, "seed body")
	leadCBody(t, pool, huge, strings.Repeat("x ", 100000))
	leadCBody(t, pool, small, "small body text")
	for _, to := range []string{nobody, huge, small} {
		leadCEdge(t, pool, seed, to, "REFERENCES")
	}
	h, spy := leadCResolveH(t, pool)

	body := leadCResolveBody([]string{seed}, `"include_bodies":true,"budget_tokens":1`)
	lead := leadCDo(h, "POST", "/api/graph/resolve", body, "")
	top := leadCDecode(t, lead)
	arts := leadCList(t, top, "artifacts")
	got := leadCIDs(arts, "node_id")
	want := []string{seed, huge, nobody, small}
	if g := append([]string(nil), got...); !sameSet(g, want) || len(got) != len(want) {
		t.Fatalf("lead artifacts = %v, want exactly %v (bodyless and oversize selected under budget_tokens=1)", got, want)
	}
	if strings.Contains(lead.Body.String(), `"body"`) {
		t.Errorf("lead mode returned a body despite include_bodies: %s", lead.Body.String())
	}
	tokens := 0
	for _, a := range arts {
		if _, ok := a["body"]; ok {
			t.Errorf("%v has a body key", a["node_id"])
		}
		leadCCheckFields(t, a, false)
		tokens += (utf8.RuneCountInString(a["title"].(string)) + utf8.RuneCountInString(a["lead_summary"].(string)) + 3) / 4
	}
	if top["context_tokens"] != float64(tokens) {
		t.Errorf("context_tokens = %v, want ceil((title+lead_summary)/4) summed = %d", top["context_tokens"], tokens)
	}
	if !reflect.DeepEqual(toStrings(top["cache_misses"]), []string{nobody}) {
		t.Errorf("cache_misses = %v, want [%s]", top["cache_misses"], nobody)
	}
	if got := spy.drain(); !reflect.DeepEqual(got, []string{nobody}) {
		t.Errorf("enqueued %v, want [%s]", got, nobody)
	}

	// The same request without the opt-in header: the default path drops the
	// bodyless item under the tiny budget, which lead mode must not do.
	plain := leadCDecode(t, leadCDo(h, "POST", "/api/graph/resolve", body, "", leadHeader, ""))
	plainIDs := leadCIDs(leadCList(t, plain, "artifacts"), "node_id")
	for _, id := range plainIDs {
		if id == nobody {
			t.Errorf("default path kept the bodyless item %s; the comparison is moot (%v)", nobody, plainIDs)
		}
	}
	if len(plainIDs) >= len(got) {
		t.Errorf("default path returned %v with budget_tokens=1; lead mode %v must return more", plainIDs, got)
	}
}

func sameSet(a, b []string) bool {
	a, b = append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(a)
	sort.Strings(b)
	return reflect.DeepEqual(a, b)
}

func toStrings(v any) []string {
	raw, _ := v.([]any)
	out := make([]string, len(raw))
	for i, s := range raw {
		out[i], _ = s.(string)
	}
	return out
}

func TestLeadC_ResolveCacheMissesAndEnqueue(t *testing.T) {
	pool := leadCSetup(t)
	hana := leadCPerson(t, pool, "Lead C Hana", "ULCHANA", false)
	seed := leadCJira(t, pool, "LCM-SEED", "miss seed", "2026-03-01T00:00:00Z", 0)
	cached := leadCJira(t, pool, "LCM-CACHED", "cached item", "2026-03-01T00:00:00Z", 0)
	miss := leadCJira(t, pool, "LCM-MISS", "uncached item", "2026-03-01T00:00:00Z", 0)
	// T1: root cached, reply not -> the reply folds into a cached root: no miss.
	r1 := leadCMsg(t, pool, "LCM1", "100.000001", "100.000001", hana, "2026-03-01T00:00:00Z", "t1 root")
	p1 := leadCMsg(t, pool, "LCM1", "100.000002", "100.000001", hana, "2026-03-01T00:00:00Z", "t1 reply")
	// T2: root not cached, reply cached -> the item is the root: a miss.
	r2 := leadCMsg(t, pool, "LCM1", "200.000001", "200.000001", hana, "2026-03-01T00:00:00Z", "t2 root")
	p2 := leadCMsg(t, pool, "LCM1", "200.000002", "200.000001", hana, "2026-03-01T00:00:00Z", "t2 reply")
	// T3: root cached and seeded directly.
	r3 := leadCMsg(t, pool, "LCM1", "300.000001", "300.000001", hana, "2026-03-01T00:00:00Z", "t3 root")
	for _, id := range []string{seed, cached, r1, p2, r3} {
		leadCBody(t, pool, id, "cached body of "+id)
	}
	for _, to := range []string{cached, miss, p1, p2, r3} {
		leadCEdge(t, pool, seed, to, "REFERENCES")
	}
	wantMiss := []string{miss, r2}
	h, spy := leadCResolveH(t, pool)
	body := leadCResolveBody([]string{seed}, "")

	check := func(t *testing.T, w *httptest.ResponseRecorder) {
		t.Helper()
		top := leadCDecode(t, w)
		arts := leadCList(t, top, "artifacts")
		ids := leadCIDs(arts, "node_id")
		if !sameSet(ids, []string{seed, cached, miss, r1, r2, r3}) {
			t.Fatalf("artifacts = %v, want the seed, the two jira items and the three thread roots (no reply ids)", ids)
		}
		// Exact list, in selection (artifact) order.
		var ordered []string
		for _, id := range ids {
			if id == miss || id == r2 {
				ordered = append(ordered, id)
			}
		}
		if got := toStrings(top["cache_misses"]); !reflect.DeepEqual(got, ordered) || !sameSet(got, wantMiss) {
			t.Fatalf("cache_misses = %v, want %v in selection order", got, ordered)
		}
	}

	cases := []struct {
		name    string
		body    string
		hdr     []string
		enqueue bool
	}{
		{"no header", body, nil, true},
		{"header 1", body, []string{"X-Graph-No-Side-Effects", "1"}, false},
		{"header true", body, []string{"X-Graph-No-Side-Effects", "true"}, false},
		{"header TRUE", body, []string{"X-Graph-No-Side-Effects", "TRUE"}, false},
		{"body flag only", leadCResolveBody([]string{seed}, `"no_side_effects":true`), nil, false},
		{"header 0 overrides body flag", leadCResolveBody([]string{seed}, `"no_side_effects":true`), []string{"X-Graph-No-Side-Effects", "0"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spy.drain()
			check(t, leadCDo(h, "POST", "/api/graph/resolve", tc.body, "", tc.hdr...))
			got := spy.drain()
			if tc.enqueue && !sameSet(got, wantMiss) {
				t.Errorf("enqueued %v, want %v", got, wantMiss)
			}
			if !tc.enqueue && len(got) != 0 {
				t.Errorf("enqueued %v despite no-side-effects", got)
			}
		})
	}
}

func TestLeadC_ResolveMissQueryFailure(t *testing.T) {
	pool := leadCSetup(t)
	seed := leadCJira(t, pool, "LCF-SEED", "fail seed", "2026-03-01T00:00:00Z", 0)
	other := leadCJira(t, pool, "LCF-OTHER", "fail other", "2026-03-01T00:00:00Z", 0)
	leadCEdge(t, pool, seed, other, "REFERENCES")
	aclPerson(t, pool, 6102, "leadc-fail@example.com")

	body := leadCResolveBody([]string{seed}, "")
	okH, _ := leadCResolveH(t, pool)
	if w := leadCDo(okH, "POST", "/api/graph/resolve", body, ""); w.Code != http.StatusOK {
		t.Fatalf("control on the healthy pool: %d %s", w.Code, w.Body.String())
	}

	role := leadCRole(t, pool, "graph.artifact_bodies")
	bad := leadCPool(t, nil, role)
	h, spy := leadCResolveH(t, bad)
	for _, asker := range []string{"", "leadc-fail@example.com"} {
		w := leadCDo(h, "POST", "/api/graph/resolve", body, asker)
		if w.Code != http.StatusServiceUnavailable || strings.TrimSpace(w.Body.String()) != "lead_lookup_failed" {
			t.Errorf("asker %q: status %d body %q, want 503 lead_lookup_failed", asker, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "LCF-") {
			t.Errorf("error body leaks node ids: %q", w.Body.String())
		}
	}
	if got := spy.drain(); len(got) != 0 {
		t.Errorf("enqueued %v after a failed miss query", got)
	}
	// The same pool serves the default path without the header: only the lead
	// query failed, nothing else was broken by the role.
	if w := leadCDo(h, "POST", "/api/graph/resolve", body, "", leadHeader, ""); w.Code != http.StatusOK {
		t.Errorf("default path on the restricted pool: %d %s", w.Code, w.Body.String())
	}
}

func leadCSeedFixture(t *testing.T, pool *pgxpool.Pool) (seeds, others []string) {
	t.Helper()
	for i := 1; i <= 3; i++ {
		seeds = append(seeds, leadCJira(t, pool, fmt.Sprintf("LCQ-S%d", i), "seed", fmt.Sprintf("2026-03-0%dT00:00:00Z", i), 0))
	}
	// Distinct recency: O1 newest. Later items are older, so scores strictly fall.
	for i := 1; i <= 5; i++ {
		id := leadCJira(t, pool, fmt.Sprintf("LCQ-O%d", i), "other", time.Now().UTC().AddDate(0, 0, -10*i).Format(time.RFC3339), 0)
		others = append(others, id)
		leadCEdge(t, pool, seeds[0], id, "REFERENCES")
	}
	return seeds, others
}

func TestLeadC_ResolveSeedsVersusMaxItems(t *testing.T) {
	pool := leadCSetup(t)
	seeds, others := leadCSeedFixture(t, pool)
	h, _ := leadCResolveH(t, pool)
	run := func(extra string) []map[string]any {
		t.Helper()
		return leadCList(t, leadCDecode(t, leadCDo(h, "POST", "/api/graph/resolve", leadCResolveBody(seeds, extra), "")), "artifacts")
	}
	for _, tc := range []struct {
		name  string
		extra string
		want  []string
	}{
		{"seeds exceed max_items", `"max_items":2`, nil},
		{"seeds equal max_items", `"max_items":3`, nil},
		{"one slot left", `"max_items":4`, others[:1]},
		{"two slots left", `"max_items":5`, others[:2]},
		{"default 30", ``, others},
		{"max_items null means default", `"max_items":null`, others},
		{"max_items 100", `"max_items":100`, others},
	} {
		t.Run(tc.name, func(t *testing.T) {
			arts := run(tc.extra)
			ids := leadCIDs(arts, "node_id")
			if len(ids) != len(seeds)+len(tc.want) {
				t.Fatalf("got %v, want the %d seeds then %v", ids, len(seeds), tc.want)
			}
			if !sameSet(ids[:len(seeds)], seeds) {
				t.Errorf("first %d items %v, want the seeds %v", len(seeds), ids[:len(seeds)], seeds)
			}
			if got := ids[len(seeds):]; len(got) != len(tc.want) || (len(got) > 0 && !reflect.DeepEqual(got, tc.want)) {
				t.Errorf("non-seed items %v, want %v (best score first)", ids[len(seeds):], tc.want)
			}
			for i, a := range arts {
				wantHop := 1.0
				if i < len(seeds) {
					wantHop = 0
				}
				if a["hop"] != wantHop {
					t.Errorf("%v hop = %v, want %v", a["node_id"], a["hop"], wantHop)
				}
			}
		})
	}
}

func TestLeadC_ResolveMaxItemsValidation(t *testing.T) {
	pool := leadCSetup(t)
	seeds, _ := leadCSeedFixture(t, pool)
	h, _ := leadCResolveH(t, pool)
	bad := []string{`0`, `-1`, `101`, `1000000`, `"5"`, `1.5`, `true`, `[]`, `{}`, `"x"`}
	for _, v := range bad {
		w := leadCDo(h, "POST", "/api/graph/resolve", leadCResolveBody(seeds, `"max_items":`+v), "")
		if w.Code != http.StatusBadRequest || strings.TrimSpace(w.Body.String()) != "bad_max_items" {
			t.Errorf("max_items=%s: status %d body %q, want 400 bad_max_items", v, w.Code, w.Body.String())
		}
	}
	for _, v := range []string{`1`, `100`} {
		if w := leadCDo(h, "POST", "/api/graph/resolve", leadCResolveBody(seeds, `"max_items":`+v), ""); w.Code != http.StatusOK {
			t.Errorf("max_items=%s: status %d body %q, want 200", v, w.Code, w.Body.String())
		}
	}
	// Absent: default.
	if w := leadCDo(h, "POST", "/api/graph/resolve", leadCResolveBody(seeds, ""), ""); w.Code != http.StatusOK {
		t.Errorf("max_items absent: status %d", w.Code)
	}
}

func TestLeadC_ResolveRepeatedRunsSameOrder(t *testing.T) {
	pool := leadCSetup(t)
	hana := leadCPerson(t, pool, "Lead C Hana", "ULCHANA", false)
	seed := leadCJira(t, pool, "LCD-SEED", "tie seed", "2026-03-01T00:00:00Z", 0)
	// Every candidate has the same age, hop and author, so every score ties and
	// the order must come from node ids alone.
	const when = "2026-03-01T00:00:00Z"
	var keys []string
	for i := 6; i >= 1; i-- {
		id := leadCJira(t, pool, fmt.Sprintf("LCD-T%d", i), "tie", when, 0)
		leadCEdge(t, pool, seed, id, "REFERENCES")
		keys = append(keys, id)
	}
	for i := 3; i >= 1; i-- {
		ch := fmt.Sprintf("LCD%d", i)
		root := leadCMsg(t, pool, ch, "100.000001", "100.000001", hana, when, "tie root")
		leadCMsg(t, pool, ch, "100.000002", "100.000001", hana, when, "tie reply")
		leadCEdge(t, pool, seed, root, "REFERENCES")
		keys = append(keys, root)
	}
	sort.Strings(keys)
	h, _ := leadCResolveH(t, pool)

	for _, tc := range []struct {
		name  string
		extra string
		want  []string
	}{
		{"all", ``, append([]string{seed}, keys...)},
		{"capped", `"max_items":5`, append([]string{seed}, keys[:4]...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for run := range 10 {
				ids := leadCIDs(leadCList(t, leadCDecode(t, leadCDo(h, "POST", "/api/graph/resolve", leadCResolveBody([]string{seed}, tc.extra), "")), "artifacts"), "node_id")
				if !reflect.DeepEqual(ids, tc.want) {
					t.Fatalf("run %d: order %v, want %v", run, ids, tc.want)
				}
			}
		})
	}
}

// ======================================================================================
// Item 8: aggregate
// ======================================================================================

type leadCThread struct{ last, bot string }

func leadCCheckThread(t *testing.T, who string, it map[string]any, want leadCThread) {
	t.Helper()
	if it["last_activity_at"] != want.last || it["bot_in_thread"] != want.bot {
		t.Errorf("%s: last_activity_at=%v bot_in_thread=%v, want %s / %s", who, it["last_activity_at"], it["bot_in_thread"], want.last, want.bot)
	}
}

func TestLeadC_AggregateIgnoresNonMessageAndHiddenRows(t *testing.T) {
	pool := leadCSetup(t)
	ann := leadCPerson(t, pool, "Lead C Ann", "ULCANN", false)
	bob := leadCPerson(t, pool, "Lead C Bob", "ULCBOB", false)
	bot := leadCPerson(t, pool, "Lead C Bot", "ULCBOT", true)
	root := leadCMsg(t, pool, "LCA1", "5000.000001", "5000.000001", ann, "2026-04-01T08:00:00Z", "agg root")
	reply := leadCMsg(t, pool, "LCA1", "5000.000002", "5000.000001", bob, "2026-04-01T09:00:00Z", "agg reply")

	// Rows that share the thread's scope and thread key but are not live Slack
	// messages, all bot-authored and newer than the real activity.
	meta := `{"thread_ts":"5000.000001"}`
	leadCPut(t, pool, leadCNodeSpec{id: "slack_file:FLCA1", typ: "slack_file", scope: "slack:LCA1", meta: meta, author: bot, updated: "2026-06-01T00:00:00Z"})
	leadCPut(t, pool, leadCNodeSpec{id: "jira_attachment:LCA-1:att1", typ: "jira_attachment", scope: "slack:LCA1", meta: meta, author: bot, updated: "2026-06-02T00:00:00Z"})
	leadCPut(t, pool, leadCNodeSpec{id: "slack_thread:LCA1:5000.000001", typ: "slack_thread", scope: "slack:LCA1", meta: meta, author: bot, updated: "2026-06-03T00:00:00Z"})
	leadCMsgX(t, pool, "LCA1", "5000.000003", "5000.000001", bot, "2026-07-01T00:00:00Z", "deleted bot reply", true)
	// A hidden channel with the same thread timestamp and a bot message.
	hiddenRoot := leadCMsg(t, pool, "LCAH", "5000.000001", "5000.000001", ann, "2026-08-01T00:00:00Z", "hidden root")
	leadCMsg(t, pool, "LCAH", "5000.000009", "5000.000001", bot, "2026-08-02T00:00:00Z", "hidden bot reply")
	leadCEdge(t, pool, root, "slack_file:FLCA1", "REFERENCES")
	leadCEdge(t, pool, root, "jira_attachment:LCA-1:att1", "REFERENCES")
	const asker = "leadc-agg@example.com"
	aclPerson(t, pool, 6103, asker, "slack:LCA1")

	nodeH := leadCNodeH(pool)
	resH, _ := leadCResolveH(t, pool)
	check := func(t *testing.T, want leadCThread) {
		t.Helper()
		for _, who := range []string{"", asker} {
			label := "admin"
			if who != "" {
				label = "filtered"
			}
			for _, id := range []string{root, reply} {
				w := leadCDo(nodeH, "GET", leadCNodeTarget(id), "", who)
				leadCCheckThread(t, label+" node "+id, leadCDecode(t, w), want)
				if strings.Contains(w.Body.String(), "LCAH") {
					t.Errorf("%s node %s mentions the hidden channel", label, id)
				}
			}
			w := leadCDo(resH, "POST", "/api/graph/resolve", leadCResolveBody([]string{root}, ""), who)
			rows := leadCByID(leadCList(t, leadCDecode(t, w), "artifacts"), "node_id")
			if rows[root] == nil {
				t.Fatalf("%s resolve: root artifact missing", label)
			}
			leadCCheckThread(t, label+" resolve", rows[root], want)
			if strings.Contains(w.Body.String(), "LCAH") || strings.Contains(w.Body.String(), hiddenRoot) {
				t.Errorf("%s resolve mentions the hidden channel", label)
			}
		}
	}
	t.Run("non-message and hidden rows are ignored", func(t *testing.T) {
		check(t, leadCThread{last: "2026-04-01T09:00:00Z", bot: "no"})
	})
	// Control: a live bot message in the thread does move both values, so the
	// test above is not passing because the aggregate ignores everything.
	leadCMsg(t, pool, "LCA1", "5000.000004", "5000.000001", bot, "2026-05-01T00:00:00Z", "live bot reply")
	t.Run("a live message does count", func(t *testing.T) {
		check(t, leadCThread{last: "2026-05-01T00:00:00Z", bot: "yes"})
	})
}

func TestLeadC_AggregateOncePerResponse(t *testing.T) {
	pool := leadCSetup(t)
	hana := leadCPerson(t, pool, "Lead C Hana", "ULCHANA", false)
	seed := leadCJira(t, pool, "LCO-SEED", "once seed", "2026-03-01T00:00:00Z", 0)
	var roots, replies []string
	for i := 1; i <= 4; i++ {
		ch := fmt.Sprintf("LCO%d", i)
		r := leadCMsg(t, pool, ch, "100.000001", "100.000001", hana, "2026-03-01T00:00:00Z", "leadconce root")
		p := leadCMsg(t, pool, ch, "100.000002", "100.000001", hana, "2026-03-01T01:00:00Z", "leadconce reply")
		leadCEdge(t, pool, seed, r, "REFERENCES")
		roots, replies = append(roots, r), append(replies, p)
	}
	leadCEdge(t, pool, seed, "slack:LCO1:100.000002", "REFERENCES")
	aclPerson(t, pool, 6104, "leadc-once@example.com", "slack:LCO1", "slack:LCO2", "slack:LCO3", "slack:LCO4")

	tr := &leadCTracer{}
	traced := leadCPool(t, tr, "")
	res, _ := leadCResolveH(t, traced)
	nodeH := leadCNodeH(traced)
	search := leadCSearchH(t, traced)
	count := func(t *testing.T, what string, w *httptest.ResponseRecorder, want int) {
		t.Helper()
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", what, w.Code, w.Body.String())
		}
		if got := len(tr.with("lead:aggregate")); got != want {
			t.Errorf("%s: %d aggregate queries, want %d", what, got, want)
		}
		tr.reset()
	}
	for _, who := range []string{"", "leadc-once@example.com"} {
		name := "admin"
		if who != "" {
			name = "filtered"
		}
		tr.reset()
		count(t, name+" resolve", leadCDo(res, "POST", "/api/graph/resolve", leadCResolveBody([]string{seed}, ""), who), 1)
		count(t, name+" resolve, slack-only seeds", leadCDo(res, "POST", "/api/graph/resolve", leadCResolveBody([]string{roots[0], roots[1], replies[2]}, `"depth":1`), who), 1)
		count(t, name+" node", leadCDo(nodeH, "GET", leadCNodeTarget(replies[0]), "", who), 1)
		count(t, name+" default search", leadCDo(search, "GET", "/api/graph/search?q=leadconce&limit=50", "", who), 1)
		count(t, name+" hybrid search", leadCDo(search, "GET", "/api/graph/search?q=leadconce&match=hybrid&limit=50", "", who), 1)
		// No Slack item in the response: nothing to aggregate.
		count(t, name+" jira-only resolve", leadCDo(res, "POST", "/api/graph/resolve", leadCResolveBody([]string{seed}, `"depth":1,"max_items":1`), who), 0)
		count(t, name+" node on jira", leadCDo(nodeH, "GET", leadCNodeTarget(seed), "", who), 0)
	}
}

// leadCExplain re-runs q with EXPLAIN (ANALYZE, BUFFERS) and the captured args.
func leadCExplain(t *testing.T, pool *pgxpool.Pool, q leadCQuery) string {
	t.Helper()
	rows, err := pool.Query(context.Background(), "EXPLAIN (ANALYZE, BUFFERS) "+q.sql, q.args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, l)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain: %v", err)
	}
	return strings.Join(lines, "\n")
}

func TestLeadC_AggregateScaleAndPlan(t *testing.T) {
	pool := leadCSetup(t)
	ctx := context.Background()
	human := leadCPerson(t, pool, "Lead C Scale Human", "ULCSH", false)
	bot := leadCPerson(t, pool, "Lead C Scale Bot", "ULCSB", true)
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `DELETE FROM graph.nodes WHERE scope LIKE 'slack:LCP%'`); err != nil {
			t.Errorf("delete scale rows: %v", err)
		}
		// Give later tests a table whose statistics match its contents again.
		if _, err := pool.Exec(ctx, `VACUUM (ANALYZE) graph.nodes`); err != nil {
			t.Errorf("vacuum: %v", err)
		}
	})
	// 50 channels x 3600 messages (threads of 10, one bot message every 97th,
	// every channel using the same timestamps and a 5000-value updated_at
	// pattern so equal values are common) plus 20000 replies in one hot thread
	// of channel LCP07: 200000 nodes. Some messages are soft-deleted.
	start := time.Now()
	if _, err := pool.Exec(ctx, `
INSERT INTO graph.nodes (id, type, natural_key, scope, metadata, author_person_id, updated_at, created_at, deleted_at, machine_id)
SELECT 'slack:' || ch || ':' || ts, 'slack', 'slack:' || ch || ':' || ts, 'slack:' || ch,
       jsonb_build_object('ts', ts, 'thread_ts', tts),
       CASE WHEN n % 97 = 5 THEN $1::bigint ELSE $2::bigint END,
       timestamptz '2026-01-01 00:00:00+00' + ((n * 7 + c) % 5000) * interval '1 minute',
       timestamptz '2026-01-01 00:00:00+00',
       CASE WHEN n % 101 = 7 AND n >= 10 THEN now() END,
       'test'
FROM (
  SELECT c, n, 'LCP' || lpad(c::text, 2, '0') AS ch,
         (1700000000 + n)::text || '.000001' AS ts,
         (1700000000 + (n / 10) * 10)::text || '.000001' AS tts
  FROM generate_series(0, 49) AS c, generate_series(0, 3599) AS n
) s`, bot, human); err != nil {
		t.Fatalf("insert regular rows: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO graph.nodes (id, type, natural_key, scope, metadata, author_person_id, updated_at, created_at, deleted_at, machine_id)
SELECT 'slack:LCP07:' || ts, 'slack', 'slack:LCP07:' || ts, 'slack:LCP07',
       jsonb_build_object('ts', ts, 'thread_ts', '1700000000.000001'),
       CASE WHEN m % 89 = 3 THEN $1::bigint ELSE $2::bigint END,
       timestamptz '2026-01-01 00:00:00+00' + (m % 3000) * interval '1 minute',
       timestamptz '2026-01-01 00:00:00+00',
       CASE WHEN m % 101 = 7 THEN now() END,
       'test'
FROM (SELECT m, (1800000000 + m)::text || '.000001' AS ts FROM generate_series(0, 19999) AS m) s`, bot, human); err != nil {
		t.Fatalf("insert hot thread: %v", err)
	}
	var total int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph.nodes WHERE scope LIKE 'slack:LCP%'`).Scan(&total); err != nil || total != 200000 {
		t.Fatalf("scale rows = %d (%v), want 200000", total, err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE graph.nodes`); err != nil {
		t.Fatal(err)
	}
	t.Logf("loaded %d nodes in %v", total, time.Since(start).Round(time.Millisecond))

	const asker = "leadc-scale@example.com"
	aclPerson(t, pool, 6105, asker, "slack:LCP03", "slack:LCP07", "slack:LCP30")

	tr := &leadCTracer{}
	traced := leadCPool(t, tr, "")
	nodeH := leadCNodeH(traced)
	resH, _ := leadCResolveH(t, traced)

	expect := func(t *testing.T, scope, thread string) leadCThread {
		t.Helper()
		var last time.Time
		var bots bool
		err := pool.QueryRow(ctx, `
SELECT max(n.updated_at), COALESCE(bool_or(COALESCE(p.is_bot, false)), false)
FROM graph.nodes n LEFT JOIN graph.people p ON p.id = n.author_person_id
WHERE n.scope = $1 AND n.deleted_at IS NULL AND n.type = 'slack'
  AND COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3)) = $2`, scope, thread).Scan(&last, &bots)
		if err != nil {
			t.Fatal(err)
		}
		out := leadCThread{last: last.UTC().Format(time.RFC3339), bot: "no"}
		if bots {
			out.bot = "yes"
		}
		return out
	}
	hotRoot, coldRoot := "slack:LCP07:1700000000.000001", "slack:LCP03:1700000010.000001"
	hotWant, coldWant := expect(t, "slack:LCP07", "1700000000.000001"), expect(t, "slack:LCP03", "1700000010.000001")
	if hotWant.bot != "yes" || coldWant.bot != "no" {
		t.Fatalf("dataset: hot thread bot=%s cold thread bot=%s, want yes/no", hotWant.bot, coldWant.bot)
	}

	var captured []leadCQuery
	run := func(name string, w *httptest.ResponseRecorder, want map[string]leadCThread) {
		t.Helper()
		top := leadCDecode(t, w)
		var items []map[string]any
		if arts, ok := top["artifacts"]; ok {
			items = leadCList(t, map[string]any{"artifacts": arts}, "artifacts")
		} else {
			items = []map[string]any{top}
		}
		by := leadCByID(items, "node_id")
		for id, wt := range want {
			if by[id] == nil {
				t.Fatalf("%s: %s missing from %v", name, id, leadCIDs(items, "node_id"))
			}
			leadCCheckThread(t, name+" "+id, by[id], wt)
		}
		qs := tr.with("lead:aggregate")
		if len(qs) != 1 {
			t.Fatalf("%s: %d aggregate queries, want 1", name, len(qs))
		}
		captured = append(captured, qs[0])
		tr.reset()
	}
	tr.reset()
	run("node hot (filtered)", leadCDo(nodeH, "GET", leadCNodeTarget("slack:LCP07:1800000042.000001"), "", asker), map[string]leadCThread{"slack:LCP07:1800000042.000001": hotWant})
	run("node cold (admin)", leadCDo(nodeH, "GET", leadCNodeTarget(coldRoot), "", ""), map[string]leadCThread{coldRoot: coldWant})
	seeds := []string{coldRoot, "slack:LCP30:1700000010.000001", "slack:LCP11:1700000010.000001"}
	run("resolve cold threads in three channels (admin)", leadCDo(resH, "POST", "/api/graph/resolve", leadCResolveBody(seeds, `"depth":1`), ""),
		map[string]leadCThread{
			coldRoot:                        coldWant,
			"slack:LCP30:1700000010.000001": expect(t, "slack:LCP30", "1700000010.000001"),
			"slack:LCP11:1700000010.000001": expect(t, "slack:LCP11", "1700000010.000001"),
		})
	run("resolve hot thread (filtered)", leadCDo(resH, "POST", "/api/graph/resolve", leadCResolveBody([]string{hotRoot, coldRoot}, `"depth":1`), asker),
		map[string]leadCThread{hotRoot: hotWant, coldRoot: coldWant})

	// The plan of the exact SQL each response ran, under default planner settings.
	for i, q := range captured {
		plan := leadCExplain(t, pool, q)
		t.Logf("plan %d:\n%s", i, plan)
		if !strings.Contains(plan, "graph_nodes_thread_key_idx") {
			t.Errorf("aggregate #%d does not use graph_nodes_thread_key_idx:\n%s", i, plan)
		}
		if strings.Contains(plan, "Seq Scan on nodes") {
			t.Errorf("aggregate #%d scans graph.nodes sequentially:\n%s", i, plan)
		}
	}
}

func TestLeadC_CancelledContext(t *testing.T) {
	pool := leadCSetup(t)
	hana := leadCPerson(t, pool, "Lead C Hana", "ULCHANA", false)
	root := leadCMsg(t, pool, "LCX1", "100.000001", "100.000001", hana, "2026-03-01T00:00:00Z", "leadccancel root")
	reply := leadCMsg(t, pool, "LCX1", "100.000002", "100.000001", hana, "2026-03-01T01:00:00Z", "leadccancel reply")
	leadCEdge(t, pool, root, reply, "REFERENCES")

	type route struct {
		name, method, target, body string
		h                          func(*pgxpool.Pool) http.Handler
		list                       string
	}
	routes := []route{
		{"resolve", "POST", "/api/graph/resolve", leadCResolveBody([]string{root}, ""), func(p *pgxpool.Pool) http.Handler { h, _ := leadCResolveH(t, p); return h }, "artifacts"},
		{"node", "GET", leadCNodeTarget(reply), "", func(p *pgxpool.Pool) http.Handler { return leadCNodeH(p) }, ""},
		{"default search", "GET", "/api/graph/search?q=leadccancel&limit=50", "", func(p *pgxpool.Pool) http.Handler { return leadCSearchH(t, p) }, "results"},
		{"hybrid search", "GET", "/api/graph/search?q=leadccancel&match=hybrid&limit=50", "", func(p *pgxpool.Pool) http.Handler { return leadCSearchH(t, p) }, "results"},
	}
	for _, rt := range routes {
		t.Run(rt.name+"/cancelled before the request", func(t *testing.T) {
			h := rt.h(pool)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			start := time.Now()
			w := leadCServe(h, leadCReq(rt.method, rt.target, rt.body, "").WithContext(ctx))
			if d := time.Since(start); d > time.Second {
				t.Fatalf("took %v with a cancelled context", d)
			}
			// Either an error status, or a 200 that carries no data.
			if w.Code == http.StatusOK && rt.list != "" {
				var top map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &top); err != nil {
					t.Fatal(err)
				}
				if items, _ := top[rt.list].([]any); len(items) != 0 {
					t.Fatalf("200 with data under a cancelled context: %s", w.Body.String())
				}
			} else if w.Code == http.StatusOK {
				t.Fatalf("200 under a cancelled context: %s", w.Body.String())
			}
		})
		t.Run(rt.name+"/cancelled at the aggregate query", func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tr := &leadCTracer{hook: func(sql string) {
				if strings.Contains(sql, "lead:aggregate") {
					cancel()
				}
			}}
			h := rt.h(leadCPool(t, tr, ""))
			start := time.Now()
			w := leadCServe(h, leadCReq(rt.method, rt.target, rt.body, "").WithContext(ctx))
			if d := time.Since(start); d > time.Second {
				t.Fatalf("took %v after the aggregate was cancelled", d)
			}
			if len(tr.with("lead:aggregate")) != 1 {
				t.Fatalf("aggregate ran %d times, want 1 (the hook is what cancels)", len(tr.with("lead:aggregate")))
			}
			if w.Code != http.StatusServiceUnavailable || strings.TrimSpace(w.Body.String()) != "lead_lookup_failed" {
				t.Fatalf("status %d body %q, want 503 lead_lookup_failed", w.Code, w.Body.String())
			}
		})
	}
}

// ======================================================================================
// Item 9: bot fields
// ======================================================================================

func TestLeadC_BotFields(t *testing.T) {
	pool := leadCSetup(t)
	hum := leadCPerson(t, pool, "Lead C Human", "UBHUM", false)
	ans := leadCPerson(t, pool, "Lead C Answer Bot", "UBANS", false)
	gen := leadCPerson(t, pool, "Lead C Generic Bot", "UBGEN", true)
	enzo := leadCPerson(t, pool, "Lead C EnzoBot", leadCEnzoBot, false)
	noSlack := leadCPerson(t, pool, "Lead C No Slack Id", "", false)
	leadCAnswerBot(t, pool, "UBANS")
	const when = "2026-03-01T00:00:00Z"
	const ch = "LCB1"
	root := func(ts string, author int64) string { return leadCMsg(t, pool, ch, ts, ts, author, when, "root "+ts) }
	reply := func(ts, thread string, author int64, deleted bool) string {
		return leadCMsgX(t, pool, ch, ts, thread, author, "2026-03-02T00:00:00Z", "reply "+ts, deleted)
	}

	type want struct{ slack, bot, inThread string }
	cases := []struct {
		name string
		id   string
		want want
	}{}
	add := func(name, id string, w want) {
		cases = append(cases, struct {
			name string
			id   string
			want want
		}{name, id, w})
	}

	add("answer-list bot, root only", root("1.000001", ans), want{"UBANS", "bot", "yes"})
	add("generic is_bot, root only", root("2.000001", gen), want{"UBGEN", "bot", "yes"})
	add("seeded EnzoBot (answer list from the migration)", root("10.000001", enzo), want{leadCEnzoBot, "bot", "yes"})
	t3 := root("3.000001", hum)
	reply("3.000002", "3.000001", hum, false)
	add("human thread", t3, want{"UBHUM", "human", "no"})
	add("missing person, root only", root("4.000001", 0), want{"", "unknown", "unknown"})
	t5 := root("5.000001", hum)
	t5r := reply("5.000002", "5.000001", enzo, false)
	add("human root with bot reply (root)", t5, want{"UBHUM", "human", "yes"})
	add("human root with bot reply (the reply keeps its id, root's values)", t5r, want{"UBHUM", "human", "yes"})
	t6 := root("6.000001", hum)
	reply("6.000002", "6.000001", gen, true)
	add("human root, deleted bot reply", t6, want{"UBHUM", "human", "no"})
	t7 := root("7.000001", hum)
	reply("7.000002", "7.000001", 0, false)
	add("human root, reply without a person", t7, want{"UBHUM", "human", "unknown"})
	add("person without slack id", root("8.000001", noSlack), want{"", "human", "no"})
	t9 := root("9.000001", hum)
	reply("9.000002", "9.000001", ans, false)
	reply("9.000003", "9.000001", 0, false)
	add("bot reply beside a reply without a person", t9, want{"UBHUM", "human", "yes"})

	nodeH := leadCNodeH(pool)
	resH, _ := leadCResolveH(t, pool)
	for _, tc := range cases {
		t.Run("node/"+tc.name, func(t *testing.T) {
			it := leadCDecode(t, leadCDo(nodeH, "GET", leadCNodeTarget(tc.id), "", ""))
			leadCCheckFields(t, it, true)
			if it["author_slack_id"] != tc.want.slack || it["authored_by_bot"] != tc.want.bot || it["bot_in_thread"] != tc.want.inThread || it["channel_id"] != ch {
				t.Errorf("fields = %v/%v/%v/%v, want %v/%v/%v/%v", it["author_slack_id"], it["authored_by_bot"], it["bot_in_thread"], it["channel_id"],
					tc.want.slack, tc.want.bot, tc.want.inThread, ch)
			}
		})
	}
	// Non-Slack nodes: own author only, no bot_in_thread key at all.
	jBot := leadCJira(t, pool, "LCB-BOT", "bot jira", when, gen)
	jAns := leadCJira(t, pool, "LCB-ANS", "answer bot jira", when, ans)
	jNone := leadCJira(t, pool, "LCB-NONE", "orphan jira", when, 0)
	jHum := leadCJira(t, pool, "LCB-HUM", "human jira", when, hum)
	for id, w := range map[string]want{
		jBot: {"UBGEN", "bot", ""}, jAns: {"UBANS", "bot", ""}, jNone: {"", "unknown", ""}, jHum: {"UBHUM", "human", ""},
	} {
		t.Run("node/"+id, func(t *testing.T) {
			it := leadCDecode(t, leadCDo(nodeH, "GET", leadCNodeTarget(id), "", ""))
			leadCCheckFields(t, it, false)
			if it["author_slack_id"] != w.slack || it["authored_by_bot"] != w.bot || it["channel_id"] != "" {
				t.Errorf("fields = %v/%v/%v, want %v/%v/\"\"", it["author_slack_id"], it["authored_by_bot"], it["channel_id"], w.slack, w.bot)
			}
		})
	}

	// The same values through resolve (roots of the cases above).
	t.Run("resolve", func(t *testing.T) {
		seeds := []string{t3, t5, t6, t7, t9, jBot, jNone}
		w := leadCDo(resH, "POST", "/api/graph/resolve", leadCResolveBody(seeds, ""), "")
		by := leadCByID(leadCList(t, leadCDecode(t, w), "artifacts"), "node_id")
		for _, c := range []struct {
			id string
			w  want
		}{{t3, want{"UBHUM", "human", "no"}}, {t5, want{"UBHUM", "human", "yes"}}, {t6, want{"UBHUM", "human", "no"}}, {t7, want{"UBHUM", "human", "unknown"}},
			{t9, want{"UBHUM", "human", "yes"}}, {jBot, want{"UBGEN", "bot", ""}}, {jNone, want{"", "unknown", ""}}} {
			it := by[c.id]
			if it == nil {
				t.Errorf("%s missing", c.id)
				continue
			}
			leadCCheckFields(t, it, strings.HasPrefix(c.id, "slack:"))
			got := want{it["author_slack_id"].(string), it["authored_by_bot"].(string), ""}
			if s, ok := it["bot_in_thread"].(string); ok {
				got.inThread = s
			}
			if got != c.w {
				t.Errorf("%s: %+v, want %+v", c.id, got, c.w)
			}
		}
	})

	// A reader that cannot see the people's bot list: roles denied graph.answer_bots.
	// Humans must not silently become "human" when the bot list failed to load.
	t.Run("loader error", func(t *testing.T) {
		aclPerson(t, pool, 6106, "leadc-bots@example.com", "slack:"+ch)
		role := leadCRole(t, pool, "graph.answer_bots")
		bad := leadCPool(t, nil, role)
		for _, who := range []string{"", "leadc-bots@example.com"} {
			badRes, spy := leadCResolveH(t, bad)
			for name, w := range map[string]*httptest.ResponseRecorder{
				"node":           leadCDo(leadCNodeH(bad), "GET", leadCNodeTarget(t5), "", who),
				"node on jira":   leadCDo(leadCNodeH(bad), "GET", leadCNodeTarget(jHum), "", who),
				"resolve":        leadCDo(badRes, "POST", "/api/graph/resolve", leadCResolveBody([]string{t5}, ""), who),
				"default search": leadCDo(leadCSearchH(t, bad), "GET", "/api/graph/search?q=reply&limit=50", "", who),
				"hybrid search":  leadCDo(leadCSearchH(t, bad), "GET", "/api/graph/search?q=reply&match=hybrid&limit=50", "", who),
			} {
				if w.Code != http.StatusServiceUnavailable || strings.TrimSpace(w.Body.String()) != "lead_lookup_failed" {
					t.Errorf("%s (asker %q): status %d body %q, want 503 lead_lookup_failed", name, who, w.Code, w.Body.String())
				}
			}
			if got := spy.drain(); len(got) != 0 {
				t.Errorf("enqueued %v after a lead lookup failure", got)
			}
		}
		// Control: the same requests without the header are unaffected by the role.
		if w := leadCDo(leadCNodeH(bad), "GET", leadCNodeTarget(t5), "", "", leadHeader, ""); w.Code != http.StatusOK {
			t.Errorf("non-lead node on the restricted pool: %d %s", w.Code, w.Body.String())
		}
	})
}
