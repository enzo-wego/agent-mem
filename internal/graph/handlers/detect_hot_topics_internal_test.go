package handlers

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
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
	// 4) Senior, different subject: Ross talks about lunch. findHotThreads is now
	// topic-agnostic (semantic filtering happens in the handler), so this passes
	// the HOT gate via seniority; topicMatches would later reject it.
	insSlack(t, pool, "C4", ts(now, 40), "", ross, "anyone up for lunch")

	sub := subscription{Topic: "payments", MinParticipants: 4, MaxAuthorDepth: 2}
	hot, err := findHotThreads(ctx, pool, sub)
	if err != nil {
		t.Fatalf("findHotThreads: %v", err)
	}
	got := map[string]hotThread{}
	for _, h := range hot {
		got[h.Channel] = h
	}
	if _, ok := got["C1"]; !ok {
		t.Errorf("expected C1 (seniority) to fire; got channels %v", keys(got))
	}
	if _, ok := got["C2"]; !ok {
		t.Errorf("expected C2 (volume) to fire; got channels %v", keys(got))
	}
	if _, ok := got["C3"]; ok {
		t.Errorf("C3 is quiet (2 ICs) and must NOT pass the hot gate")
	}
	if _, ok := got["C4"]; !ok {
		t.Errorf("C4 should pass the hot gate via seniority (topic filtered later); got %v", keys(got))
	}
	if c2 := got["C2"]; c2.Participants != 4 {
		t.Errorf("C2 participants = %d, want 4", c2.Participants)
	}
	if c1 := got["C1"]; c1.Blob == "" {
		t.Errorf("C1 blob should be populated for semantic matching")
	}
}

// TestWhyFlagged checks the plain-language reason has no jargon and names the
// senior speaker when known.
func TestWhyFlagged(t *testing.T) {
	s := subscription{MinParticipants: 4, MaxAuthorDepth: 2}
	// Senior + volume, named.
	got := whyFlagged(s, hotThread{TopDepth: 0, Participants: 6}, "Ross")
	if want := "Ross (a senior leader) is involved and 6 people are discussing it"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// Senior only, name unknown ⇒ no "someone", no "org-depth".
	got = whyFlagged(s, hotThread{TopDepth: 0, Participants: 1}, "")
	if want := "a senior leader raised or joined it"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if strings.Contains(got, "org-depth") || strings.Contains(got, "someone") {
		t.Errorf("reason still has jargon: %q", got)
	}
	// Volume only.
	got = whyFlagged(s, hotThread{TopDepth: 9, Participants: 5}, "")
	if want := "5 people are actively discussing it"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestCosine checks the similarity helper: identical vectors ≈ 1, orthogonal ≈ 0.
func TestCosine(t *testing.T) {
	a := []float32{1, 0, 1}
	if s := cosine(a, a); s < 0.999 {
		t.Errorf("cosine(a,a) = %v, want ~1", s)
	}
	if s := cosine([]float32{1, 0}, []float32{0, 1}); s != 0 {
		t.Errorf("cosine(orthogonal) = %v, want 0", s)
	}
	if s := cosine([]float32{1, 2}, nil); s != 0 {
		t.Errorf("cosine(mismatched) = %v, want 0", s)
	}
}

// TestTopicMatches_KeywordFallback verifies that with no embedder, topicMatches
// falls back to a literal keyword check over thread text + channel name.
func TestTopicMatches_KeywordFallback(t *testing.T) {
	deps := Deps{} // no Gemini ⇒ keyword fallback
	s := subscription{Topic: "payments"}
	if !topicMatches(context.Background(), deps, s, nil, hotThread{Blob: "the payments service is down"}) {
		t.Errorf("expected keyword match on blob")
	}
	if !topicMatches(context.Background(), deps, s, nil, hotThread{ChannelName: "payments-ops"}) {
		t.Errorf("expected keyword match on channel name")
	}
	if topicMatches(context.Background(), deps, s, nil, hotThread{Blob: "lunch plans"}) {
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
