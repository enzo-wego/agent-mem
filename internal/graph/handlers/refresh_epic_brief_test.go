package handlers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestEpicBriefSignature(t *testing.T) {
	a := []briefMember{{NodeID: "slack:C1:1.0", Version: "v9:3:100"}, {NodeID: "jira:PAY-101", Version: "1700000000"}}
	b := []briefMember{a[1], a[0]} // same set, different order
	if epicBriefSignature(a) != epicBriefSignature(b) {
		t.Fatal("signature must be order-independent")
	}
	if !strings.HasPrefix(epicBriefSignature(a), epicBriefSigVersion+":") {
		t.Fatalf("signature %q lacks version prefix", epicBriefSignature(a))
	}
	resum := []briefMember{{NodeID: "slack:C1:1.0", Version: "v9:4:200"}, a[1]}
	if epicBriefSignature(a) == epicBriefSignature(resum) {
		t.Fatal("re-summarized thread must change the signature")
	}
	joined := append([]briefMember{{NodeID: "gh_pr:wego/payments#1", Version: ""}}, a...)
	if epicBriefSignature(a) == epicBriefSignature(joined) {
		t.Fatal("new member must change the signature")
	}
	if epicBriefSignature(nil) == epicBriefSignature(a) {
		t.Fatal("empty set must differ")
	}
}

func TestSelectBriefInputs(t *testing.T) {
	at := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	var members []briefMember
	for i := range epicBriefMaxMembers + 5 {
		members = append(members, briefMember{NodeID: "n" + string(rune('A'+i)), SummaryAt: at.Add(-time.Duration(i) * time.Hour)})
	}
	// First build: capped at epicBriefMaxMembers, order preserved (newest first).
	first := selectBriefInputs(members, nil, time.Time{}, false)
	if len(first) != epicBriefMaxMembers || first[0].NodeID != "nA" {
		t.Fatalf("first build: got %d inputs starting %q", len(first), first[0].NodeID)
	}

	// Delta: previous build knew nA..nD at `at`; nB was re-summarized after,
	// nZ is new; nA, nC, nD unchanged and known → excluded.
	ms := []briefMember{
		{NodeID: "nA", SummaryAt: at.Add(-time.Hour)},
		{NodeID: "nB", SummaryAt: at.Add(time.Minute)},
		{NodeID: "nC", SummaryAt: at.Add(-2 * time.Hour)},
		{NodeID: "nD", SummaryAt: at},
		{NodeID: "nZ", SummaryAt: at.Add(-3 * time.Hour)},
	}
	got := selectBriefInputs(ms, []string{"nA", "nB", "nC", "nD"}, at, true)
	var ids []string
	for _, m := range got {
		ids = append(ids, m.NodeID)
	}
	if strings.Join(ids, ",") != "nB,nZ" {
		t.Fatalf("delta inputs = %v, want [nB nZ]", ids)
	}
	if n := selectBriefInputs(ms[:1], []string{"nA"}, at, true); len(n) != 0 {
		t.Fatalf("unchanged known member must yield no delta, got %v", n)
	}
}

func TestFirstSentence(t *testing.T) {
	cases := map[string]string{
		"India GST invoicing is live. Rollout continues.": "India GST invoicing is live.",
		"No boundary here":               "No boundary here",
		"Line one\nLine two.":            "Line one",
		"v2.1 shipped! Next is refunds.": "v2.1 shipped!",
	}
	for in, want := range cases {
		if got := firstSentence(in, 200); got != want {
			t.Errorf("firstSentence(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRefreshEpicBrief_Lifecycle drives the job over the round-1 fixture with
// a fake gateway: first build feeds every member; an unchanged signature makes
// no call; a re-summarized thread outside the interval floor runs a delta with
// only the changed member and the previous brief; dry_run never writes.
func TestRefreshEpicBrief_Lifecycle(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	exec(`DELETE FROM graph.epic_briefs`)
	exec(`DELETE FROM graph.thread_summaries`)
	exec(`DELETE FROM settings WHERE key LIKE 'graph.epic_briefs.%'`)
	t.Cleanup(func() {
		truncateGraphHandlerTables(t, pool)
		_, _ = pool.Exec(ctx, `DELETE FROM graph.epic_briefs`)
		_, _ = pool.Exec(ctx, `DELETE FROM graph.thread_summaries`)
		_, _ = pool.Exec(ctx, `DELETE FROM settings WHERE key LIKE 'graph.epic_briefs.%'`)
	})
	seedEpicFixture(t, pool)
	exec(`UPDATE graph.nodes SET title='GST invoicing for India', body='Issue tax invoices for Indian merchants.' WHERE id='jira:PAY-100'`)
	exec(`INSERT INTO graph.thread_summaries (channel_id, thread_ts, signature, summary, overview, updated_at)
	      VALUES ('C1','1.0','v9:2:100','GST invoice format','Thread A discussed the GST invoice format.', NOW() - interval '3 hours'),
	             ('C1','2.0','v9:1:50','Tax id validation','Thread B asked about tax id validation.', NOW() - interval '3 hours')`)
	if err := rebuildEpicHierarchy(ctx, pool, "test", "PAY", map[string]int{"PAY-100": 0}); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	var calls int
	gem := &mockGemini{}
	gem.generateResult = func() (string, error) {
		calls++
		return `{"brief":"PAY-100 covers GST invoicing for India. Thread A settled the invoice format.",
		         "highlights":[{"text":"Invoice format agreed","sources":["slack:C1:1.0"]},{"text":"made up","sources":["jira:PAY-999"]}],
		         "open_items":[{"text":"Tax id validation pending","sources":["slack:C1:2.0","jira:PAY-102"]}]}`, nil
	}
	deps := Deps{DB: pool, Gemini: gem, Logger: zerolog.Nop(), MachineID: "test"}
	h := NewRefreshEpicBriefHandler(deps).Handler
	run := func(dry bool) {
		t.Helper()
		payload, _ := json.Marshal(refreshEpicBriefPayload{EpicKey: "PAY-100", DryRun: dry})
		if err := h(ctx, payload); err != nil {
			t.Fatalf("handler: %v", err)
		}
	}
	countBriefs := func() int {
		var n int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM graph.epic_briefs`).Scan(&n)
		return n
	}

	// Disabled (default): no call, no row.
	run(false)
	if calls != 0 || countBriefs() != 0 {
		t.Fatalf("disabled: calls=%d rows=%d", calls, countBriefs())
	}
	// Dry run works while disabled: exactly one call, still no row.
	run(true)
	if calls != 1 || countBriefs() != 0 {
		t.Fatalf("dry run: calls=%d rows=%d", calls, countBriefs())
	}
	for _, id := range []string{"[slack:C1:1.0]", "[slack:C1:2.0]", "[jira:PAY-101]", "[jira:PAY-102]", "[gh_pr:wego/payments#1]", "Issue tax invoices"} {
		if !strings.Contains(gem.generateUser, id) {
			t.Errorf("first-build prompt lacks %s:\n%s", id, gem.generateUser)
		}
	}
	if strings.Contains(gem.generateUser, "[jira:PAY-100]") || strings.Contains(gem.generateUser, "[slack:C1:1.5]") {
		t.Errorf("prompt must exclude the epic node and Slack replies:\n%s", gem.generateUser)
	}

	// Enabled: first real build writes the row with filtered citations.
	exec(`INSERT INTO settings(key,value) VALUES('graph.epic_briefs.enabled','true')`)
	run(false)
	if calls != 2 || countBriefs() != 1 {
		t.Fatalf("first build: calls=%d rows=%d", calls, countBriefs())
	}
	br, ok := loadEpicBrief(ctx, pool, "PAY-100")
	if !ok || !strings.HasPrefix(br.Brief, "PAY-100 covers") || br.Previous != "" {
		t.Fatalf("stored brief = %+v", br)
	}
	var hl []briefItem
	_ = json.Unmarshal(br.Highlights, &hl)
	if len(hl) != 2 || len(hl[0].Sources) != 1 || hl[0].Sources[0] != "slack:C1:1.0" || len(hl[1].Sources) != 0 {
		t.Fatalf("highlights citations not filtered to members: %s", br.Highlights)
	}
	if len(br.Sources) != 5 {
		t.Fatalf("sources = %v, want 5 members", br.Sources)
	}

	// Same signature: idempotent, no call.
	run(false)
	if calls != 2 {
		t.Fatalf("unchanged signature must not call the LLM, calls=%d", calls)
	}

	// Thread B re-summarized: signature changes, but the row is < 60 min old.
	exec(`UPDATE graph.thread_summaries SET signature='v9:3:80', overview='Thread B: tax id validation shipped.', updated_at=NOW() WHERE thread_ts='2.0'`)
	run(false)
	if calls != 2 {
		t.Fatalf("interval floor must hold, calls=%d", calls)
	}
	// Age the brief past the floor: delta run carries only thread B and the previous brief.
	exec(`UPDATE graph.epic_briefs SET updated_at = NOW() - interval '2 hours'`)
	gem.generateResult = func() (string, error) {
		calls++
		return `{"brief":"Updated: tax id validation shipped.","highlights":[],"open_items":[]}`, nil
	}
	run(false)
	if calls != 3 {
		t.Fatalf("delta must call once, calls=%d", calls)
	}
	if !strings.Contains(gem.generateUser, "Previous brief") || !strings.Contains(gem.generateUser, "[slack:C1:2.0]") {
		t.Errorf("delta prompt lacks previous brief / changed member:\n%s", gem.generateUser)
	}
	for _, id := range []string{"[slack:C1:1.0]", "[jira:PAY-101]", "[gh_pr:wego/payments#1]"} {
		if strings.Contains(gem.generateUser, id) {
			t.Errorf("delta prompt must not carry unchanged member %s:\n%s", id, gem.generateUser)
		}
	}
	br2, _ := loadEpicBrief(ctx, pool, "PAY-100")
	if br2.Brief != "Updated: tax id validation shipped." || br2.Previous != br.Brief || br2.PreviousAt == nil || len(br2.Sources) != 5 {
		t.Fatalf("delta row = %+v", br2)
	}
	if br2.Signature == br.Signature {
		t.Fatal("signature must advance after a delta build")
	}

	// Read side: /api/graph/epic carries the brief; DM line uses the first sentence.
	if key, first := threadEpicBrief(ctx, pool, "slack:C1:1.0"); key != "PAY-100" || first != "Updated: tax id validation shipped." {
		t.Fatalf("threadEpicBrief = %q %q", key, first)
	}
	if key, _ := threadEpicBrief(ctx, pool, "slack:C1:5.0"); key != "" {
		t.Fatalf("eligible-only message must report no epic, got %q", key)
	}

	// refresh_jira_board's enqueue: nothing when signatures match, one job after a change.
	if n := enqueueEpicBriefs(ctx, deps, "PAY"); n != 0 {
		t.Fatalf("enqueue with matching signature = %d, want 0", n)
	}
	exec(`UPDATE graph.thread_summaries SET signature='v9:9:999' WHERE thread_ts='1.0'`)
	if n := enqueueEpicBriefs(ctx, deps, "PAY"); n != 1 {
		t.Fatalf("enqueue after change = %d, want 1", n)
	}
	if n := enqueueEpicBriefs(ctx, deps, "PAY"); n != 0 {
		t.Fatalf("enqueue must dedup a queued job, got %d", n)
	}
}
