package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func cascadeCount(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func cascadeRun(t *testing.T, pool *pgxpool.Pool, srv *httptest.Server, payload string) {
	t.Helper()
	h := NewFetchBodyHandler(jiraHandlerTestDeps(pool, srv))
	if err := h.Handler(context.Background(), []byte(payload)); err != nil {
		t.Fatalf("fetch_body %s: %v", payload, err)
	}
}

func cascadeSetup(t *testing.T) (*pgxpool.Pool, *httptest.Server) {
	t.Helper()
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	t.Cleanup(func() { truncateGraphHandlerTables(t, pool) })
	var mode atomic.Int32
	srv := jiraHandlerTestServer(t, &mode)
	t.Cleanup(srv.Close)
	return pool, srv
}

func TestFetchBodyCascade_DepthZeroEnqueuesDepthOne(t *testing.T) {
	pool, srv := cascadeSetup(t)
	cascadeRun(t, pool, srv, `{"node_id":"jira:TEST-1"}`)

	rows, err := pool.Query(context.Background(), `SELECT to_node_id FROM graph.edges WHERE from_node_id='jira:TEST-1' AND kind='REFERENCES'`)
	if err != nil {
		t.Fatal(err)
	}
	var targets []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		targets = append(targets, id)
	}
	rows.Close()
	if len(targets) != 4 {
		t.Fatalf("want 4 targets, got %v", targets)
	}
	for _, id := range targets {
		n := cascadeCount(t, pool, `SELECT count(*) FROM graph.jobs WHERE type='fetch_body' AND payload->>'node_id'=$1`, id)
		if n != 1 {
			t.Errorf("%s: want exactly 1 fetch_body job, got %d", id, n)
		}
		n = cascadeCount(t, pool, `SELECT count(*) FROM graph.jobs WHERE type='fetch_body' AND payload->>'node_id'=$1 AND payload->>'depth'='1' AND payload->>'via'='jira:TEST-1'`, id)
		if n != 1 {
			t.Errorf("%s: want depth=1 via=jira:TEST-1, got %d matching jobs", id, n)
		}
	}
}

func TestFetchBodyCascade_DepthOneEnqueuesNothing(t *testing.T) {
	pool, srv := cascadeSetup(t)
	cascadeRun(t, pool, srv, `{"node_id":"jira:TEST-1","depth":1}`)

	if n := cascadeCount(t, pool, `SELECT count(*) FROM graph.jobs WHERE type='fetch_body' AND payload->>'node_id' <> 'jira:TEST-1'`); n != 0 {
		t.Errorf("depth-1 fetch enqueued %d fetch_body jobs", n)
	}
	if n := cascadeCount(t, pool, `SELECT count(*) FROM graph.jobs WHERE type='index_artifact' AND payload->>'node_id'='jira:TEST-1'`); n != 1 {
		t.Errorf("want 1 index_artifact for jira:TEST-1, got %d", n)
	}
	assertJiraHandlerTargets(t, pool)
}

func TestFetchBodyCascade_DepthOneSkipsDescribeAttachment(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	t.Cleanup(func() { truncateGraphHandlerTables(t, pool) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/rest/api/3/issue/TEST-9":
			fmt.Fprint(w, `{"key":"TEST-9","fields":{"summary":"Attachment ticket","updated":"2026-09-30T10:00:00.000+0000","description":{"type":"doc","version":1,"content":[{"type":"paragraph","content":[{"type":"text","text":"No links"}]}]},"attachment":[{"id":"777","filename":"invoice.png","mimeType":"image/png","size":123,"content":"https://example.invalid/att/777"}]}}`)
		case "/rest/api/3/issue/TEST-9/comment":
			fmt.Fprint(w, `{"startAt":0,"maxResults":100,"total":0,"comments":[]}`)
		case "/rest/api/3/issue/TEST-9/remotelink":
			fmt.Fprint(w, `[]`)
		default:
			t.Errorf("unexpected Jira route: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	const descQ = `SELECT count(*) FROM graph.jobs WHERE type='describe_attachment'`
	const attQ = `SELECT count(*) FROM graph.edges WHERE from_node_id='jira:TEST-9' AND kind='REFERENCES' AND to_node_id IN (SELECT id FROM graph.nodes WHERE type='jira_attachment')`

	cascadeRun(t, pool, srv, `{"node_id":"jira:TEST-9"}`)
	if n := cascadeCount(t, pool, descQ); n != 1 {
		t.Fatalf("depth 0: want 1 describe_attachment, got %d", n)
	}
	if n := cascadeCount(t, pool, attQ); n != 1 {
		t.Fatalf("depth 0: want 1 attachment edge, got %d", n)
	}

	truncateGraphHandlerTables(t, pool)
	cascadeRun(t, pool, srv, `{"node_id":"jira:TEST-9","depth":1}`)
	if n := cascadeCount(t, pool, descQ); n != 0 {
		t.Errorf("depth 1: want 0 describe_attachment, got %d", n)
	}
	if n := cascadeCount(t, pool, attQ); n != 1 {
		t.Errorf("depth 1: attachment node/edge must still exist, got %d edges", n)
	}
}

func TestFetchBodyCascade_NoDuplicateWhileQueued(t *testing.T) {
	pool, srv := cascadeSetup(t)
	const countQ = `SELECT count(*) FROM graph.jobs WHERE type='fetch_body' AND payload->>'node_id'='jira:PAY-12'`

	// The target node must exist for the handler to consider it; the handler
	// creates it as an edge stub, so pre-insert the job and let the run do the rest.
	insert := func(availableAt string) {
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO graph.jobs (type, payload, priority, machine_id, available_at)
			VALUES ('fetch_body', '{"node_id":"jira:PAY-12"}'::jsonb, 5, 'test', $1::timestamptz)`, availableAt); err != nil {
			t.Fatal(err)
		}
	}

	insert("now")
	cascadeRun(t, pool, srv, `{"node_id":"jira:TEST-1"}`)
	if n := cascadeCount(t, pool, countQ); n != 1 {
		t.Fatalf("live queued job: want exactly 1 for jira:PAY-12, got %d", n)
	}

	truncateGraphHandlerTables(t, pool)
	insert("2099-01-01")
	cascadeRun(t, pool, srv, `{"node_id":"jira:TEST-1"}`)
	if n := cascadeCount(t, pool, countQ); n != 2 {
		t.Fatalf("parked job: want 2 for jira:PAY-12, got %d", n)
	}
}
