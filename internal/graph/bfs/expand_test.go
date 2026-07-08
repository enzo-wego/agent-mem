package bfs_test

import (
	"context"
	"os"
	"strings"
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
	for _, tbl := range []string{"graph.artifact_index", "graph.edges", "graph.nodes"} {
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

// unitVec returns a 3072-dim halfvec (matching artifact_index HALFVEC(3072)) that is 1
// at index hot and 0 elsewhere — so two such vectors are identical (cosine 1) when
// hot matches and orthogonal (cosine 0) when it differs.
func unitVec(hot int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := range 3072 {
		if i > 0 {
			b.WriteByte(',')
		}
		if i == hot {
			b.WriteByte('1')
		} else {
			b.WriteByte('0')
		}
	}
	b.WriteByte(']')
	return b.String()
}

func seedEmbedding(t *testing.T, pool *pgxpool.Pool, id, vec string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO graph.artifact_index (node_id, embedding, machine_id)
VALUES ($1, $2::halfvec, 'test')
ON CONFLICT (node_id) DO UPDATE SET embedding = excluded.embedding`, id, vec)
	if err != nil {
		t.Fatalf("seedEmbedding %s: %v", id, err)
	}
}

// Topically-close thread roots in different channels link via SIMILAR even with no
// shared edge; orthogonal topics, replies, same thread, and DMs do not.
func TestExpand_SimilarThreadsBridgesByTopic(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	seedSlackMsg(t, pool, "slack:C:100", "slack:C", "")    // opened root, topic A
	seedSlackMsg(t, pool, "slack:G:300", "slack:G", "")    // other-channel root, topic A
	seedSlackMsg(t, pool, "slack:H:400", "slack:H", "")    // root, unrelated topic B
	seedSlackMsg(t, pool, "slack:C:200", "slack:C", "100") // reply in opened thread, topic A
	seedSlackMsg(t, pool, "slack:D9:500", "slack:D9", "")  // DM root, topic A
	seedEmbedding(t, pool, "slack:C:100", unitVec(0))
	seedEmbedding(t, pool, "slack:G:300", unitVec(0))
	seedEmbedding(t, pool, "slack:H:400", unitVec(1))
	seedEmbedding(t, pool, "slack:C:200", unitVec(0))
	seedEmbedding(t, pool, "slack:D9:500", unitVec(0))

	sim, err := bfs.NewExpander(pool).SimilarThreads(ctx, "slack:C:100")
	if err != nil {
		t.Fatal(err)
	}
	if !hasNeighbor(sim, "slack:G:300") {
		t.Errorf("expected cross-channel topical root slack:G:300 in %v", sim)
	}
	for _, bad := range []string{"slack:C:100", "slack:C:200", "slack:H:400", "slack:D9:500"} {
		if hasNeighbor(sim, bad) {
			t.Errorf("did not expect %s (self/reply/unrelated/DM) in %v", bad, sim)
		}
	}
	for _, n := range sim {
		if n.EdgeKind != "SIMILAR" {
			t.Errorf("edge kind = %q, want SIMILAR", n.EdgeKind)
		}
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
