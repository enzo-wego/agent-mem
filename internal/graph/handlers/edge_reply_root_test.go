package handlers

import (
	"context"
	"testing"

	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/extractor"
)

// TestEdgePersist_ReplyRoot checks that a reply permalink persists edges to
// both the reply and the thread root, and that removing the link prunes both.
func TestEdgePersist_ReplyRoot(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	t.Cleanup(func() { truncateGraphHandlerTables(t, pool) })
	ctx := context.Background()
	deps := Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "test", Extractor: extractor.New(nil, zerolog.Nop())}

	const from = "jira:PAY-2400"
	const reply = "slack:C0BBJAHV4G1:1790581016.177759"
	const root = "slack:C0BBJAHV4G1:1789701873.754609"
	if _, err := pool.Exec(ctx, `INSERT INTO graph.nodes (id, type, natural_key, body, updated_at, machine_id)
		VALUES ($1, 'jira', 'PAY-2400', 'x', NOW(), 'test')`, from); err != nil {
		t.Fatalf("seed node: %v", err)
	}

	persist := func(body string) {
		t.Helper()
		res, err := deps.Extractor.Extract(ctx, body)
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		keep, err := reconcileEdges(ctx, deps, from, res.Findings)
		if err != nil {
			t.Fatalf("reconcileEdges: %v", err)
		}
		if err := pruneStaleEdges(ctx, deps, from, keep); err != nil {
			t.Fatalf("pruneStaleEdges: %v", err)
		}
	}
	hasEdge := func(to string) bool {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph.edges WHERE from_node_id=$1 AND to_node_id=$2 AND kind='REFERENCES'`, from, to).Scan(&n); err != nil {
			t.Fatalf("count edges: %v", err)
		}
		return n == 1
	}

	persist("link (https://wego.slack.com/archives/C0BBJAHV4G1/p1790581016177759?thread_ts=1789701873.754609&cid=C0BBJAHV4G1)")
	if !hasEdge(reply) || !hasEdge(root) {
		t.Fatalf("after link: reply=%v root=%v, want both", hasEdge(reply), hasEdge(root))
	}

	persist("no links any more")
	if hasEdge(reply) || hasEdge(root) {
		t.Fatalf("after removal: reply=%v root=%v, want neither", hasEdge(reply), hasEdge(root))
	}
}
