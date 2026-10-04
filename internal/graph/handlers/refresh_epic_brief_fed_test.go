package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

func TestPendingMembersAndSelect(t *testing.T) {
	ms := []briefMember{{NodeID: "a", Version: "1"}, {NodeID: "b", Version: "1"}, {NodeID: "c", Version: "2"}}
	fed := map[string]string{"a": epicBriefFedValue(ms[0]), "b": epicBriefSigVersion + "|b|0", "gone": "x"}
	got := pendingMembers(ms, fed)
	if len(got) != 2 || got[0].NodeID != "b" || got[1].NodeID != "c" {
		t.Fatalf("pending = %+v, want b (changed) and c (absent)", got)
	}
	if n := len(pendingMembers(ms, nil)); n != 3 {
		t.Fatalf("nil fed: pending %d, want 3", n)
	}
	var many []briefMember
	for i := range epicBriefMaxMembers + 5 {
		many = append(many, briefMember{NodeID: fmt.Sprintf("n%03d", i)})
	}
	if in := selectBriefInputs(many, nil, true); len(in) != epicBriefMaxMembers || in[0].NodeID != "n000" {
		t.Fatalf("full: %d inputs", len(in))
	}
	if in := selectBriefInputs(many, many[3:], false); len(in) != epicBriefMaxMembers || in[0].NodeID != "n003" {
		t.Fatalf("delta: %d inputs starting %q", len(in), in[0].NodeID)
	}
	if in := selectBriefInputs(many, nil, false); len(in) != 0 {
		t.Fatalf("delta with nothing pending must be empty, got %d", len(in))
	}
}

// briefHarness drives enqueue + handler cycles over the real hierarchy
// rebuild, so graph.epic_membership carries the business-root rows exactly
// as in production.
type briefHarness struct {
	t       *testing.T
	pool    *pgxpool.Pool
	ctx     context.Context
	deps    Deps
	prompts []string
	// calls counts LLM calls
	calls int
	epics map[string]int
}

var briefIDRe = regexp.MustCompile(`\[(jira:PAY-\d+)\]`)

func newBriefHarness(t *testing.T) *briefHarness {
	t.Helper()
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	ctx := context.Background()
	clean := func() {
		truncateGraphHandlerTables(t, pool)
		for _, q := range []string{
			`DELETE FROM graph.epic_briefs`, `DELETE FROM graph.epic_membership`,
			`DELETE FROM graph.thread_summaries`, `DELETE FROM settings WHERE key LIKE 'graph.epic_briefs.%'`,
		} {
			_, _ = pool.Exec(ctx, q)
		}
	}
	clean()
	t.Cleanup(clean)
	h := &briefHarness{t: t, pool: pool, ctx: ctx, epics: map[string]int{"PAY-1000": 0}}
	h.exec(`INSERT INTO settings(key,value) VALUES('graph.epic_briefs.enabled','true')`)
	gem := &mockGemini{}
	gem.generateResult = func() (string, error) {
		h.calls++
		h.prompts = append(h.prompts, gem.generateUser)
		return fmt.Sprintf(`{"brief":"brief %d","highlights":[],"open_items":[]}`, h.calls), nil
	}
	h.deps = Deps{DB: pool, Gemini: gem, Logger: zerolog.Nop(), MachineID: "test"}
	h.epic("PAY-1000")
	return h
}

func (h *briefHarness) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.pool.Exec(h.ctx, sql, args...); err != nil {
		h.t.Fatalf("%v\n%s", err, sql)
	}
}

// epic seeds an epic's Jira node (no jira_epic_map self row: an epic that
// loses its issues must drop off the board map).
func (h *briefHarness) epic(key string) {
	h.exec(`INSERT INTO graph.nodes (id, type, natural_key, title, scope, metadata, machine_id)
	        VALUES ($1,'jira',$2,$2,'slack:C1','{}'::jsonb,'test') ON CONFLICT DO NOTHING`, "jira:"+key, key)
}

// member seeds issue PAY-(2000+i), summarized, mapped to the epic. Lower i =
// newer.
func (h *briefHarness) member(i int, epic string) string {
	key := fmt.Sprintf("PAY-%d", 2000+i)
	id := "jira:" + key
	h.exec(`INSERT INTO graph.nodes (id, type, natural_key, title, scope, metadata, machine_id, created_at)
	        VALUES ($1,'jira',$2,$2,'slack:C1','{}'::jsonb,'test', NOW() - ($3 || ' hours')::interval)`, id, key, fmt.Sprint(i+1))
	h.exec(`INSERT INTO graph.artifact_index (node_id, summary, machine_id, refreshed_at)
	        VALUES ($1,$2,'test', NOW() - interval '30 days')`, id, "summary of "+key)
	h.mapTo(i, epic)
	return id
}

func (h *briefHarness) mapTo(i int, epic string) {
	key := fmt.Sprintf("PAY-%d", 2000+i)
	h.exec(`INSERT INTO graph.jira_epic_map (issue_key, epic_key, epic_summary, machine_id) VALUES ($1,$2,'Epic','test')
	        ON CONFLICT (issue_key) DO UPDATE SET epic_key = EXCLUDED.epic_key`, key, epic)
}

func (h *briefHarness) unmap(i int) {
	h.exec(`DELETE FROM graph.jira_epic_map WHERE issue_key = $1`, fmt.Sprintf("PAY-%d", 2000+i))
}

func (h *briefHarness) resummarize(i int) {
	h.exec(`UPDATE graph.artifact_index SET refreshed_at = refreshed_at + interval '1 day' WHERE node_id = $1`,
		fmt.Sprintf("jira:PAY-%d", 2000+i))
}

func (h *briefHarness) rebuild() {
	h.t.Helper()
	if err := rebuildEpicHierarchy(h.ctx, h.pool, "test", "PAY", h.epics); err != nil {
		h.t.Fatalf("rebuild: %v", err)
	}
	// The business root must hold rows for the epic's members: every query
	// over epic_membership has to tolerate them.
	var n int
	_ = h.pool.QueryRow(h.ctx, `SELECT count(*) FROM graph.epic_membership WHERE epic_key = $1`, businessRootID).Scan(&n)
	if n == 0 {
		h.t.Fatalf("rebuild left no business-root membership rows")
	}
}

// cycle = enqueue, run every queued job to completion, age the rows past the
// interval floor. Returns jobs enqueued and the prompts sent this cycle.
func (h *briefHarness) cycle() (int, []string) {
	h.t.Helper()
	before := len(h.prompts)
	n := enqueueEpicBriefs(h.ctx, h.deps, "PAY")
	hd := NewRefreshEpicBriefHandler(h.deps).Handler
	for {
		j, err := jobs.Claim(h.ctx, h.pool, "refresh_epic_brief", SummaryLease, "w", h.deps.Runner)
		if err != nil {
			h.t.Fatalf("claim: %v", err)
		}
		if j == nil {
			break
		}
		if err := hd(h.ctx, j.Payload); err != nil {
			h.t.Fatalf("handler: %v", err)
		}
		if err := jobs.Complete(h.ctx, h.pool, j.ID); err != nil {
			h.t.Fatalf("complete: %v", err)
		}
	}
	var stray int
	_ = h.pool.QueryRow(h.ctx, `SELECT count(*) FROM graph.jobs WHERE type='refresh_epic_brief' AND payload->>'epic_key' NOT LIKE 'PAY-%'`).Scan(&stray)
	var strayRows int
	_ = h.pool.QueryRow(h.ctx, `SELECT count(*) FROM graph.epic_briefs WHERE epic_key NOT LIKE 'PAY-%'`).Scan(&strayRows)
	if stray != 0 || strayRows != 0 {
		h.t.Fatalf("business root leaked into the brief job: jobs=%d rows=%d", stray, strayRows)
	}
	h.exec(`UPDATE graph.epic_briefs SET updated_at = NOW() - interval '2 hours'`)
	return n, h.prompts[before:]
}

func (h *briefHarness) fed(epic string) map[string]string {
	h.t.Helper()
	br, ok := loadEpicBrief(h.ctx, h.pool, epic)
	if !ok {
		h.t.Fatalf("no brief row for %s", epic)
	}
	return br.Fed
}

func (h *briefHarness) fedText(epic string) string {
	var s *string
	_ = h.pool.QueryRow(h.ctx, `SELECT fed::text FROM graph.epic_briefs WHERE epic_key=$1`, epic).Scan(&s)
	if s == nil {
		return "NULL"
	}
	return *s
}

func promptIDs(p string) []string {
	var ids []string
	for _, m := range briefIDRe.FindAllStringSubmatch(p, -1) {
		ids = append(ids, m[1])
	}
	sort.Strings(ids)
	return ids
}

func idRange(from, to int) []string {
	var ids []string
	for i := from; i < to; i++ {
		ids = append(ids, fmt.Sprintf("jira:PAY-%d", 2000+i))
	}
	sort.Strings(ids)
	return ids
}

func eqStrings(a, b []string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

func (h *briefHarness) seed(n int) {
	for i := range n {
		h.member(i, "PAY-1000")
	}
	h.rebuild()
}

func TestEpicBrief_DeltaFeedsOnlyPending(t *testing.T) {
	h := newBriefHarness(t)
	h.seed(60)

	if n, ps := h.cycle(); n != 1 || len(ps) != 1 || len(h.fed("PAY-1000")) != 40 {
		t.Fatalf("first cycle: enq=%d prompts=%d fed=%d", n, len(ps), len(h.fed("PAY-1000")))
	}
	if ids := promptIDs(h.prompts[0]); !eqStrings(ids, idRange(0, 40)) {
		t.Fatalf("first build must feed the newest 40, got %v", ids)
	}
	h.resummarize(5)
	n, ps := h.cycle()
	if n != 1 || len(ps) != 1 {
		t.Fatalf("second cycle: enq=%d prompts=%d", n, len(ps))
	}
	want := append(idRange(40, 60), "jira:PAY-2005")
	sort.Strings(want)
	if ids := promptIDs(ps[0]); !eqStrings(ids, want) {
		t.Fatalf("delta must feed the re-summarized member plus the 20 never fed, got %v", ids)
	}
	if !strings.Contains(ps[0], "Previous brief") ||
		!strings.Contains(ps[0], "Members not yet reflected at their current version:") ||
		strings.Contains(ps[0], "changed or added") {
		t.Fatalf("delta prompt wording wrong:\n%s", ps[0])
	}
	if len(h.fed("PAY-1000")) != 60 {
		t.Fatalf("fed = %d, want 60", len(h.fed("PAY-1000")))
	}
	if n, ps := h.cycle(); n != 0 || len(ps) != 0 {
		t.Fatalf("no change: enq=%d prompts=%d", n, len(ps))
	}
}

func TestEpicBrief_LargeChangeDrains(t *testing.T) {
	h := newBriefHarness(t)
	h.seed(100)
	for range 3 { // 40, 40, 20
		h.cycle()
	}
	if len(h.fed("PAY-1000")) != 100 {
		t.Fatalf("setup: fed = %d, want 100", len(h.fed("PAY-1000")))
	}
	for i := range 100 {
		h.resummarize(i)
	}
	for _, want := range []int{40, 40, 20} {
		n, ps := h.cycle()
		if n != 1 || len(ps) != 1 || len(promptIDs(ps[0])) != want {
			t.Fatalf("drain: enq=%d prompts=%d, want one prompt of %d members", n, len(ps), want)
		}
	}
	if n, ps := h.cycle(); n != 0 || len(ps) != 0 {
		t.Fatalf("after draining: enq=%d prompts=%d, want none", n, len(ps))
	}
}

func TestEpicBrief_LegacyNullFedFullBuild(t *testing.T) {
	h := newBriefHarness(t)
	h.seed(3)
	h.cycle()
	h.exec(`UPDATE graph.epic_briefs SET fed = NULL, built_version = NULL, sources = ARRAY['jira:OLD-1','jira:PAY-2000']`)
	n, ps := h.cycle()
	if n != 1 || len(ps) != 1 {
		t.Fatalf("legacy row: enq=%d prompts=%d", n, len(ps))
	}
	if strings.Contains(ps[0], "Previous brief") {
		t.Fatalf("legacy NULL fed must be a full build:\n%s", ps[0])
	}
	br, _ := loadEpicBrief(h.ctx, h.pool, "PAY-1000")
	if !eqStrings(br.Sources, idRange(0, 3)) || len(br.Fed) != 3 || br.BuiltVersion != epicBriefSigVersion {
		t.Fatalf("after legacy rebuild: sources=%v fed=%d built=%q", br.Sources, len(br.Fed), br.BuiltVersion)
	}
}

func setSigVersion(t *testing.T, v string) {
	t.Helper()
	old := epicBriefSigVersion
	epicBriefSigVersion = v
	t.Cleanup(func() { epicBriefSigVersion = old })
}

func TestEpicBrief_VersionBumpFullBuild(t *testing.T) {
	h := newBriefHarness(t)
	h.seed(100)
	for range 3 {
		h.cycle()
	}
	if len(h.fed("PAY-1000")) != 100 {
		t.Fatalf("setup: fed = %d", len(h.fed("PAY-1000")))
	}
	setSigVersion(t, "v-test2")

	n, ps := h.cycle()
	if n != 1 || len(ps) != 1 {
		t.Fatalf("bump cycle: enq=%d prompts=%d", n, len(ps))
	}
	if strings.Contains(ps[0], "Previous brief") || !eqStrings(promptIDs(ps[0]), idRange(0, 40)) {
		t.Fatalf("version bump must be a full build of the newest 40:\n%s", ps[0])
	}
	br, _ := loadEpicBrief(h.ctx, h.pool, "PAY-1000")
	if len(br.Sources) != 40 || len(br.Fed) != 40 || br.BuiltVersion != "v-test2" {
		t.Fatalf("after full build: sources=%d fed=%d built=%q", len(br.Sources), len(br.Fed), br.BuiltVersion)
	}
	for id, v := range br.Fed {
		if !strings.HasPrefix(v, "v-test2|"+id+"|") {
			t.Fatalf("fed[%s] = %q lacks the new version prefix", id, v)
		}
	}
	for _, want := range []int{40, 20} {
		n, ps := h.cycle()
		if n != 1 || len(ps) != 1 || len(promptIDs(ps[0])) != want || !strings.Contains(ps[0], "Previous brief") {
			t.Fatalf("delta after bump: enq=%d prompts=%d, want a delta of %d", n, len(ps), want)
		}
	}
	if n, ps := h.cycle(); n != 0 || len(ps) != 0 {
		t.Fatalf("after drain: enq=%d prompts=%d", n, len(ps))
	}
}

func TestEpicBrief_DryRunWritesNothing(t *testing.T) {
	h := newBriefHarness(t)
	h.seed(5)
	h.cycle()
	h.resummarize(1)
	snap := func() string {
		var s string
		if err := h.pool.QueryRow(h.ctx, `SELECT coalesce(fed::text,'NULL')||'|'||sources::text||'|'||brief||'|'||member_signature||'|'||coalesce(built_version,'NULL')||'|'||updated_at::text
		   FROM graph.epic_briefs WHERE epic_key='PAY-1000'`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := snap()
	calls := h.calls
	payload, _ := json.Marshal(refreshEpicBriefPayload{EpicKey: "PAY-1000", DryRun: true})
	if err := NewRefreshEpicBriefHandler(h.deps).Handler(h.ctx, payload); err != nil {
		t.Fatal(err)
	}
	if h.calls != calls+1 {
		t.Fatalf("dry run should make one LLM call, made %d", h.calls-calls)
	}
	if after := snap(); after != before {
		t.Fatalf("dry run wrote:\nbefore %s\nafter  %s", before, after)
	}
}

func TestEpicBrief_RemoveRejoin(t *testing.T) {
	t.Run("moved_out", func(t *testing.T) {
		h := newBriefHarness(t)
		h.epic("PAY-1500")
		h.epics["PAY-1500"] = 1
		h.member(0, "PAY-1000")
		h.rebuild()
		h.cycle()
		if len(h.fed("PAY-1000")) != 1 {
			t.Fatalf("setup: fed %v", h.fed("PAY-1000"))
		}
		calls := func(epic string) int {
			n := 0
			for _, p := range h.prompts {
				if strings.HasPrefix(p, "Epic "+epic+":") {
					n++
				}
			}
			return n
		}
		old := calls("PAY-1000")

		h.mapTo(0, "PAY-1500")
		h.rebuild()
		var onBoard int
		_ = h.pool.QueryRow(h.ctx, `SELECT count(*) FROM graph.jira_epic_map WHERE epic_key='PAY-1000'`).Scan(&onBoard)
		if onBoard != 0 {
			t.Fatalf("setup: old epic still in the board map")
		}
		n, _ := h.cycle()
		if n != 2 { // PAY-1500 first build + PAY-1000 cleanup
			t.Fatalf("cycle after move: enq=%d, want 2", n)
		}
		if calls("PAY-1000") != old || h.fedText("PAY-1000") != "{}" {
			t.Fatalf("old epic: llm calls %d->%d fed=%s", old, calls("PAY-1000"), h.fedText("PAY-1000"))
		}

		h.mapTo(0, "PAY-1000")
		h.rebuild()
		h.cycle()
		if calls("PAY-1000") != old+1 || len(h.fed("PAY-1000")) != 1 {
			t.Fatalf("rejoin: llm calls %d, fed=%s", calls("PAY-1000"), h.fedText("PAY-1000"))
		}
	})

	t.Run("version_bump_after_empty", func(t *testing.T) {
		h := newBriefHarness(t)
		h.seed(3)
		h.cycle()
		for i := range 3 {
			h.unmap(i)
		}
		h.rebuild()
		if n, ps := h.cycle(); n != 1 || len(ps) != 0 || h.fedText("PAY-1000") != "{}" {
			t.Fatalf("empty cycle: enq=%d prompts=%d fed=%s", n, len(ps), h.fedText("PAY-1000"))
		}
		if n, _ := h.cycle(); n != 0 {
			t.Fatalf("a cleaned-up empty epic must not re-enqueue, got %d", n)
		}
		setSigVersion(t, "v-test2")
		if n, ps := h.cycle(); n != 1 || len(ps) != 0 {
			t.Fatalf("bump over empty epic: enq=%d prompts=%d", n, len(ps))
		}
		for i := range 3 {
			h.mapTo(i, "PAY-1000")
		}
		h.rebuild()
		n, ps := h.cycle()
		if n != 1 || len(ps) != 1 || strings.Contains(ps[0], "Previous brief") {
			t.Fatalf("rejoin after bump must be a full build: enq=%d prompts=%v", n, ps)
		}
		br, _ := loadEpicBrief(h.ctx, h.pool, "PAY-1000")
		if br.BuiltVersion != "v-test2" || len(br.Fed) != 3 {
			t.Fatalf("built=%q fed=%d", br.BuiltVersion, len(br.Fed))
		}
	})

	t.Run("one_of_many", func(t *testing.T) {
		h := newBriefHarness(t)
		h.seed(3)
		h.cycle()
		before, _ := loadEpicBrief(h.ctx, h.pool, "PAY-1000")
		h.unmap(1)
		h.rebuild()
		n, ps := h.cycle()
		if n != 1 || len(ps) != 0 {
			t.Fatalf("remove one: enq=%d prompts=%d", n, len(ps))
		}
		after, _ := loadEpicBrief(h.ctx, h.pool, "PAY-1000")
		if _, ok := after.Fed["jira:PAY-2001"]; ok || len(after.Fed) != 2 {
			t.Fatalf("fed still lists the removed member: %v", after.Fed)
		}
		if !eqStrings(after.Sources, before.Sources) || after.Brief != before.Brief {
			t.Fatalf("sources/brief must not change: %v vs %v", after.Sources, before.Sources)
		}
		h.mapTo(1, "PAY-1000")
		h.rebuild()
		n, ps = h.cycle()
		if n != 1 || len(ps) != 1 || !eqStrings(promptIDs(ps[0]), []string{"jira:PAY-2001"}) {
			t.Fatalf("rejoin: enq=%d prompts=%v", n, ps)
		}
	})

	t.Run("last_member", func(t *testing.T) {
		h := newBriefHarness(t)
		h.seed(1)
		h.cycle()
		before, _ := loadEpicBrief(h.ctx, h.pool, "PAY-1000")
		h.unmap(0)
		h.rebuild()
		n, ps := h.cycle()
		if n != 1 || len(ps) != 0 || h.fedText("PAY-1000") != "{}" {
			t.Fatalf("remove last: enq=%d prompts=%d fed=%s", n, len(ps), h.fedText("PAY-1000"))
		}
		after, _ := loadEpicBrief(h.ctx, h.pool, "PAY-1000")
		if after.Brief != before.Brief || !eqStrings(after.Sources, before.Sources) {
			t.Fatalf("brief/sources changed: %+v", after)
		}
		h.mapTo(0, "PAY-1000")
		h.rebuild()
		n, ps = h.cycle()
		if n != 1 || len(ps) != 1 || !eqStrings(promptIDs(ps[0]), idRange(0, 1)) {
			t.Fatalf("rejoin: enq=%d prompts=%v", n, ps)
		}
	})
}
