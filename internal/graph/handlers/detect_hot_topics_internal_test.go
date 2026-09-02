package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

// TestFindHotThreads verifies the two triggers (seniority OR volume), the topic
// gate, and that a quiet on-topic thread does not fire.
func TestFindHotThreads(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	for _, tbl := range []string{"graph.topic_notifications", "graph.topic_subscriptions", "graph.nodes", "graph.people"} {
		if _, err := pool.Exec(ctx, "DELETE FROM "+tbl); err != nil {
			t.Fatalf("clean %s: %v", tbl, err)
		}
	}

	ross := insPerson(t, pool, "Ross", 0)
	ic := []int64{
		insPerson(t, pool, "IC One", 5),
		insPerson(t, pool, "IC Two", 5),
		insPerson(t, pool, "IC Three", 5),
		insPerson(t, pool, "IC Four", 5),
	}

	now := time.Now()
	// 1) Seniority trigger: Ross posts a standalone "payments" message.
	insSlack(t, pool, "C1", ts(now, 0), "", ross, "payments are failing for TripleA")
	// 2) Volume trigger: 4 distinct ICs in one thread about payments.
	root := ts(now, 10)
	for i, p := range ic {
		insSlack(t, pool, "C2", ts(now, 11+i), root, p, "discussing payments rollout")
	}
	// 3) Quiet on-topic thread: 2 ICs only → neither trigger. Must NOT match.
	qroot := ts(now, 30)
	insSlack(t, pool, "C3", ts(now, 31), qroot, ic[0], "payments minor question")
	insSlack(t, pool, "C3", ts(now, 32), qroot, ic[1], "payments reply")
	// 4) Single message, different subject (Ross/lunch). With the seniority gate
	// dropped, a lone message must NOT fire regardless of author.
	insSlack(t, pool, "C4", ts(now, 40), "", ross, "anyone up for lunch")

	// Volume-only gate: ≥ min_participants distinct people.
	sub := subscription{Topic: "payments", MinParticipants: 4}
	hot, err := findHotThreads(ctx, pool, sub, nil, nil, "")
	if err != nil {
		t.Fatalf("findHotThreads: %v", err)
	}
	got := map[string]hotThread{}
	for _, h := range hot {
		got[h.Channel] = h
	}
	if _, ok := got["C2"]; !ok {
		t.Errorf("expected C2 (4 participants) to fire; got channels %v", keys(got))
	}
	for _, ch := range []string{"C1", "C3", "C4"} {
		if _, ok := got[ch]; ok {
			t.Errorf("%s has < 4 participants and must NOT fire (seniority dropped)", ch)
		}
	}
	if c2 := got["C2"]; c2.Participants != 4 {
		t.Errorf("C2 participants = %d, want 4", c2.Participants)
	}
	if c2 := got["C2"]; c2.Blob == "" {
		t.Errorf("C2 blob should be populated for semantic matching")
	}

	// With min_participants=2, a reporter+responder thread (C3) also fires.
	hot2, _ := findHotThreads(ctx, pool, subscription{Topic: "payments", MinParticipants: 2}, nil, nil, "")
	got2 := map[string]bool{}
	for _, h := range hot2 {
		got2[h.Channel] = true
	}
	if !got2["C3"] {
		t.Errorf("C3 (2 participants) should fire at min_participants=2")
	}
	if got2["C1"] || got2["C4"] {
		t.Errorf("single-message threads must never fire")
	}
}

// TestFindHotThreadsSkipsSubscriberThreads verifies that a thread the subscriber
// has posted in is dropped (Slack already notifies them), while the same thread
// fires for a subscriber who is not in it.
func TestFindHotThreadsSkipsSubscriberThreads(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	for _, tbl := range []string{"graph.nodes", "graph.people"} {
		if _, err := pool.Exec(ctx, "DELETE FROM "+tbl); err != nil {
			t.Fatalf("clean %s: %v", tbl, err)
		}
	}

	me := insPerson(t, pool, "Owner", 5)
	if _, err := pool.Exec(ctx,
		`UPDATE graph.people SET slack_user_id='UOWNER' WHERE id=$1`, me); err != nil {
		t.Fatalf("set slack_user_id: %v", err)
	}
	other := insPerson(t, pool, "Colleague", 5)

	now := time.Now()
	// Thread the owner is in.
	mine := ts(now, 10)
	insSlack(t, pool, "C1", ts(now, 11), mine, me, "I am driving this release")
	insSlack(t, pool, "C1", ts(now, 12), mine, other, "ack, go ahead")
	// Thread where the owner speaks only under a merged duplicate identity (a
	// GitHub/Jira person row with no slack_user_id, merged into the owner).
	dup := insPerson(t, pool, "enzo-gh", 5)
	if _, err := pool.Exec(ctx, `UPDATE graph.people SET merged_into=$1 WHERE id=$2`, me, dup); err != nil {
		t.Fatalf("merge dup: %v", err)
	}
	merged := ts(now, 15)
	insSlack(t, pool, "C3", ts(now, 16), merged, dup, "same human, other identity")
	insSlack(t, pool, "C3", ts(now, 17), merged, other, "reply")
	// Thread the owner is not in.
	theirs := ts(now, 20)
	insSlack(t, pool, "C2", ts(now, 21), theirs, other, "payments question")
	insSlack(t, pool, "C2", ts(now, 22), theirs, insPerson(t, pool, "Third", 5), "reply")

	sub := subscription{Topic: "payments", MinParticipants: 2}
	hot, err := findHotThreads(ctx, pool, sub, nil, nil, "UOWNER")
	if err != nil {
		t.Fatalf("findHotThreads: %v", err)
	}
	got := map[string]bool{}
	for _, h := range hot {
		got[h.Channel] = true
	}
	if got["C1"] {
		t.Errorf("C1 has a message from the subscriber and must not fire")
	}
	if got["C3"] {
		t.Errorf("C3 has a message from a merged identity of the subscriber and must not fire")
	}
	if !got["C2"] {
		t.Errorf("C2 has no subscriber message and must still fire; got %v", got)
	}

	// Empty subscriber disables the filter entirely.
	all, err := findHotThreads(ctx, pool, sub, nil, nil, "")
	if err != nil {
		t.Fatalf("findHotThreads(no subscriber): %v", err)
	}
	if len(all) != 3 {
		t.Errorf("with no subscriber all three threads should fire; got %d", len(all))
	}
}

// TestSourceParsers checks Confluence page-id and GitHub repo extraction.
func TestSourceParsers(t *testing.T) {
	if m := cfPageIDRe.FindStringSubmatch("https://wegomushi.atlassian.net/wiki/spaces/PA/pages/2122252293/Payment+PRDs"); m == nil || m[1] != "2122252293" {
		t.Errorf("cfPageIDRe failed: %v", m)
	}
	if m := ghRepoRe.FindStringSubmatch("https://github.com/wego/payments"); m == nil || m[1] != "wego/payments" {
		t.Errorf("ghRepoRe failed: %v", m)
	}
	if m := ghRepoRe.FindStringSubmatch("git@github.com:wego/payments.git"); m == nil || m[1] != "wego/payments.git" {
		t.Errorf("ghRepoRe ssh failed: %v", m)
	}
}

// TestSlackNodeIDFromURL checks the inverse of slackPermalink parses only the
// path, so Slack's "Copy link" query/fragment is ignored, and rejects malformed
// shapes.
func TestSlackNodeIDFromURL(t *testing.T) {
	const wantID = "slack:C0AV14LGPMG:1782118242.921599"
	cases := []struct {
		name, url, want string
	}{
		{"full url with query", "https://wego.slack.com/archives/C0AV14LGPMG/p1782118242921599?thread_ts=1781081424.346499&cid=C0AV14LGPMG", wantID},
		{"bare", "https://wego.slack.com/archives/C0AV14LGPMG/p1782118242921599", wantID},
		{"trailing slash", "https://wego.slack.com/archives/C0AV14LGPMG/p1782118242921599/", wantID},
		{"fragment", "https://wego.slack.com/archives/C0AV14LGPMG/p1782118242921599#thread-anchor", wantID},
		{"non-slack", "https://github.com/wego/payments/pull/2198", ""},
		{"missing p prefix", "https://wego.slack.com/archives/C0AV14LGPMG/1782118242921599", ""},
		{"non-numeric p segment", "https://wego.slack.com/archives/C0AV14LGPMG/pXYZ1234567", ""},
		{"too few digits", "https://wego.slack.com/archives/C0AV14LGPMG/p123456", ""},
		{"wrong path root", "https://wego.slack.com/team/C0AV14LGPMG", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		if got := slackNodeIDFromURL(tc.url); got != tc.want {
			t.Errorf("%s: slackNodeIDFromURL(%q) = %q, want %q", tc.name, tc.url, got, tc.want)
		}
	}
	// Round-trips with slackPermalink, the function it inverts.
	if got := slackNodeIDFromURL(slackPermalink(wantID)); got != wantID {
		t.Errorf("round-trip via slackPermalink: got %q, want %q", got, wantID)
	}
}

// TestStripQueryFragment checks tracking params and fragments are removed while
// non-URL inputs and query-less URLs pass through unchanged.
func TestStripQueryFragment(t *testing.T) {
	cases := map[string]string{
		"https://wegomushi.atlassian.net/browse/PAY-2128?utm_source=slack": "https://wegomushi.atlassian.net/browse/PAY-2128",
		"https://github.com/wego/payments/pull/2198#issuecomment-1":        "https://github.com/wego/payments/pull/2198",
		"https://example.com/a/b":                                          "https://example.com/a/b",
		"slack:C:1":                                                        "slack:C:1",
		"":                                                                 "",
	}
	for in, want := range cases {
		if got := stripQueryFragment(in); got != want {
			t.Errorf("stripQueryFragment(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestWhyFlagged checks the plain-language reason is jargon-free and names an
// important sender when present.
func TestWhyFlagged(t *testing.T) {
	got := whyFlagged(hotThread{Participants: 6})
	if want := "6 people are discussing it"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if strings.Contains(got, "org-depth") || strings.Contains(got, "someone") || strings.Contains(got, "senior") {
		t.Errorf("reason still has jargon: %q", got)
	}
	// Important lone message.
	got = whyFlagged(hotThread{Participants: 1, HasImportant: true, ImportantAuthor: "Lei Zheng"})
	if want := "Lei Zheng (important to you) raised it"; got != want {
		t.Errorf("important lone: got %q, want %q", got, want)
	}
	// Important + discussion.
	got = whyFlagged(hotThread{Participants: 3, HasImportant: true, ImportantAuthor: "Ross"})
	if want := "Ross (important to you) is involved and 3 people are discussing it"; got != want {
		t.Errorf("important+discussion: got %q, want %q", got, want)
	}
}

// TestHumanizeSlack checks Slack mention/link codes resolve to readable text.
func TestHumanizeSlack(t *testing.T) {
	names := map[string]string{"U024HMWA6": "Ross Veitch"}
	cases := map[string]string{
		"hey <@U024HMWA6> can you check":         "hey @Ross Veitch can you check",
		"unknown <@U999XYZ>":                     "unknown @U999XYZ",
		"see <#C0B1BR522F5|payments-ops> please": "see #payments-ops please",
		"<!here> deploy is broken":               "@here deploy is broken",
		"docs <https://x.com/p|the PR> landed":   "docs the PR landed",
		"raw <https://api.datadoghq.com> link":   "raw https://api.datadoghq.com link",
	}
	for in, want := range cases {
		if got := humanizeSlack(in, names); got != want {
			t.Errorf("humanizeSlack(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestFindHotThreads_ImportantLoneMessage: a single message from an important
// person surfaces even when the participant gate fails.
func TestFindHotThreads_ImportantLoneMessage(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	for _, tbl := range []string{"graph.nodes", "graph.people"} {
		_, _ = pool.Exec(ctx, "DELETE FROM "+tbl)
	}
	var boss int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO graph.people (eeid, display_name, machine_id) VALUES (7,'Boss','test') RETURNING id`).Scan(&boss); err != nil {
		t.Fatalf("seed boss: %v", err)
	}
	now := time.Now()
	insSlack(t, pool, "CB", ts(now, 0), "", boss, "payments are down in PK")

	// min_participants=4 (volume gate fails for a lone msg); important=[7] must surface it.
	hot, err := findHotThreads(ctx, pool, subscription{Topic: "payments", MinParticipants: 4}, []int32{7}, nil, "")
	if err != nil {
		t.Fatalf("findHotThreads: %v", err)
	}
	var cb *hotThread
	for i := range hot {
		if hot[i].Channel == "CB" {
			cb = &hot[i]
		}
	}
	if cb == nil {
		t.Fatal("important lone message should surface despite participants<min")
	}
	if !cb.HasImportant || cb.ImportantAuthor != "Boss" {
		t.Errorf("HasImportant=%v ImportantAuthor=%q, want true/Boss", cb.HasImportant, cb.ImportantAuthor)
	}
	// Without the important set, the lone message must NOT surface.
	hot0, _ := findHotThreads(ctx, pool, subscription{Topic: "payments", MinParticipants: 4}, nil, nil, "")
	for _, h := range hot0 {
		if h.Channel == "CB" {
			t.Errorf("lone message must not surface without importance")
		}
	}
}

// TestDetectHotTopics_AlwaysAlertBypassesOnlyRelevance proves that an explicit
// always-alert author ignores cached and fresh negative relevance verdicts,
// while the wider important set, channel ignore list, subscriber suppression,
// and judgment/notification dedup behavior remain unchanged.
func TestDetectHotTopics_AlwaysAlertBypassesOnlyRelevance(t *testing.T) {
	pool := openTestDB(t)
	ctx := context.Background()
	for _, tbl := range []string{
		"graph.topic_notifications",
		"graph.topic_judgments",
		"graph.topic_subscriptions",
		"graph.thread_summaries",
	} {
		if _, err := pool.Exec(ctx, "DELETE FROM "+tbl); err != nil {
			t.Fatalf("clean %s: %v", tbl, err)
		}
	}
	truncateGraphHandlerTables(t, pool)

	insPersonWithEEID := func(name, slack string, eeid int32) int64 {
		t.Helper()
		var id int64
		if err := pool.QueryRow(ctx, `
INSERT INTO graph.people (eeid, display_name, slack_user_id, depth_from_root, machine_id)
VALUES ($1,$2,NULLIF($3,''),5,'test') RETURNING id`, eeid, name, slack).Scan(&id); err != nil {
			t.Fatalf("seed person %s: %v", name, err)
		}
		return id
	}
	owner := insPersonWithEEID("Enzo Nguyen", "UOWNER", 982)
	ross := insPersonWithEEID("Ross Veitch", "UROSS", 452)
	alex := insPersonWithEEID("Alexandre Morin", "UALEX", 1001)

	now := time.Now()
	freshTS := ts(now, 10)
	cachedTS := ts(now, 20)
	alexTS := ts(now, 30)
	ignoredTS := ts(now, 40)
	subscriberTS := ts(now, 50)
	insSlack(t, pool, "CROSSFRESH", freshTS, "", ross, "company strategy and hiring")
	insSlack(t, pool, "CROSSCACHED", cachedTS, "", ross, "office planning")
	insSlack(t, pool, "CALEX", alexTS, "", alex, "team lunch")
	insSlack(t, pool, "C0B1BR522F5", ignoredTS, "", ross, "board preparation")
	insSlack(t, pool, "CSUBSCRIBER", subscriberTS, subscriberTS, ross, "annual planning")
	insSlack(t, pool, "CSUBSCRIBER", ts(now, 51), subscriberTS, owner, "I am already in this thread")

	subID := insSubscription(t, pool, "UOWNER", "payments")
	freshID := "slack:CROSSFRESH:" + freshTS
	cachedID := "slack:CROSSCACHED:" + cachedTS
	alexID := "slack:CALEX:" + alexTS
	ignoredID := "slack:C0B1BR522F5:" + ignoredTS
	subscriberID := "slack:CSUBSCRIBER:" + subscriberTS
	if _, err := pool.Exec(ctx, `
INSERT INTO graph.topic_judgments (subscription_id, root_node_id, msg_count, relevant)
VALUES ($1,$2,1,false)`, subID, cachedID); err != nil {
		t.Fatalf("seed cached negative judgment: %v", err)
	}
	for _, summary := range []struct{ channel, threadTS string }{
		{"CROSSFRESH", freshTS},
		{"CROSSCACHED", cachedTS},
	} {
		if _, err := pool.Exec(ctx, `
INSERT INTO graph.thread_summaries (channel_id, thread_ts, signature, summary, overview)
VALUES ($1,$2,'1:test','cached','cached overview')`, summary.channel, summary.threadTS); err != nil {
			t.Fatalf("seed thread summary %s: %v", summary.channel, err)
		}
	}

	var judgeCalls int
	gem := &mockGemini{generateResult: func() (string, error) {
		judgeCalls++
		return `{"relevant":false}`, nil
	}}
	var posted int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "chat.postMessage") {
			posted++
		}
		_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"DTEST"},"ts":"1700000000.000001"}`))
	}))
	defer srv.Close()
	oldBase, oldHTTP := slackAPIBaseURL, slackHTTP
	slackAPIBaseURL, slackHTTP = srv.URL, srv.Client()
	t.Cleanup(func() { slackAPIBaseURL, slackHTTP = oldBase, oldHTTP })

	deps := Deps{
		DB:            pool,
		Gemini:        gem,
		Logger:        zerolog.Nop(),
		MachineID:     "test",
		SlackBotToken: "xoxb-placeholder-not-real",
	}
	if err := NewDetectHotTopics(deps)(ctx, nil); err != nil {
		t.Fatalf("detect hot topics: %v", err)
	}
	if judgeCalls != 1 {
		t.Fatalf("LLM calls = %d, want 1 for Alexandre only; Ross must bypass relevance", judgeCalls)
	}
	if posted != 2 {
		t.Fatalf("Slack posts = %d, want 2 for Ross's fresh and cached-negative threads", posted)
	}

	notified := loadNotified(ctx, pool, subID)
	for _, id := range []string{freshID, cachedID} {
		if !notified[id] {
			t.Errorf("always-alert thread %s was not notified", id)
		}
	}
	for _, id := range []string{alexID, ignoredID, subscriberID} {
		if notified[id] {
			t.Errorf("suppressed thread %s was notified", id)
		}
	}
	var freshJudgments int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM graph.topic_judgments
WHERE subscription_id=$1 AND root_node_id=$2`, subID, freshID).Scan(&freshJudgments); err != nil {
		t.Fatalf("count fresh Ross judgments: %v", err)
	}
	if freshJudgments != 0 {
		t.Errorf("fresh always-alert judgments = %d, want 0", freshJudgments)
	}
	var alexRelevant bool
	if err := pool.QueryRow(ctx, `
SELECT relevant FROM graph.topic_judgments
WHERE subscription_id=$1 AND root_node_id=$2`, subID, alexID).Scan(&alexRelevant); err != nil {
		t.Fatalf("read Alexandre judgment: %v", err)
	}
	if alexRelevant {
		t.Error("Alexandre's negative relevance judgment was not preserved")
	}
}

// TestTopicMatches_LLMJudge verifies the LLM yes/no relevance gate is honored.
func TestTopicMatches_LLMJudge(t *testing.T) {
	gem := &mockGemini{}
	deps := Deps{Gemini: gem, Logger: zerolog.Nop()}
	s := subscription{Topic: "payments"}

	gem.generateResult = func() (string, error) { return `{"relevant": true}`, nil }
	relevant, fromLLM := topicMatches(context.Background(), deps, s, hotThread{Blob: "juspay blocked pk ip, 403 on card"})
	if !relevant {
		t.Errorf("relevant=true should match")
	}
	if !fromLLM {
		t.Errorf("LLM verdict must report fromLLM=true (cacheable)")
	}
	gem.generateResult = func() (string, error) { return `{"relevant": false}`, nil }
	if relevant, _ := topicMatches(context.Background(), deps, s, hotThread{Blob: "aws secret missing, deployment failed"}); relevant {
		t.Errorf("relevant=false should NOT match")
	}
}

// TestTopicMatches_KeywordFallback verifies that with no LLM, topicMatches
// falls back to a literal keyword check over thread text + channel name.
func TestTopicMatches_KeywordFallback(t *testing.T) {
	deps := Deps{} // no Gemini ⇒ keyword fallback
	s := subscription{Topic: "payments"}
	relevant, fromLLM := topicMatches(context.Background(), deps, s, hotThread{Blob: "the payments service is down"})
	if !relevant {
		t.Errorf("expected keyword match on blob")
	}
	if fromLLM {
		t.Errorf("keyword fallback must report fromLLM=false (not cacheable)")
	}
	if relevant, _ := topicMatches(context.Background(), deps, s, hotThread{ChannelName: "payments-ops"}); !relevant {
		t.Errorf("expected keyword match on channel name")
	}
	if relevant, _ := topicMatches(context.Background(), deps, s, hotThread{Blob: "lunch plans"}); relevant {
		t.Errorf("did not expect match for unrelated text")
	}
}

func keys(m map[string]hotThread) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func ts(base time.Time, addSec int) string {
	return fmt.Sprintf("%d.000000", base.Add(time.Duration(addSec)*time.Second).Unix())
}

func insPerson(t *testing.T, pool *pgxpool.Pool, name string, depth int) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO graph.people (display_name, depth_from_root, machine_id)
		 VALUES ($1,$2,'test') RETURNING id`, name, depth).Scan(&id)
	if err != nil {
		t.Fatalf("insPerson %s: %v", name, err)
	}
	return id
}

func insSlack(t *testing.T, pool *pgxpool.Pool, channel, ts, threadTS string, personID int64, body string) {
	t.Helper()
	id := "slack:" + channel + ":" + ts
	meta := fmt.Sprintf(`{"ts":%q,"thread_ts":%q}`, ts, threadTS)
	_, err := pool.Exec(context.Background(), `
INSERT INTO graph.nodes (id, type, natural_key, body, scope, metadata, author_person_id, created_at, first_seen_at, machine_id)
VALUES ($1,'slack',$1,$2,$3,$4::jsonb,$5,NOW(),NOW(),'test')
ON CONFLICT (id) DO NOTHING`, id, body, "slack:"+channel, meta, personID)
	if err != nil {
		t.Fatalf("insSlack %s: %v", id, err)
	}
}

// TestFindHotThreads_AttachmentBlob proves attachment OCR/description reaches the
// judge blob (criterion 1), that a no-attachment thread's blob is byte-identical
// to the message text (criterion 2), that the LATERAL join does not fan out rows
// so msg_count/participants are unchanged (criterion 3), and that a huge OCR is
// capped so it can't evict the message text (criterion 4).
//
// The image fixture writes the OCR into artifact_bodies.body_full — the ONLY
// column describe_attachment populates in prod (body_full = description +
// "\n\nOCR:\n" + ocr) — so a green test proves the production path, not a
// fictional one. A second attachment exercises the description/ocr_text columns,
// which are populated only on rows synced from another machine.
func TestFindHotThreads_AttachmentBlob(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	// Delete children before parents; nodes cascades to edges/artifact_bodies but
	// be explicit.
	for _, tbl := range []string{"graph.topic_notifications", "graph.artifact_bodies", "graph.edges", "graph.nodes", "graph.people"} {
		if _, err := pool.Exec(ctx, "DELETE FROM "+tbl); err != nil {
			t.Fatalf("clean %s: %v", tbl, err)
		}
	}

	now := time.Now()
	author := insPerson(t, pool, "Reporter", 5)

	// CIMG: a lone message + a screenshot whose defect is legible only in OCR
	// (mirrors the ValU incident). body_full carries the OCR, exactly as
	// describe_attachment writes it in prod.
	const ocrMarker = "Pay with ValU redirect description inconsistent"
	const imgText = "who can update this text"
	imgMsg := ts(now, 0)
	insSlack(t, pool, "CIMG", imgMsg, "", author, imgText)
	insSlackFile(t, pool, "FIMG", "payments-form.png")
	insEdge(t, pool, "slack:CIMG:"+imgMsg, "slack_file:FIMG", "REFERENCES")
	insArtifactBody(t, pool, "slack_file:FIMG",
		"A screenshot of the Wego payments form.\n\nOCR:\n"+ocrMarker, "", "")

	// CSYNC: attachment content lives in description/ocr_text (the shape of a row
	// synced from another machine); body_full is a placeholder.
	const syncMarker = "SYNCED_OCR_TOKEN"
	syncMsg := ts(now, 5)
	insSlack(t, pool, "CSYNC", syncMsg, "", author, "see attached")
	insSlackFile(t, pool, "FSYNC", "diagram.png")
	insEdge(t, pool, "slack:CSYNC:"+syncMsg, "slack_file:FSYNC", "REFERENCES")
	insArtifactBody(t, pool, "slack_file:FSYNC", "placeholder", "A diagram", syncMarker)

	// CPLAIN: a lone message, no attachment. Its blob must equal the message text
	// byte-for-byte (regression, criterion 2).
	const plainText = "just a normal message with no files"
	plainMsg := ts(now, 10)
	insSlack(t, pool, "CPLAIN", plainMsg, "", author, plainText)

	// CBIG: a 3-message thread with a 5000-char OCR on the root. Checks no row
	// fan-out (criterion 3) and the 500-char cap (criterion 4).
	const bigMsgText = "first message before the huge screenshot"
	author2 := insPerson(t, pool, "Reporter Two", 5)
	author3 := insPerson(t, pool, "Reporter Three", 5)
	bigRoot := ts(now, 20)
	insSlack(t, pool, "CBIG", bigRoot, bigRoot, author, bigMsgText)
	insSlack(t, pool, "CBIG", ts(now, 21), bigRoot, author2, "second reply")
	insSlack(t, pool, "CBIG", ts(now, 22), bigRoot, author3, "third reply")
	insSlackFile(t, pool, "FBIG", "huge.png")
	insEdge(t, pool, "slack:CBIG:"+bigRoot, "slack_file:FBIG", "REFERENCES")
	insArtifactBody(t, pool, "slack_file:FBIG", "Screenshot.\n\nOCR:\n"+strings.Repeat("Z", 5000), "", "")

	// min_participants=1 surfaces every thread. Topic is unused by findHotThreads
	// (relevance is judged later in the handler).
	hot, err := findHotThreads(ctx, pool, subscription{Topic: "", MinParticipants: 1}, nil, nil, "")
	if err != nil {
		t.Fatalf("findHotThreads: %v", err)
	}
	got := map[string]hotThread{}
	for _, h := range hot {
		got[h.Channel] = h
	}

	// Criterion 1 (prod shape): image OCR from body_full is in the blob, and the
	// message text survived.
	if h, ok := got["CIMG"]; !ok {
		t.Fatalf("CIMG missing; channels=%v", keys(got))
	} else {
		if !strings.Contains(h.Blob, ocrMarker) {
			t.Errorf("CIMG blob missing OCR marker.\nblob=%q", h.Blob)
		}
		if !strings.Contains(h.Blob, imgText) {
			t.Errorf("CIMG blob dropped the message text.\nblob=%q", h.Blob)
		}
	}

	// Criterion 1 (sync shape): description/ocr_text are read too.
	if h, ok := got["CSYNC"]; !ok {
		t.Fatalf("CSYNC missing; channels=%v", keys(got))
	} else if !strings.Contains(h.Blob, syncMarker) {
		t.Errorf("CSYNC blob missing synced OCR token.\nblob=%q", h.Blob)
	}

	// Criterion 2: no-attachment blob is byte-identical to the message text.
	if h, ok := got["CPLAIN"]; !ok {
		t.Fatalf("CPLAIN missing")
	} else if h.Blob != plainText {
		t.Errorf("CPLAIN blob = %q, want byte-identical %q", h.Blob, plainText)
	}

	// Criteria 3 & 4.
	if h, ok := got["CBIG"]; !ok {
		t.Fatalf("CBIG missing")
	} else {
		if h.MsgCount != 3 {
			t.Errorf("CBIG msg_count = %d, want 3 (attachment must not fan out rows)", h.MsgCount)
		}
		if h.Participants != 3 {
			t.Errorf("CBIG participants = %d, want 3", h.Participants)
		}
		if !strings.Contains(h.Blob, bigMsgText) {
			t.Errorf("CBIG blob dropped message text under a 5000-char OCR.\nblob=%q", firstLine(h.Blob, 200))
		}
		if z := strings.Count(h.Blob, "Z"); z == 0 {
			t.Errorf("CBIG blob has no OCR content; expected up to 500 capped chars")
		} else if z > 500 {
			t.Errorf("CBIG OCR contribution = %d chars, want ≤500 (per-attachment cap)", z)
		}
	}
}

func insSlackFile(t *testing.T, pool *pgxpool.Pool, fileID, title string) {
	t.Helper()
	id := "slack_file:" + fileID
	_, err := pool.Exec(context.Background(), `
INSERT INTO graph.nodes (id, type, natural_key, title, machine_id)
VALUES ($1,'slack_file',$2,$3,'test')
ON CONFLICT (id) DO NOTHING`, id, fileID, title)
	if err != nil {
		t.Fatalf("insSlackFile %s: %v", id, err)
	}
}

func insEdge(t *testing.T, pool *pgxpool.Pool, from, to, kind string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO graph.edges (from_node_id, to_node_id, kind, machine_id)
VALUES ($1,$2,$3,'test')
ON CONFLICT (from_node_id, to_node_id, kind) DO NOTHING`, from, to, kind)
	if err != nil {
		t.Fatalf("insEdge %s->%s: %v", from, to, err)
	}
}

// insArtifactBody upserts a body. Empty description/ocr are stored as NULL to
// mirror the prod describe_attachment write (only body_full populated).
func insArtifactBody(t *testing.T, pool *pgxpool.Pool, nodeID, bodyFull, description, ocr string) {
	t.Helper()
	var descArg, ocrArg any
	if description != "" {
		descArg = description
	}
	if ocr != "" {
		ocrArg = ocr
	}
	_, err := pool.Exec(context.Background(), `
INSERT INTO graph.artifact_bodies (node_id, body_full, description, ocr_text, machine_id)
VALUES ($1,$2,$3,$4,'test')
ON CONFLICT (node_id) DO UPDATE SET
	body_full   = EXCLUDED.body_full,
	description = EXCLUDED.description,
	ocr_text    = EXCLUDED.ocr_text`, nodeID, bodyFull, descArg, ocrArg)
	if err != nil {
		t.Fatalf("insArtifactBody %s: %v", nodeID, err)
	}
}
