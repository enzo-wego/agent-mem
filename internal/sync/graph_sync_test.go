package sync

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"github.com/agent-mem/agent-mem/internal/database"
)

// openTestPool connects to the DATABASE_URL Postgres instance.
// Skips if DATABASE_URL is not set.
func openTestPool(t *testing.T) *pgxpool.Pool {
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
	t.Cleanup(pool.Close)
	return pool
}

// truncateGraphSyncTables removes test data created by this test.
func truncateGraphSyncTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	tables := []string{
		"graph.artifact_bodies",
		"graph.artifact_index",
		"graph.edges",
		"graph.jobs",
		"graph.nodes",
		"graph.identity_map",
		"graph.people",
		"graph.slack_groups",
		"graph.entities",
		"graph.user_affinity_config",
	}
	for _, tbl := range tables {
		if _, err := pool.Exec(ctx, "DELETE FROM "+tbl); err != nil {
			t.Fatalf("truncate %s: %v", tbl, err)
		}
	}
}

// TestGraphSync_PushPull verifies that a graph.nodes row inserted on machine A
// appears after a push/pull cycle on machine B (simulated via two DB wrappers
// pointing at the same Postgres instance with different machine IDs).
func TestGraphSync_PushPull(t *testing.T) {
	pool := openTestPool(t)
	truncateGraphSyncTables(t, pool)

	ctx := context.Background()
	dbA := database.NewDB(pool)
	dbB := database.NewDB(pool)

	const machineA = "test-machine-a"
	const machineB = "test-machine-b"

	// Insert a graph.nodes row as machine A (sync_version = 0 → unsynced).
	nodeID := "jira:TST-1001"
	bodyTS := time.Now().UTC().Truncate(time.Second)
	_, err := pool.Exec(ctx, `
		INSERT INTO graph.nodes
			(id, type, natural_key, body, body_revision, body_ts, updated_at, machine_id)
		VALUES ($1, 'jira', 'TST-1001', 'test body', 1, $2, NOW(), $3)
		ON CONFLICT (id) DO NOTHING`,
		nodeID, bodyTS, machineA,
	)
	if err != nil {
		t.Fatalf("insert node as machine A: %v", err)
	}

	// Collect unsynced nodes from machine A.
	unsynced, err := dbA.GetUnsyncedGraphNodes(ctx, 100)
	if err != nil {
		t.Fatalf("GetUnsyncedGraphNodes: %v", err)
	}
	if len(unsynced) == 0 {
		t.Fatal("expected at least 1 unsynced node from machine A")
	}
	found := false
	for _, n := range unsynced {
		if n.ID == nodeID {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected node %q in unsynced list", nodeID)
	}

	// Simulate push: machine B imports the node (server side receives push payload).
	for i := range unsynced {
		if unsynced[i].ID != nodeID {
			continue
		}
		if err := dbB.ImportGraphNode(ctx, &unsynced[i]); err != nil {
			t.Fatalf("ImportGraphNode: %v", err)
		}
	}

	// Mark synced on machine A side (as if server acknowledged).
	syncIDs := make([]string, 0, len(unsynced))
	for _, n := range unsynced {
		syncIDs = append(syncIDs, n.SyncID)
	}
	if err := dbA.MarkSyncedGraphBySyncID(ctx, "graph", "nodes", syncIDs, int64(time.Now().Unix())); err != nil {
		t.Fatalf("MarkSyncedGraphBySyncID: %v", err)
	}

	// Verify: machine B (excludeSource=machineB) can pull the row authored by machine A.
	pulled, err := dbB.GetGraphNodesForPull(ctx, machineB, 0, 100)
	if err != nil {
		t.Fatalf("GetGraphNodesForPull: %v", err)
	}
	pulledFound := false
	for _, n := range pulled {
		if n.ID == nodeID && n.MachineID == machineA {
			pulledFound = true
		}
	}
	if !pulledFound {
		t.Errorf("expected node %q from machine A in pull results for machine B", nodeID)
	}

	// Verify that after MarkSynced, the node no longer appears in unsynced list.
	unsyncedAfter, err := dbA.GetUnsyncedGraphNodes(ctx, 100)
	if err != nil {
		t.Fatalf("GetUnsyncedGraphNodes after mark: %v", err)
	}
	for _, n := range unsyncedAfter {
		if n.ID == nodeID {
			t.Errorf("node %q should not appear in unsynced list after MarkSynced", nodeID)
		}
	}
}

// TestGraphSync_ArtifactIndexEmbedding verifies embeddings survive the
// pull -> import round trip (they are stored as halfvec(3072)).
func TestGraphSync_ArtifactIndexEmbedding(t *testing.T) {
	pool := openTestPool(t)
	truncateGraphSyncTables(t, pool)

	ctx := context.Background()
	dbA := database.NewDB(pool)

	// artifact_index.node_id references graph.nodes - create the node first.
	_, err := pool.Exec(ctx, `
		INSERT INTO graph.nodes
			(id, type, natural_key, body, body_revision, body_ts, updated_at, machine_id)
		VALUES ('slack:TST-EMB', 'slack_thread', 'TST-EMB', 'b', 1, NOW(), NOW(), 'cloud-test')`)
	if err != nil {
		t.Fatalf("insert node: %v", err)
	}

	emb := make([]float32, 3072)
	emb[0], emb[1] = 0.5, -0.25
	_, err = pool.Exec(ctx, `
		INSERT INTO graph.artifact_index
			(node_id, summary, summary_kind, embedding, refreshed_at, machine_id)
		VALUES ('slack:TST-EMB', 'test summary', 'auto', $1, NOW(), 'cloud-test')`,
		pgvector.NewHalfVector(emb))
	if err != nil {
		t.Fatalf("insert artifact_index: %v", err)
	}

	pulled, err := dbA.GetGraphArtifactIndexForPull(ctx, "local-test", 0, 10)
	if err != nil {
		t.Fatalf("GetGraphArtifactIndexForPull: %v", err)
	}
	if len(pulled) != 1 {
		t.Fatalf("expected 1 pulled row, got %d", len(pulled))
	}
	if len(pulled[0].Embedding) != 3072 || pulled[0].Embedding[0] != 0.5 {
		t.Fatalf("embedding did not survive pull: len=%d", len(pulled[0].Embedding))
	}

	// Simulate the receiving side: delete, then import the pulled row.
	if _, err := pool.Exec(ctx, `DELETE FROM graph.artifact_index WHERE node_id = 'slack:TST-EMB'`); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := dbA.ImportGraphArtifactIndex(ctx, &pulled[0]); err != nil {
		t.Fatalf("ImportGraphArtifactIndex: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM graph.artifact_index
		WHERE node_id = 'slack:TST-EMB' AND embedding IS NOT NULL`).Scan(&n); err != nil {
		t.Fatalf("verify query: %v", err)
	}
	if n != 1 {
		t.Fatalf("imported row has NULL embedding")
	}
}
