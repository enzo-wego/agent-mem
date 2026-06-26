package bfs_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/bfs"
)

func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("DB ping: %v", err)
	}
	t.Cleanup(func() {
		cleanupBFSTables(t, pool)
		pool.Close()
	})
	cleanupBFSTables(t, pool)
	return pool
}

func cleanupBFSTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, tbl := range []string{"graph.edges", "graph.nodes"} {
		if _, err := pool.Exec(ctx, "DELETE FROM "+tbl); err != nil {
			t.Logf("cleanup %s: %v", tbl, err)
		}
	}
}

func seedNode(t *testing.T, pool *pgxpool.Pool, id, typ, title string) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
INSERT INTO graph.nodes (id, type, natural_key, title, machine_id)
VALUES ($1, $2, $1, $3, 'test')
ON CONFLICT (id) DO NOTHING`, id, typ, title)
	if err != nil {
		t.Fatalf("seedNode %s: %v", id, err)
	}
}

func seedEdge(t *testing.T, pool *pgxpool.Pool, from, to, kind string) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
INSERT INTO graph.edges (from_node_id, to_node_id, kind, machine_id)
VALUES ($1, $2, $3, 'test')
ON CONFLICT (from_node_id, to_node_id, kind) DO NOTHING`, from, to, kind)
	if err != nil {
		t.Fatalf("seedEdge %s->%s: %v", from, to, err)
	}
}

func seedSlackMsg(t *testing.T, pool *pgxpool.Pool, id, scope, threadTs string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO graph.nodes (id, type, natural_key, scope, metadata, machine_id)
VALUES ($1, 'slack', $1, $2, jsonb_build_object('thread_ts', $3), 'test')
ON CONFLICT (id) DO UPDATE SET scope=excluded.scope, metadata=excluded.metadata`,
		id, scope, threadTs)
	if err != nil {
		t.Fatalf("seedSlackMsg %s: %v", id, err)
	}
}

// A reply's resource (jira) must be reachable from the thread root even though no
// edge connects root↔reply — thread siblings bridge it.
func TestExpand_ThreadSiblingsBridgeToReplyResources(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	seedSlackMsg(t, pool, "slack:C:100", "slack:C", "")    // root (thread key = own ts 100)
	seedSlackMsg(t, pool, "slack:C:200", "slack:C", "100") // reply in thread 100
	seedNode(t, pool, "jira:PAY-1", "jira", "PAY-1")
	seedEdge(t, pool, "slack:C:200", "jira:PAY-1", "REFERENCES")

	e := bfs.NewExpander(pool)

	// Root expands to the reply via a synthetic THREAD link.
	rootNbrs, err := e.Expand(ctx, "slack:C:100", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasNeighbor(rootNbrs, "slack:C:200") {
		t.Errorf("root did not reach reply via thread: %v", rootNbrs)
	}

	// Reply expands to both the root (THREAD) and the jira (REFERENCES).
	replyNbrs, err := e.Expand(ctx, "slack:C:200", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasNeighbor(replyNbrs, "slack:C:100") || !hasNeighbor(replyNbrs, "jira:PAY-1") {
		t.Errorf("reply missing root or jira: %v", replyNbrs)
	}
}

func hasNeighbor(ns []bfs.Neighbor, id string) bool {
	for _, n := range ns {
		if n.NodeID == id {
			return true
		}
	}
	return false
}

func TestExpand_ReturnsNeighborsInBothDirections(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	seedNode(t, pool, "slack:C:1", "slack_thread", "thread A")
	seedNode(t, pool, "jira:PAY-1", "jira", "PAY-1")
	seedNode(t, pool, "gh_pr:wego/x#10", "gh_pr", "PR 10")
	seedEdge(t, pool, "slack:C:1", "jira:PAY-1", "REFERENCES")
	seedEdge(t, pool, "gh_pr:wego/x#10", "jira:PAY-1", "REFERENCES")

	e := bfs.NewExpander(pool)
	neighbours, err := e.Expand(ctx, "jira:PAY-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"slack:C:1": true, "gh_pr:wego/x#10": true}
	for id := range want {
		found := false
		for _, n := range neighbours {
			if n.NodeID == id {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing %q in %v", id, neighbours)
		}
	}
}

func TestExpand_FiltersByEdgeKind(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	seedNode(t, pool, "a", "slack_thread", "a")
	seedNode(t, pool, "b", "jira", "b")
	seedNode(t, pool, "c", "gh_pr", "c")
	seedEdge(t, pool, "a", "b", "REFERENCES")
	seedEdge(t, pool, "a", "c", "MENTIONS")

	e := bfs.NewExpander(pool)
	neighbours, _ := e.Expand(ctx, "a", []string{"REFERENCES"})
	if len(neighbours) != 1 || neighbours[0].NodeID != "b" {
		t.Errorf("kind filter failed; got %v", neighbours)
	}
}
