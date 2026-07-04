package handlers

import (
	"context"
	"fmt"
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
	hot, err := findHotThreads(ctx, pool, sub, nil)
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
	hot2, _ := findHotThreads(ctx, pool, subscription{Topic: "payments", MinParticipants: 2}, nil)
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
		"hey <@U024HMWA6> can you check":           "hey @Ross Veitch can you check",
		"unknown <@U999XYZ>":                       "unknown @U999XYZ",
		"see <#C0B1BR522F5|payments-ops> please":   "see #payments-ops please",
		"<!here> deploy is broken":                 "@here deploy is broken",
		"docs <https://x.com/p|the PR> landed":     "docs the PR landed",
		"raw <https://api.datadoghq.com> link":     "raw https://api.datadoghq.com link",
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
	hot, err := findHotThreads(ctx, pool, subscription{Topic: "payments", MinParticipants: 4}, []int32{7})
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
	hot0, _ := findHotThreads(ctx, pool, subscription{Topic: "payments", MinParticipants: 4}, nil)
	for _, h := range hot0 {
		if h.Channel == "CB" {
			t.Errorf("lone message must not surface without importance")
		}
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
