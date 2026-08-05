package sync

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"github.com/agent-mem/agent-mem/internal/database"
)

// databaseName extracts the database name from a postgres DSN. A DSN that does
// not parse returns "", which fails the test-database guard closed.
func databaseName(dsn string) string {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return ""
	}
	return config.ConnConfig.Database
}

// openTestPool connects to the DATABASE_URL Postgres instance.
// Skips if DATABASE_URL is not set.
func openTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	// This helper DELETEs every row in the graph tables. On 2026-07-14 an
	// integration test run against the live dev database hard-deleted the graph
	// and synced the damage to prod. Refuse anything whose database name does
	// not say "test" — use agentmem_test, not agentmem. See agent-mem-z14.
	if !strings.Contains(databaseName(dsn), "test") {
		t.Fatalf("refusing to run: DATABASE_URL database name %q does not contain \"test\"; "+
			"these tests delete all rows in the graph tables", databaseName(dsn))
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
	pulled, err := dbB.GetGraphNodesForPull(ctx, machineB, time.Time{}, "", 100)
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

	pulled, err := dbA.GetGraphArtifactIndexForPull(ctx, "local-test", time.Time{}, "", 10)
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

// TestGraphSync_PaginationUnderConcurrentInsert pages graph.nodes with a small
// limit and, between pages, re-sorts an already-returned row (a body refresh
// bumps updated_at) and inserts a brand-new row — the concurrent activity the
// cloud produces. The (updated_at, id) keyset must never LOSE a row and must
// terminate. A row whose updated_at is bumped past a cursor we already passed is
// re-delivered; that is expected and harmless (Import* uses ON CONFLICT DO
// NOTHING), and is the correctness trade the composite cursor makes to guarantee
// new/updated rows are always delivered.
func TestGraphSync_PaginationUnderConcurrentInsert(t *testing.T) {
	pool := openTestPool(t)
	truncateGraphSyncTables(t, pool)
	ctx := context.Background()
	db := database.NewDB(pool)

	const cloud = "cloud-test"
	const local = "local-test"
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	insert := func(id string, updated time.Time) {
		if _, err := pool.Exec(ctx, `
			INSERT INTO graph.nodes
				(id, type, natural_key, body, body_revision, body_ts, updated_at, machine_id)
			VALUES ($1, 'jira', $1, 'b', 1, $2, $2, $3)`, id, updated, cloud); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	for i := 1; i <= 5; i++ {
		insert(fmt.Sprintf("test:node:%02d", i), base.Add(time.Duration(i)*time.Second))
	}

	seen := map[string]int{}
	var afterTS time.Time
	var afterPK string
	for iter := 0; ; iter++ {
		if iter > 50 {
			t.Fatalf("pagination did not terminate")
		}
		batch, err := db.GetGraphNodesForPull(ctx, local, afterTS, afterPK, 2)
		if err != nil {
			t.Fatalf("pull: %v", err)
		}
		if len(batch) == 0 {
			break
		}
		for _, n := range batch {
			seen[n.ID]++
		}
		last := batch[len(batch)-1]
		afterTS, afterPK = last.UpdatedAt, last.ID
		if iter == 0 {
			// Concurrent activity: node 01's body is refreshed (updated_at jumps to
			// the tail) and a new node arrives. Under (updated_at, id), node 01 now
			// sorts after the cursor and is delivered a second time.
			if _, err := pool.Exec(ctx, `UPDATE graph.nodes SET updated_at = $1 WHERE id = 'test:node:01'`, base.Add(100*time.Second)); err != nil {
				t.Fatalf("bump: %v", err)
			}
			insert("test:node:09", base.Add(200*time.Second))
		}
	}

	// No pre-existing row is lost, and the never-re-sorted ones arrive once.
	for i := 2; i <= 5; i++ {
		id := fmt.Sprintf("test:node:%02d", i)
		if seen[id] != 1 {
			t.Errorf("row %s delivered %d times, want exactly 1", id, seen[id])
		}
	}
	// node 01's updated_at was bumped past the cursor mid-walk, so it is delivered
	// twice — the expected, harmless re-delivery (dropped by ON CONFLICT DO NOTHING).
	if seen["test:node:01"] != 2 {
		t.Errorf("re-sorted row test:node:01 delivered %d times, want 2 (re-delivery)", seen["test:node:01"])
	}
	if seen["test:node:09"] != 1 {
		t.Errorf("concurrently-inserted row test:node:09 delivered %d times, want exactly 1", seen["test:node:09"])
	}
}

// TestGraphSync_ImportAbsorbsNaturalKeyCollision verifies the bare
// ON CONFLICT DO NOTHING absorbs a collision on the row's natural key (nodes.id)
// even when the incoming row carries a different sync_id — the routine
// cross-machine case where both sides derive the same id.
func TestGraphSync_ImportAbsorbsNaturalKeyCollision(t *testing.T) {
	pool := openTestPool(t)
	truncateGraphSyncTables(t, pool)
	ctx := context.Background()
	db := database.NewDB(pool)

	base := time.Now().UTC().Truncate(time.Second)
	mk := func(syncID string) *database.SyncableGraphNode {
		return &database.SyncableGraphNode{
			ID:          "jira:COLLIDE-1",
			Type:        "jira",
			NaturalKey:  "COLLIDE-1",
			Metadata:    []byte("{}"),
			FirstSeenAt: base,
			UpdatedAt:   base,
			SyncID:      syncID,
			MachineID:   "cloud-test",
		}
	}
	if err := db.ImportGraphNode(ctx, mk("11111111-1111-1111-1111-111111111111")); err != nil {
		t.Fatalf("first import: %v", err)
	}
	// Same natural id, different sync_id — must be absorbed, not error.
	if err := db.ImportGraphNode(ctx, mk("22222222-2222-2222-2222-222222222222")); err != nil {
		t.Fatalf("collision import should be absorbed, got: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph.nodes WHERE id = 'jira:COLLIDE-1'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("want exactly 1 row for id after collision, got %d", n)
	}
}

// TestGraphSync_EmptyCursorReturnsLowest confirms a zero-time cursor means "from
// the beginning": every real updated_at exceeds the zero time, so all rows come
// back ordered by (updated_at, id). With equal timestamps the id tiebreaker
// orders them, so the lowest id arrives first.
func TestGraphSync_EmptyCursorReturnsLowest(t *testing.T) {
	pool := openTestPool(t)
	truncateGraphSyncTables(t, pool)
	ctx := context.Background()
	db := database.NewDB(pool)

	ts := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	for _, id := range []string{"test:node:b", "test:node:a", "test:node:c"} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO graph.nodes
				(id, type, natural_key, body, body_revision, body_ts, updated_at, machine_id)
			VALUES ($1, 'jira', $1, 'b', 1, $2, $2, 'cloud-test')`, id, ts); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}

	pulled, err := db.GetGraphNodesForPull(ctx, "local-test", time.Time{}, "", 10)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(pulled) != 3 {
		t.Fatalf("want 3 rows, got %d", len(pulled))
	}
	if pulled[0].ID != "test:node:a" {
		t.Errorf("zero cursor should return the lowest (updated_at, id) row first, got %q", pulled[0].ID)
	}
}

// TestCursorEncodeDecodeRoundTrip covers the cursor codec directly (no DB): a
// real pair round-trips, and empty / unparseable values (a bare parked id, a
// pair with a bad timestamp) all decode to the zero time + empty pk = "from the
// beginning".
func TestCursorEncodeDecodeRoundTrip(t *testing.T) {
	ts := time.Date(2026, 8, 5, 9, 30, 15, 123456789, time.UTC)
	pk := "slack:C011RFSBLP3:1709557592.431279"

	gotTS, gotPK := DecodeCursor(EncodeCursor(ts, pk))
	if !gotTS.Equal(ts) || gotPK != pk {
		t.Errorf("round-trip mismatch: got (%s, %q), want (%s, %q)", gotTS, gotPK, ts, pk)
	}

	for _, bad := range []string{"", "wego_order:WF-ABC", "slack:C011RFSBLP3:1709557592.431279", "not-a-timestamp|slack:x"} {
		gotTS, gotPK := DecodeCursor(bad)
		if !gotTS.IsZero() || gotPK != "" {
			t.Errorf("DecodeCursor(%q) = (%s, %q), want zero time and empty pk", bad, gotTS, gotPK)
		}
	}
}

// TestGraphSync_LowerSortingNodeStillDelivered is the regression that pins this
// bug. After walking graph.nodes to exhaustion, a node whose id sorts BEFORE
// every id already seen — but carrying a fresh updated_at — must still be
// delivered. The e394fa3 id-only keyset drops it (its id is below the cursor id);
// the (updated_at, id) keyset delivers it because its updated_at exceeds the
// cursor's timestamp.
func TestGraphSync_LowerSortingNodeStillDelivered(t *testing.T) {
	pool := openTestPool(t)
	truncateGraphSyncTables(t, pool)
	ctx := context.Background()
	db := database.NewDB(pool)

	const cloud = "cloud-test"
	const local = "local-test"
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	insert := func(id string, updated time.Time) {
		if _, err := pool.Exec(ctx, `
			INSERT INTO graph.nodes
				(id, type, natural_key, body, body_revision, body_ts, updated_at, machine_id)
			VALUES ($1, 'jira', $1, 'b', 1, $2, $2, $3)`, id, updated, cloud); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	insert("mmm:1", base.Add(1*time.Second))
	insert("nnn:2", base.Add(2*time.Second))
	insert("zzz:3", base.Add(3*time.Second))

	// Walk to exhaustion, tracking the (updated_at, id) cursor.
	var afterTS time.Time
	var afterPK string
	for iter := 0; ; iter++ {
		if iter > 50 {
			t.Fatalf("walk did not terminate")
		}
		batch, err := db.GetGraphNodesForPull(ctx, local, afterTS, afterPK, 2)
		if err != nil {
			t.Fatalf("pull: %v", err)
		}
		if len(batch) == 0 {
			break
		}
		last := batch[len(batch)-1]
		afterTS, afterPK = last.UpdatedAt, last.ID
	}

	// A new node whose id sorts before every seen id, with a fresh updated_at.
	insert("aaa:new", base.Add(100*time.Second))

	batch, err := db.GetGraphNodesForPull(ctx, local, afterTS, afterPK, 100)
	if err != nil {
		t.Fatalf("pull after insert: %v", err)
	}
	delivered := false
	for _, n := range batch {
		if n.ID == "aaa:new" {
			delivered = true
		}
	}
	if !delivered {
		t.Errorf("new lower-sorting node aaa:new was not delivered after the cursor advanced")
	}
}

// TestGraphSync_SameTimestampAllDelivered proves the id tiebreaker: rows sharing
// one updated_at, paged one at a time, each arrive exactly once and the walk
// terminates. Without the pk tiebreaker a same-timestamp cursor would loop on
// one row or skip the rest.
func TestGraphSync_SameTimestampAllDelivered(t *testing.T) {
	pool := openTestPool(t)
	truncateGraphSyncTables(t, pool)
	ctx := context.Background()
	db := database.NewDB(pool)

	const local = "local-test"
	ts := time.Date(2025, 3, 3, 8, 0, 0, 0, time.UTC)
	ids := []string{"test:same:a", "test:same:b", "test:same:c", "test:same:d"}
	for _, id := range ids {
		if _, err := pool.Exec(ctx, `
			INSERT INTO graph.nodes
				(id, type, natural_key, body, body_revision, body_ts, updated_at, machine_id)
			VALUES ($1, 'jira', $1, 'b', 1, $2, $2, 'cloud-test')`, id, ts); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}

	seen := map[string]int{}
	var afterTS time.Time
	var afterPK string
	for iter := 0; ; iter++ {
		if iter > 50 {
			t.Fatalf("pagination did not terminate (pk tiebreaker not advancing)")
		}
		batch, err := db.GetGraphNodesForPull(ctx, local, afterTS, afterPK, 1)
		if err != nil {
			t.Fatalf("pull: %v", err)
		}
		if len(batch) == 0 {
			break
		}
		for _, n := range batch {
			seen[n.ID]++
		}
		last := batch[len(batch)-1]
		afterTS, afterPK = last.UpdatedAt, last.ID
	}

	for _, id := range ids {
		if seen[id] != 1 {
			t.Errorf("same-timestamp row %s delivered %d times, want exactly 1", id, seen[id])
		}
	}
}

// TestGraphSync_UnparseableCursorWalksFromStart confirms the deliberate
// fail-open: a parked bare-id cursor ("wego_order:WF-ABC") does not parse as a
// pair, so it decodes to the zero time and the walk restarts from the lowest row
// rather than returning nothing.
func TestGraphSync_UnparseableCursorWalksFromStart(t *testing.T) {
	pool := openTestPool(t)
	truncateGraphSyncTables(t, pool)
	ctx := context.Background()
	db := database.NewDB(pool)

	base := time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)
	for i, id := range []string{"test:walk:1", "test:walk:2", "test:walk:3"} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO graph.nodes
				(id, type, natural_key, body, body_revision, body_ts, updated_at, machine_id)
			VALUES ($1, 'jira', $1, 'b', 1, $2, $2, 'cloud-test')`,
			id, base.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}

	afterTS, afterPK := DecodeCursor("wego_order:WF-ABC")
	pulled, err := db.GetGraphNodesForPull(ctx, "local-test", afterTS, afterPK, 10)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(pulled) != 3 {
		t.Fatalf("unparseable cursor should walk from the start; want 3 rows, got %d", len(pulled))
	}
	if pulled[0].ID != "test:walk:1" {
		t.Errorf("want lowest (updated_at, id) row first, got %q", pulled[0].ID)
	}
}
