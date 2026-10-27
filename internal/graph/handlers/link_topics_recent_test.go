package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pgvector/pgvector-go"
	"github.com/rs/zerolog"
)

func topicVector(cosine float64) []float32 {
	v := make([]float32, GraphEmbeddingDims)
	v[0], v[1] = float32(cosine), float32(math.Sqrt(1-cosine*cosine))
	return v
}

func seedRecentThread(t *testing.T, deps Deps, id, scope, summary string, cosine float64, activity time.Time, author *int64, markets []string, stored bool) {
	t.Helper()
	ctx := context.Background()
	threadTS := strings.Split(id, ":")[2]
	meta := fmt.Sprintf(`{"ts":"%d","thread_ts":%q}`, activity.Unix(), threadTS)
	if _, err := deps.DB.Exec(ctx, `INSERT INTO graph.nodes(id,type,natural_key,title,body,scope,metadata,author_person_id,first_seen_at,updated_at,machine_id)
VALUES($1,'slack_thread',$1,$1,$3,$2,$4::jsonb,$5,$6,$6,'test')`, id, scope, summary, meta, author, activity); err != nil {
		t.Fatalf("seed node %s: %v", id, err)
	}
	if _, err := deps.DB.Exec(ctx, `INSERT INTO graph.artifact_index(node_id,summary,summary_kind,embedding,identifiers,refreshed_at,machine_id)
VALUES($1,$2,'thread_summary',$3,'{}',NOW(),'test')`, id, summary, pgvector.NewVector(topicVector(cosine))); err != nil {
		t.Fatalf("seed index %s: %v", id, err)
	}
	if stored {
		ch := strings.TrimPrefix(scope, "slack:")
		if _, err := deps.DB.Exec(ctx, `INSERT INTO graph.thread_summaries(channel_id,thread_ts,signature,summary,kind,markets) VALUES($1,$2,'test',$3,'substantive',$4)`, ch, threadTS, summary, markets); err != nil {
			t.Fatalf("seed summary %s: %v", id, err)
		}
	}
}

func candidateByID(cands []topicLinkCandidate, id string) (topicLinkCandidate, bool) {
	for _, c := range cands {
		if c.NodeID == id {
			return c, true
		}
	}
	return topicLinkCandidate{}, false
}

func TestTopicMateCandidatesGatesAndVia(t *testing.T) {
	pool := caseTestDB(t)
	deps := Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "test"}
	ctx := context.Background()
	var author, other int64
	if err := pool.QueryRow(ctx, `INSERT INTO graph.people(display_name,machine_id) VALUES('topic author','test') RETURNING id`).Scan(&author); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO graph.people(display_name,machine_id) VALUES('other author','test') RETURNING id`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Exec(ctx, `UPDATE graph.nodes SET author_person_id=NULL WHERE author_person_id IN ($1,$2)`, author, other)
		pool.Exec(ctx, `DELETE FROM graph.people WHERE id IN ($1,$2)`, author, other)
	})
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	const a = "slack:CT1:1790812800"
	seedRecentThread(t, deps, a, "slack:CT1", strings.Repeat("source work ", 8), 1, now, &author, nil, false)
	for _, f := range []struct {
		id, scope string
		cosine    float64
		who       *int64
	}{
		{"slack:CT1:1790812801", "slack:CT1", .9, &author},
		{"slack:CT1:1790812802", "slack:CT1", .9, &other},
		{"slack:CT2:1790812803", "slack:CT2", .9, &author},
		{"slack:CT1:1790812804", "slack:CT1", .64, &author},
	} {
		seedRecentThread(t, deps, f.id, f.scope, strings.Repeat("candidate work ", 6), f.cosine, now, f.who, nil, false)
	}
	c1, c2 := "slack:CT1:1790812810", "slack:CT1:1790812809"
	seedRecentThread(t, deps, c1, "slack:CT1", strings.Repeat("via work one ", 6), .9, now, &author, nil, false)
	seedRecentThread(t, deps, c2, "slack:CT1", strings.Repeat("via work two ", 6), .9, now, &author, nil, false)
	for _, via := range []string{c1, c2} {
		seedTopicEdge(t, pool, a, via, `{}`)
		for _, b := range []string{"slack:CT1:1790812801", "slack:CT1:1790812802", "slack:CT2:1790812803", "slack:CT1:1790812804"} {
			seedTopicEdge(t, pool, via, b, `{}`)
		}
	}
	cands, err := topicMateCandidates(ctx, deps, a)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := candidateByID(cands, "slack:CT1:1790812801")
	if !ok || got.TopicVia != c2 {
		t.Fatalf("valid mate = %+v, present=%v, want via %s", got, ok, c2)
	}
	for _, id := range []string{"slack:CT1:1790812802", "slack:CT2:1790812803", "slack:CT1:1790812804"} {
		if _, ok := candidateByID(cands, id); ok {
			t.Errorf("ineligible mate %s was nominated", id)
		}
	}
}

func TestTopicMateCandidatesLimitsByCosineThenID(t *testing.T) {
	pool := caseTestDB(t)
	deps := Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "test"}
	ctx := context.Background()
	var author int64
	if err := pool.QueryRow(ctx, `INSERT INTO graph.people(display_name,machine_id) VALUES('limited topic author','test') RETURNING id`).Scan(&author); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Exec(ctx, `UPDATE graph.nodes SET author_person_id=NULL WHERE author_person_id=$1`, author)
		pool.Exec(ctx, `DELETE FROM graph.people WHERE id=$1`, author)
	})

	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	const source = "slack:CTL:1790812800"
	const via = "slack:CTL:1790812801"
	seedRecentThread(t, deps, source, "slack:CTL", strings.Repeat("source work ", 8), 1, now, &author, nil, false)
	seedRecentThread(t, deps, via, "slack:CTL", strings.Repeat("via work ", 8), .95, now, &author, nil, false)
	seedTopicEdge(t, pool, source, via, `{}`)

	mates := []struct {
		id     string
		cosine float64
	}{
		{"slack:CTL:1790812810", .99},
		{"slack:CTL:1790812811", .98},
		{"slack:CTL:1790812812", .97},
		{"slack:CTL:1790812813", .96},
		{"slack:CTL:1790812814", .95},
		{"slack:CTL:1790812815", .94},
		{"slack:CTL:1790812816", .93},
		{"slack:CTL:1790812817", .92},
		{"slack:CTL:1790812818", .91},
		{"slack:CTL:1790812819", .90},
		{"slack:CTL:1790812820", .90},
		{"slack:CTL:1790812821", .89},
	}
	for _, mate := range mates {
		seedRecentThread(t, deps, mate.id, "slack:CTL", strings.Repeat("candidate work ", 6), mate.cosine, now, &author, nil, false)
		seedTopicEdge(t, pool, via, mate.id, `{}`)
	}

	cands, err := topicMateCandidates(ctx, deps, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 10 {
		t.Fatalf("candidate count=%d, want 10", len(cands))
	}
	for i, want := range mates[:10] {
		if cands[i].NodeID != want.id {
			t.Errorf("candidate %d=%s, want %s", i, cands[i].NodeID, want.id)
		}
	}
}

func TestRecentMarketCandidatesActivityMarketsLimitsAndCaps(t *testing.T) {
	pool := caseTestDB(t)
	deps := Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "test"}
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	const sourceID = "slack:CT0:1790812800"
	seedRecentThread(t, deps, sourceID, "slack:CT0", strings.Repeat("India GST source ", 5), 1, now, nil, []string{"IN"}, true)
	source, err := loadTopicLinkSource(ctx, deps, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	seedRecentThread(t, deps, "slack:CT2:1790812800", "slack:CT2", strings.Repeat("India GST candidate ", 5), .8, now.Add(30*24*time.Hour), nil, []string{"IN"}, true)
	seedRecentThread(t, deps, "slack:CT3:1790812800", "slack:CT3", strings.Repeat("Thailand candidate ", 5), .9, now, nil, []string{"TH"}, true)
	seedRecentThread(t, deps, "slack:CT4:1788134399", "slack:CT4", strings.Repeat("India outside candidate ", 5), .81, now.Add(-30*24*time.Hour-time.Second), nil, []string{"IN"}, true)
	cands, err := recentMarketCandidates(ctx, deps, source, []string{"IN"}, now, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := candidateByID(cands, "slack:CT2:1790812800"); !ok {
		t.Fatal("inclusive hi boundary/cross-channel market candidate missing")
	}
	if _, ok := candidateByID(cands, "slack:CT3:1790812800"); ok {
		t.Fatal("wrong-market candidate kept")
	}
	if _, ok := candidateByID(cands, "slack:CT4:1788134399"); ok {
		t.Fatal("candidate one second outside window kept")
	}
	if got, err := recentMarketCandidates(ctx, deps, source, []string{"IN"}, time.Time{}, time.Time{}); err != nil || len(got) != 0 {
		t.Fatalf("unknown source window = %v, %v", got, err)
	}

	for i := range 13 {
		id := fmt.Sprintf("slack:CT5:%010d", 1790812900+i)
		seedRecentThread(t, deps, id, "slack:CT5", strings.Repeat("India matching work ", 4), .8, now, nil, []string{"IN"}, true)
	}
	cands, err = recentMarketCandidates(ctx, deps, source, []string{"IN"}, now, now)
	if err != nil || len(cands) != topicLinkRecentLimit {
		t.Fatalf("matching limit = %d, err=%v", len(cands), err)
	}
	for i := range 50 {
		id := fmt.Sprintf("slack:CT6:%010d", 1790813000+i)
		seedRecentThread(t, deps, id, "slack:CT6", strings.Repeat("Thailand higher cosine ", 4), .95-float64(i)/10000, now, nil, []string{"TH"}, true)
	}
	behind := "slack:CT7:1790813999"
	seedRecentThread(t, deps, behind, "slack:CT7", strings.Repeat("India behind cap ", 5), .79, now, nil, []string{"IN"}, true)
	cands, err = recentMarketCandidates(ctx, deps, source, []string{"IN"}, now, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := candidateByID(cands, behind); ok {
		t.Fatal("same-market candidate behind 50-row SQL cap was returned")
	}
}

func TestMarketsMigrationRoundTrip(t *testing.T) {
	pool := caseTestDB(t)
	path := filepath.Join("..", "..", "..", "migrations", "20261027095115_graph_thread_summaries_markets.sql")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(string(body), "-- +goose Down")
	if len(parts) != 2 {
		t.Fatalf("migration markers malformed")
	}
	up := strings.TrimPrefix(parts[0], "-- +goose Up")
	down := parts[1]
	ctx := context.Background()
	t.Cleanup(func() { pool.Exec(ctx, up) })
	if _, err := pool.Exec(ctx, down); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema='graph' AND table_name='thread_summaries' AND column_name='markets'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("column after Down: count=%d err=%v", count, err)
	}
	if _, err := pool.Exec(ctx, up); err != nil {
		t.Fatal(err)
	}
	var dataType, nullable string
	if err := pool.QueryRow(ctx, `SELECT data_type,is_nullable FROM information_schema.columns WHERE table_schema='graph' AND table_name='thread_summaries' AND column_name='markets'`).Scan(&dataType, &nullable); err != nil {
		t.Fatal(err)
	}
	if dataType != "ARRAY" || nullable != "YES" {
		t.Fatalf("column type=%s nullable=%s", dataType, nullable)
	}
}

type topicLifecycleJudge struct {
	GeminiClient
	mu      sync.Mutex
	abCalls int
}

func (j *topicLifecycleJudge) Generate(_ context.Context, _, user string) (string, error) {
	j.mu.Lock()
	if strings.Contains(user, "SOURCE-A-MARKER") && strings.Contains(user, "TARGET-B-MARKER") {
		j.abCalls++
	}
	j.mu.Unlock()
	return `{"tag":"bug_incident","same_topic":true,"confidence":0.9,"topic":"shared incident","why":"same exact incident","evidence_a":"Shared exact payment incident","evidence_b":"Shared exact payment incident"}`, nil
}

func (j *topicLifecycleJudge) GenerateCheap(context.Context, string, string) (string, error) {
	return "", fmt.Errorf("unexpected GenerateCheap")
}

func (j *topicLifecycleJudge) snapshot() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.abCalls
}

func TestTopicMateLifecycleKeepsAndDropsContext(t *testing.T) {
	pool := caseTestDB(t)
	ctx := context.Background()
	var author int64
	if err := pool.QueryRow(ctx, `INSERT INTO graph.people(display_name,machine_id) VALUES('lifecycle author','test') RETURNING id`).Scan(&author); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Exec(ctx, `UPDATE graph.nodes SET author_person_id=NULL WHERE author_person_id=$1`, author)
		pool.Exec(ctx, `DELETE FROM graph.people WHERE id=$1`, author)
	})
	deps := Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "test"}
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	a, b, c := "slack:CT8:1790812800", "slack:CT8:1790812801", "slack:CT8:1790812802"
	seedRecentThread(t, deps, a, "slack:CT8", "SOURCE-A-MARKER Shared exact payment incident "+strings.Repeat("source ", 5), 1, now, &author, nil, false)
	seedRecentThread(t, deps, b, "slack:CT8", "TARGET-B-MARKER Shared exact payment incident "+strings.Repeat("target ", 5), .8, now, &author, nil, false)
	seedRecentThread(t, deps, c, "slack:CT8", "SUPPORT-C-MARKER Shared exact payment incident "+strings.Repeat("support ", 5), .9, now, &author, nil, false)
	seedTopicEdge(t, pool, a, c, `{}`)
	seedTopicEdge(t, pool, c, b, `{}`)
	judge := &topicLifecycleJudge{}
	deps.Gemini = judge
	payload, _ := json.Marshal(linkTopicsPayload{NodeID: a})
	run := func() {
		if err := linkTopicsHandler(deps)(ctx, payload); err != nil {
			t.Fatal(err)
		}
	}
	run()
	if got := judge.snapshot(); got != 1 {
		t.Fatalf("run 1 A-B calls=%d, want 1", got)
	}
	if _, ok := edgeMeta(t, pool, a, b); !ok {
		t.Fatal("run 1 did not create A-B edge")
	}
	run()
	if got := judge.snapshot(); got != 1 {
		t.Fatalf("run 2 added A-B call; total=%d want 1", got)
	}
	if _, ok := edgeMeta(t, pool, a, b); !ok {
		t.Fatal("run 2 removed A-B edge")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM graph.edges WHERE kind='SAME_TOPIC' AND ((from_node_id=$1 AND to_node_id=$2) OR (from_node_id=$2 AND to_node_id=$1))`, c, b); err != nil {
		t.Fatal(err)
	}
	run()
	if got := judge.snapshot(); got != 2 {
		t.Fatalf("run 3 A-B calls=%d, want 2", got)
	}
}

func TestShortlistMissRecoveredByTopicAndMarketLanes(t *testing.T) {
	pool := caseTestDB(t)
	ctx := context.Background()
	var author int64
	if err := pool.QueryRow(ctx, `INSERT INTO graph.people(display_name,machine_id) VALUES('shortlist author','test') RETURNING id`).Scan(&author); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Exec(ctx, `UPDATE graph.nodes SET author_person_id=NULL WHERE author_person_id=$1`, author)
		pool.Exec(ctx, `DELETE FROM graph.people WHERE id=$1`, author)
	})
	deps := Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "test"}
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	a, b, c := "slack:CT9:1790812800", "slack:CT9:1790812801", "slack:CT9:1790812802"
	seedRecentThread(t, deps, a, "slack:CT9", strings.Repeat("India GST source ", 5), 1, now, &author, []string{"IN"}, true)
	seedRecentThread(t, deps, b, "slack:CT9", strings.Repeat("India GST target ", 5), .8, now, &author, []string{"IN"}, true)
	seedRecentThread(t, deps, c, "slack:CT9", strings.Repeat("India GST support ", 5), .82, now, &author, []string{"IN"}, true)
	seedTopicEdge(t, pool, a, c, `{}`)
	seedTopicEdge(t, pool, c, b, `{}`)
	for i := range 14 {
		id := fmt.Sprintf("slack:CTA:%010d", 1790814000+i)
		seedRecentThread(t, deps, id, "slack:CTA", strings.Repeat("Thailand distractor ", 5), .95-float64(i)/100, now, nil, []string{"TH"}, true)
	}
	shortlist, err := shortlistTopicLinks(ctx, deps, a)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := candidateByID(shortlist, b); ok {
		t.Fatal("B unexpectedly present in embedding shortlist")
	}
	mates, err := topicMateCandidates(ctx, deps, a)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := candidateByID(mates, b); !ok || got.TopicVia != c {
		t.Fatalf("topic-mate lane did not recover B: %+v present=%v", got, ok)
	}
	source, err := loadTopicLinkSource(ctx, deps, a)
	if err != nil {
		t.Fatal(err)
	}
	recent, err := recentMarketCandidates(ctx, deps, source, []string{"IN"}, now, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := candidateByID(recent, b); !ok {
		t.Fatal("recent-market lane did not recover B")
	}
}
