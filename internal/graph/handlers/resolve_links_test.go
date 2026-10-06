package handlers_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/handlers"
)

type linkFixture struct {
	t    *testing.T
	pool *pgxpool.Pool
}

func newLinkFixture(t *testing.T) *linkFixture {
	return &linkFixture{t: t, pool: testDB(t)}
}

func (f *linkFixture) node(id, typ string) {
	seedNode(f.t, f.pool, id, typ, id)
	seedBody(f.t, f.pool, id, "short fixture body")
}

func (f *linkFixture) edge(from, to, kind, meta string) {
	f.t.Helper()
	seedEdge(f.t, f.pool, from, to, kind)
	if meta == "" {
		return
	}
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE graph.edges SET metadata=$4::jsonb WHERE from_node_id=$1 AND to_node_id=$2 AND kind=$3`,
		from, to, kind, meta); err != nil {
		f.t.Fatal(err)
	}
}

// thread builds seed + reply in one Slack thread.
func (f *linkFixture) thread(seed, reply string) {
	f.node(seed, "slack")
	f.node(reply, "slack")
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE graph.nodes SET scope='slack:C9', metadata=jsonb_build_object('thread_ts','100.000001') WHERE id=ANY($1)`,
		[]string{seed, reply}); err != nil {
		f.t.Fatal(err)
	}
}

// resolve returns the raw artifact JSON objects by node id.
func (f *linkFixture) resolve(seeds []string, depth int) map[string]map[string]json.RawMessage {
	f.t.Helper()
	h, err := handlers.NewResolve(f.pool)
	if err != nil {
		f.t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"seeds": seeds, "depth": depth, "asker_eeid": 0, "include_bodies": false, "budget_tokens": 100000})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := httptest.NewRequest("POST", "/api/graph/resolve", strings.NewReader(string(body))).WithContext(ctx)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		f.t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Artifacts []map[string]json.RawMessage `json:"artifacts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		f.t.Fatal(err)
	}
	out := map[string]map[string]json.RawMessage{}
	for _, a := range resp.Artifacts {
		var id string
		_ = json.Unmarshal(a["node_id"], &id)
		out[id] = a
	}
	return out
}

func (f *linkFixture) links(arts map[string]map[string]json.RawMessage, id string) []map[string]any {
	f.t.Helper()
	a, ok := arts[id]
	if !ok {
		f.t.Fatalf("artifact %s missing", id)
	}
	raw, ok := a["links"]
	if !ok {
		return nil
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *linkFixture) wantLinks(arts map[string]map[string]json.RawMessage, id string, want []map[string]any) {
	f.t.Helper()
	got := f.links(arts, id)
	if !reflect.DeepEqual(got, want) {
		f.t.Fatalf("links[%s] = %v, want %v", id, got, want)
	}
}

func TestResolveLinks_References(t *testing.T) {
	f := newLinkFixture(t)
	s := "slack:SEED:1"
	f.node(s, "slack_thread")
	f.node("jira:A", "jira")
	f.edge(s, "jira:A", "REFERENCES", "")
	arts := f.resolve([]string{s}, 2)
	f.wantLinks(arts, "jira:A", []map[string]any{{"kind": "REFERENCES"}})
	if _, ok := arts[s]["links"]; ok {
		t.Fatal("seed has links key")
	}
}

func TestResolveLinks_SameTopicFields(t *testing.T) {
	f := newLinkFixture(t)
	s := "slack:SEED:1"
	f.node(s, "slack_thread")
	f.node("slack:C1:1", "slack_thread")
	f.edge(s, "slack:C1:1", "SAME_TOPIC", `{"confidence":0.91,"tag":"bug_incident","topic":"refund dup","why":"same mechanism"}`)
	arts := f.resolve([]string{s}, 2)
	f.wantLinks(arts, "slack:C1:1", []map[string]any{{"kind": "SAME_TOPIC", "tag": "bug_incident", "topic": "refund dup", "why": "same mechanism", "confidence": 0.91}})
}

func TestResolveLinks_TwoKindsOrdered(t *testing.T) {
	f := newLinkFixture(t)
	s := "slack:SEED:1"
	f.node(s, "slack_thread")
	f.node("jira:B", "jira")
	f.edge(s, "jira:B", "REFERENCES", "")
	f.edge("jira:B", s, "SAME_TOPIC", "")
	arts := f.resolve([]string{s}, 2)
	f.wantLinks(arts, "jira:B", []map[string]any{{"kind": "REFERENCES"}, {"kind": "SAME_TOPIC"}})
}

func TestResolveLinks_MultiSeedBestConfidence(t *testing.T) {
	f := newLinkFixture(t)
	s1, s2, o := "slack:SEED:1", "slack:SEED:2", "slack:C2:2"
	f.node(s1, "slack_thread")
	f.node(s2, "slack_thread")
	f.node(o, "slack_thread")
	f.edge(s1, o, "SAME_TOPIC", `{"confidence":0.70,"why":"w1"}`)
	f.edge(o, s2, "SAME_TOPIC", `{"confidence":0.90,"why":"w2"}`)
	arts := f.resolve([]string{s1, s2}, 2)
	f.wantLinks(arts, o, []map[string]any{{"kind": "SAME_TOPIC", "why": "w2", "confidence": 0.9}})
}

func TestResolveLinks_TieSmallestEdge(t *testing.T) {
	f := newLinkFixture(t)
	s1, s2, o := "slack:SEED:1", "slack:SEED:2", "slack:C2:2"
	f.node(s1, "slack_thread")
	f.node(s2, "slack_thread")
	f.node(o, "slack_thread")
	f.edge(s1, o, "SAME_TOPIC", `{"confidence":0.80,"why":"w1"}`)
	f.edge(o, s2, "SAME_TOPIC", `{"confidence":0.80,"why":"w2"}`)
	arts := f.resolve([]string{s1, s2}, 2)
	// (from,to) = ("slack:C2:2", ...) sorts before ("slack:SEED:1", ...): w2 wins.
	f.wantLinks(arts, o, []map[string]any{{"kind": "SAME_TOPIC", "why": "w2", "confidence": 0.8}})
}

func TestResolveLinks_Hop2HasNone(t *testing.T) {
	f := newLinkFixture(t)
	s := "slack:SEED:1"
	pr := "gh_pr:wego/x#1"
	f.node(s, "slack_thread")
	f.node("jira:C", "jira")
	f.node(pr, "gh_pr")
	f.edge(s, "jira:C", "REFERENCES", "")
	f.edge("jira:C", pr, "REFERENCES", "")
	arts := f.resolve([]string{s}, 2)
	if _, ok := arts[pr]; !ok {
		t.Fatal("PR missing")
	}
	if _, ok := arts[pr]["links"]; ok {
		t.Fatal("hop-2 node has links key")
	}
}

func TestResolveLinks_ThreadSibling(t *testing.T) {
	for _, depth := range []int{1, 2} {
		f := newLinkFixture(t)
		s, r := "slack:C9:100.000001", "slack:C9:100.000002"
		f.thread(s, r)
		arts := f.resolve([]string{s}, depth)
		f.wantLinks(arts, r, []map[string]any{{"kind": "THREAD"}})
	}
}

func TestResolveLinks_ThreadSiblingWithStoredTopic(t *testing.T) {
	for _, depth := range []int{1, 2} {
		f := newLinkFixture(t)
		s, r := "slack:C9:100.000001", "slack:C9:100.000002"
		f.thread(s, r)
		f.edge(s, r, "SAME_TOPIC", "")
		arts := f.resolve([]string{s}, depth)
		f.wantLinks(arts, r, []map[string]any{{"kind": "THREAD"}, {"kind": "SAME_TOPIC"}})
	}
}

func TestResolveLinks_ThreadSiblingWithStoredThread(t *testing.T) {
	for _, depth := range []int{1, 2} {
		f := newLinkFixture(t)
		s, r := "slack:C9:100.000001", "slack:C9:100.000002"
		f.thread(s, r)
		f.edge(s, r, "THREAD", "")
		arts := f.resolve([]string{s}, depth)
		f.wantLinks(arts, r, []map[string]any{{"kind": "THREAD"}})
	}
}
