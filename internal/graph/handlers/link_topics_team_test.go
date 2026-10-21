package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

func TestSkipTopicLinkSourceTeam(t *testing.T) {
	src := topicLinkNode{NodeID: "team:t", Type: "team", Summary: strings.Repeat("directory entry ", 10)}
	if !skipTopicLinkSource(src) {
		t.Fatal("team source must never topic-link")
	}
}

func TestDropTeamCandidates(t *testing.T) {
	mk := func(id, typ string) topicLinkCandidate {
		return topicLinkCandidate{topicLinkNode: topicLinkNode{NodeID: id, Type: typ}}
	}
	got := dropTeamCandidates([]topicLinkCandidate{mk("jira:A-1", "jira"), mk("team:t", "team"), mk("slack:C:1", "slack")})
	if len(got) != 2 || got[0].Type != "jira" || got[1].Type != "slack" {
		t.Fatalf("got %+v", got)
	}
}

// recordingJudge answers every judging call negatively and records the prompts.
type recordingJudge struct {
	GeminiClient
	mu    sync.Mutex
	users []string
}

func (f *recordingJudge) Generate(_ context.Context, _, user string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users = append(f.users, user)
	return `{"same_topic":false,"confidence":0.9,"topic":"x","why":"unrelated"}`, nil
}

func (f *recordingJudge) GenerateCheap(context.Context, string, string) (string, error) {
	return "", errors.New("recordingJudge: unexpected GenerateCheap")
}

func (f *recordingJudge) prompts() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.users, "\n----\n")
}

const teamSummarySentinel = "TEAMSUMMARYSENTINEL directory entry for the payments routing team."

// topicTestRun returns a per-run suffix. The judgment cache is keyed on
// content, so ids and summaries must be unique per run for the judge to be
// asked again; no cleanup is then needed.
func topicTestRun() string { return strconv.FormatInt(time.Now().UnixNano(), 36) }

func seedTopicThread(t *testing.T, pool *pgxpool.Pool, id, run string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
INSERT INTO graph.nodes (id, type, natural_key, title, scope, machine_id)
VALUES ($1, 'slack_thread', $1, $1, 'slack:' || split_part($1,':',2), 'test')`, id); err != nil {
		t.Fatalf("seed thread: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO graph.artifact_index (node_id, summary, summary_kind, identifiers, refreshed_at, machine_id)
VALUES ($1, $2, 'thread_summary', '{}', NOW(), 'test')`, id, "Thread "+id+" "+run+": a summary long enough to clear the forty character floor."); err != nil {
		t.Fatalf("seed thread index: %v", err)
	}
}

func seedTeamForTopicTest(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
INSERT INTO graph.nodes (id, type, natural_key, title, scope, machine_id)
VALUES ($1, 'team', $1, 'Team', 'public', 'test') ON CONFLICT (id) DO NOTHING`, id); err != nil {
		t.Fatalf("seed team: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO graph.artifact_index (node_id, summary, summary_kind, identifiers, refreshed_at, machine_id)
VALUES ($1, $2, 'heuristic', '{}', NOW(), 'test') ON CONFLICT (node_id) DO NOTHING`, id, teamSummarySentinel); err != nil {
		t.Fatalf("seed team index: %v", err)
	}
}

func edgeMeta(t *testing.T, pool *pgxpool.Pool, a, b string) (string, bool) {
	t.Helper()
	var meta string
	err := pool.QueryRow(context.Background(), `
SELECT metadata::text FROM graph.edges WHERE kind='SAME_TOPIC'
  AND ((from_node_id=$1 AND to_node_id=$2) OR (from_node_id=$2 AND to_node_id=$1))`, a, b).Scan(&meta)
	if err != nil {
		return "", false
	}
	return meta, true
}

func runLinkTopics(t *testing.T, pool *pgxpool.Pool, judge *recordingJudge, node string) {
	t.Helper()
	deps := Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "test", Gemini: judge}
	payload, _ := json.Marshal(linkTopicsPayload{NodeID: node, Force: true})
	if err := linkTopicsHandler(deps)(context.Background(), payload); err != nil {
		t.Fatalf("link_topics: %v", err)
	}
}

func TestLinkTopics_TeamExistingEdgeUnchanged(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	run := topicTestRun()

	s, team := "slack:CT1:"+run, "team:tt-direct"
	seedTopicThread(t, pool, s, run)
	seedTeamForTopicTest(t, pool, team)
	seedTopicEdge(t, pool, s, team, `{"method":"cosine-shortlist + llm-confirm","confidence":0.9}`)
	before, ok := edgeMeta(t, pool, s, team)
	if !ok {
		t.Fatal("seed edge missing")
	}

	judge := &recordingJudge{}
	runLinkTopics(t, pool, judge, s)

	if strings.Contains(judge.prompts(), "TEAMSUMMARYSENTINEL") {
		t.Fatalf("judge was asked about the team node: %s", judge.prompts())
	}
	after, ok := edgeMeta(t, pool, s, team)
	if !ok || after != before {
		t.Fatalf("S-team edge changed: before=%s after=%s present=%v", before, after, ok)
	}
}

func TestLinkTopics_TeamNeverTwoHopCaseMate(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	run := topicTestRun()

	s, p, team := "slack:CT1:"+run, "slack:CT2:"+run, "team:tt-hop"
	seedTopicThread(t, pool, s, run)
	seedTopicThread(t, pool, p, run)
	seedTeamForTopicTest(t, pool, team)
	seedTopicEdge(t, pool, s, p, `{"method":"cosine-shortlist + llm-confirm","confidence":0.9}`)
	seedTopicEdge(t, pool, p, team, `{"method":"shared-identifier + llm-confirm","confidence":1,"shared_ids":["p0yy6hmqdw"]}`)
	before, _ := edgeMeta(t, pool, p, team)

	judge := &recordingJudge{}
	runLinkTopics(t, pool, judge, s)

	if judge.prompts() == "" {
		t.Fatal("judge was never called; the test would be vacuous")
	}
	if strings.Contains(judge.prompts(), "TEAMSUMMARYSENTINEL") {
		t.Fatalf("judge was asked about the team node: %s", judge.prompts())
	}
	if _, ok := edgeMeta(t, pool, s, team); ok {
		t.Fatal("S-team edge was created")
	}
	if after, ok := edgeMeta(t, pool, p, team); !ok || after != before {
		t.Fatalf("P-team edge changed: before=%s after=%s present=%v", before, after, ok)
	}
}
