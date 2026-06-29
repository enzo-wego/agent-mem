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
	hot, err := findHotThreads(ctx, pool, sub)
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
	hot2, _ := findHotThreads(ctx, pool, subscription{Topic: "payments", MinParticipants: 2})
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

// TestWhyFlagged checks the plain-language reason is jargon-free.
func TestWhyFlagged(t *testing.T) {
	got := whyFlagged(hotThread{Participants: 6})
	if want := "6 people are discussing it"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if strings.Contains(got, "org-depth") || strings.Contains(got, "someone") || strings.Contains(got, "senior") {
		t.Errorf("reason still has jargon: %q", got)
	}
}

// TestTopicMatches_LLMJudge verifies the LLM yes/no relevance gate is honored.
func TestTopicMatches_LLMJudge(t *testing.T) {
	gem := &mockGemini{}
	deps := Deps{Gemini: gem, Logger: zerolog.Nop()}
	s := subscription{Topic: "payments"}

	gem.generateResult = func() (string, error) { return `{"relevant": true}`, nil }
	if !topicMatches(context.Background(), deps, s, hotThread{Blob: "juspay blocked pk ip, 403 on card"}) {
		t.Errorf("relevant=true should match")
	}
	gem.generateResult = func() (string, error) { return `{"relevant": false}`, nil }
	if topicMatches(context.Background(), deps, s, hotThread{Blob: "aws secret missing, deployment failed"}) {
		t.Errorf("relevant=false should NOT match")
	}
}

// TestTopicMatches_KeywordFallback verifies that with no LLM, topicMatches
// falls back to a literal keyword check over thread text + channel name.
func TestTopicMatches_KeywordFallback(t *testing.T) {
	deps := Deps{} // no Gemini ⇒ keyword fallback
	s := subscription{Topic: "payments"}
	if !topicMatches(context.Background(), deps, s, hotThread{Blob: "the payments service is down"}) {
		t.Errorf("expected keyword match on blob")
	}
	if !topicMatches(context.Background(), deps, s, hotThread{ChannelName: "payments-ops"}) {
		t.Errorf("expected keyword match on channel name")
	}
	if topicMatches(context.Background(), deps, s, hotThread{Blob: "lunch plans"}) {
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
