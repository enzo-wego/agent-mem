package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/extractor"
	"github.com/agent-mem/agent-mem/internal/graph/fetchers"
	"github.com/agent-mem/agent-mem/internal/graph/identity"
	"github.com/agent-mem/agent-mem/internal/graph/jobs"
	"github.com/agent-mem/agent-mem/internal/graph/normalizer"
)

func TestFetchBodyHandler_BadPayload(t *testing.T) {
	deps := Deps{
		Logger:    zerolog.Nop(),
		MachineID: "test",
		Fetchers:  fetchers.NewRegistry(fetchers.Config{}, zerolog.Nop()),
	}
	h := NewFetchBodyHandler(deps)

	err := h.Handler(context.Background(), []byte("not json"))
	if err == nil {
		t.Fatal("expected error for bad JSON payload")
	}
	if !errors.Is(err, errFatalSentinel) {
		t.Logf("error (expected ErrFatal-wrapped): %v", err)
	}
}

func TestFetchBodyHandler_EmptyRef(t *testing.T) {
	deps := Deps{
		Logger:    zerolog.Nop(),
		MachineID: "test",
		Fetchers:  fetchers.NewRegistry(fetchers.Config{}, zerolog.Nop()),
	}
	h := NewFetchBodyHandler(deps)

	payload, _ := json.Marshal(fetchBodyPayload{})
	err := h.Handler(context.Background(), payload)
	if err == nil {
		t.Fatal("expected error when both node_id and url are empty")
	}
}

func TestFetchBodyHandler_NoFetcher(t *testing.T) {
	deps := Deps{
		Logger:    zerolog.Nop(),
		MachineID: "test",
		Fetchers:  fetchers.NewRegistry(fetchers.Config{}, zerolog.Nop()),
	}
	h := NewFetchBodyHandler(deps)

	// "unknown:xyz" won't match any registered fetcher.
	payload, _ := json.Marshal(fetchBodyPayload{NodeID: "unknown:xyz"})
	err := h.Handler(context.Background(), payload)
	if err == nil {
		t.Fatal("expected error when no fetcher matches")
	}
}

// errFatalSentinel is exported only for test assertions within the package.
var errFatalSentinel = errors.New("fatal")

func TestFetchBodyJira_CommentAndRemoteLinksBecomeEdges(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	// PAY is a fetched project on prod; bare/linked PAY keys must stay linked.
	if _, err := pool.Exec(context.Background(), `
INSERT INTO graph.nodes (id, type, natural_key, body, machine_id)
VALUES ('jira:PAY-1', 'jira', 'PAY-1', 'ticket', 'test')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { truncateGraphHandlerTables(t, pool) })
	var mode atomic.Int32
	server := jiraHandlerTestServer(t, &mode)
	defer server.Close()
	h := NewFetchBodyHandler(jiraHandlerTestDeps(pool, server))
	if err := h.Handler(context.Background(), []byte(`{"node_id":"jira:TEST-1"}`)); err != nil {
		t.Fatalf("fetch_body: %v", err)
	}
	assertJiraHandlerTargets(t, pool)
}

func TestFetchBodyJira_FailureKeepsBodyAndEdges(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	// PAY is a fetched project on prod; bare/linked PAY keys must stay linked.
	if _, err := pool.Exec(context.Background(), `
INSERT INTO graph.nodes (id, type, natural_key, body, machine_id)
VALUES ('jira:PAY-1', 'jira', 'PAY-1', 'ticket', 'test')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { truncateGraphHandlerTables(t, pool) })
	var mode atomic.Int32
	server := jiraHandlerTestServer(t, &mode)
	defer server.Close()
	h := NewFetchBodyHandler(jiraHandlerTestDeps(pool, server))
	payload := []byte(`{"node_id":"jira:TEST-1"}`)
	if err := h.Handler(context.Background(), payload); err != nil {
		t.Fatalf("initial fetch: %v", err)
	}
	assertJiraHandlerTargets(t, pool)
	before := jiraHandlerSnapshot(t, pool)
	for i, name := range []string{"remote_404", "remote_500", "comment_empty_page"} {
		t.Run(name, func(t *testing.T) {
			mode.Store(int32(i + 1))
			err := h.Handler(context.Background(), payload)
			if err == nil {
				t.Fatal("expected fetch failure")
			}
			// A failing sub-call (comments/remote links) is retryable, never
			// permanent: only a 404/410 on the issue itself is.
			var permanent *fetchers.PermanentError
			if errors.As(err, &permanent) || errors.Is(err, jobs.ErrFatal) || !errors.Is(err, jobs.ErrTransient) {
				t.Fatalf("sub-call failure must be transient, not permanent/fatal: %v", err)
			}
			if after := jiraHandlerSnapshot(t, pool); after != before {
				t.Fatalf("failed fetch changed persisted body, body_ts or edges\nbefore: %s\nafter: %s", before, after)
			}
		})
	}
	mode.Store(0)
	if err := h.Handler(context.Background(), payload); err != nil {
		t.Fatalf("recovery fetch: %v", err)
	}
	assertJiraHandlerTargets(t, pool)
}

func jiraHandlerTestDeps(pool *pgxpool.Pool, server *httptest.Server) Deps {
	log := zerolog.Nop()
	return Deps{
		DB: pool, Logger: log, MachineID: "test",
		Fetchers: fetchers.NewRegistry(fetchers.Config{
			JiraEmail: "test@example.com", JiraToken: "test-token",
			JiraBaseURL: server.URL, HTTPClient: server.Client(),
		}, log),
		Normalizers: normalizer.NewDefault(nil),
		Extractor:   extractor.New(pool, log),
		Identity:    identity.NewService(pool, log),
	}
}

func jiraHandlerTestServer(t *testing.T, mode *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "test@example.com" || password != "test-token" {
			t.Errorf("missing Jira Basic auth on %s", r.URL.Path)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/rest/api/3/issue/TEST-1":
			updated := "2026-09-30T10:00:00.000+0000"
			if mode.Load() != 0 {
				updated = "2026-09-30T11:00:00.000+0000"
			}
			fmt.Fprintf(w, `{"key":"TEST-1","fields":{"summary":"Test ticket","updated":%q,"description":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"Description without links"}]}]},"issuelinks":[{"outwardIssue":{"key":"PAY-12"}}]}}`, updated)
		case "/rest/api/3/issue/TEST-1/comment":
			if mode.Load() == 3 {
				if r.URL.Query().Get("startAt") == "0" {
					fmt.Fprint(w, `{"startAt":0,"maxResults":100,"total":2,"comments":[{"id":"1","created":"2026-09-30T09:00:00.000+0000","body":{"type":"doc","content":[]}}]}`)
				} else {
					fmt.Fprint(w, `{"startAt":1,"maxResults":100,"total":2,"comments":[]}`)
				}
				return
			}
			fmt.Fprint(w, `{"startAt":0,"maxResults":100,"total":2,"comments":[{"id":"1","author":{"displayName":"First"},"created":"2026-09-30T09:00:00.000+0000","body":{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"Comment link","marks":[{"type":"link","attrs":{"href":"https://wego.slack.com/archives/CCOMMENT/p1787303769925489"}}]}]}]}},{"id":"2","author":{"displayName":"Second"},"created":"2026-09-30T09:01:00.000+0000","body":{"type":"doc","content":[{"type":"blockCard","attrs":{"url":"https://github.com/wego/payments/pull/123"}}]}}]}`)
		case "/rest/api/3/issue/TEST-1/remotelink":
			switch mode.Load() {
			case 1:
				http.Error(w, "remote link missing", http.StatusNotFound)
			case 2:
				http.Error(w, "remote link unavailable", http.StatusInternalServerError)
			default:
				fmt.Fprint(w, `[{"object":{"title":"Remote thread","url":"https://wego.slack.com/archives/CREMOTE/p1790668180910269"}}]`)
			}
		default:
			t.Errorf("unexpected Jira route: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
}

func assertJiraHandlerTargets(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT to_node_id FROM graph.edges WHERE from_node_id='jira:TEST-1' AND kind='REFERENCES' ORDER BY to_node_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"gh_pr:wego/payments#123", "jira:PAY-12", "slack:CCOMMENT:1787303769.925489", "slack:CREMOTE:1790668180.910269"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("REFERENCES targets = %v, want %v", got, want)
	}
}

func jiraHandlerSnapshot(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var snapshot string
	err := pool.QueryRow(context.Background(), `
SELECT jsonb_build_object(
    'body', n.body, 'body_ts', n.body_ts,
    'body_full', (SELECT body_full FROM graph.artifact_bodies WHERE node_id=n.id),
    'edges', (SELECT COALESCE(jsonb_agg(to_jsonb(e) ORDER BY e.id), '[]'::jsonb)
              FROM graph.edges e WHERE e.from_node_id=n.id OR e.to_node_id=n.id)
)::text FROM graph.nodes n WHERE n.id='jira:TEST-1'`).Scan(&snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// The cluster-summary path shares this predicate with the popup BFS.
func TestClusterSummary_UsesIncomingOnly(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	t.Cleanup(func() { truncateGraphHandlerTables(t, pool) })
	ctx := context.Background()
	for _, node := range []struct{ id, typ string }{
		{"jira:TEST-1", "jira"}, {"slack:CROOT:1.000001", "slack"},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO graph.nodes (id,type,natural_key,machine_id) VALUES ($1,$2,$1,'test')`, node.id, node.typ); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO graph.edges (from_node_id,to_node_id,kind,machine_id) VALUES ('slack:CROOT:1.000001','jira:TEST-1','REFERENCES','test')`); err != nil {
		t.Fatal(err)
	}
	for i := range 13 {
		id := fmt.Sprintf("jira:OUT-%d", i)
		if _, err := pool.Exec(ctx, `INSERT INTO graph.nodes (id,type,natural_key,machine_id) VALUES ($1,'jira',$1,'test')`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO graph.edges (from_node_id,to_node_id,kind,machine_id) VALUES ('jira:TEST-1',$1,'REFERENCES','test')`, id); err != nil {
			t.Fatal(err)
		}
	}
	if !expandableThrough(ctx, pool, "jira:TEST-1") {
		t.Fatal("cluster traversal must expand a Jira resource with 13 outgoing and 1 incoming REFERENCES")
	}
}
