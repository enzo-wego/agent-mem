package sync

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/config"
	"github.com/agent-mem/agent-mem/internal/database"
)

// TestEngine_PullImportsGraphNotFlat pins the read-replica topology on the pull
// side (docs/ai/round-local-graph-replica.md): the hub serves graph memory only.
// The hub response deliberately carries both a graph node and a flat observation;
// pull() must import the node and ignore the observation, and the request URL it
// sends must no longer ask for any flat or jobs cursor.
func TestEngine_PullImportsGraphNotFlat(t *testing.T) {
	pool := openTestPool(t)
	truncateGraphSyncTables(t, pool)

	ctx := context.Background()
	db := database.NewDB(pool)

	const hub = "test-hub-pull"
	const laptop = "test-laptop-pull"
	const nodeID = "jira:TST-REPLICA-1"
	const obsMarker = "test-replica-flat-session"

	if _, err := pool.Exec(ctx, `DELETE FROM observations WHERE memory_session_id = $1`, obsMarker); err != nil {
		t.Fatalf("clean observations: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(), `DELETE FROM observations WHERE memory_session_id = $1`, obsMarker)
	})

	now := time.Now().UTC().Truncate(time.Second)
	body := "replica body"
	nodeSyncID := "11111111-1111-1111-1111-111111111111"
	obsSyncID := "22222222-2222-2222-2222-222222222222"
	hubSource := hub

	node := database.SyncableGraphNode{
		ID:           nodeID,
		Type:         "jira",
		NaturalKey:   "TST-REPLICA-1",
		Body:         &body,
		BodyRevision: 1,
		BodyTS:       &now,
		Metadata:     []byte("{}"),
		FirstSeenAt:  now,
		UpdatedAt:    now,
		SyncID:       nodeSyncID,
		SyncVersion:  0,
		MachineID:    hub,
	}
	obs := database.SyncableObservation{
		Observation: database.Observation{
			MemorySessionID: obsMarker,
			Project:         "test",
			Type:            "observation",
			CreatedAt:       now,
			CreatedAtEpoch:  now.Unix(),
			SyncID:          &obsSyncID,
			SyncVersion:     0,
			SyncSource:      &hubSource,
		},
	}

	var firstURL string
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			firstURL = r.URL.String()
			json.NewEncoder(w).Encode(SyncPullResponse{
				Observations: []database.SyncableObservation{obs},
				GraphNodes:   []database.SyncableGraphNode{node},
				Cursors:      PullCursors{GraphNodes: EncodeCursor(node.UpdatedAt, node.ID)},
			})
			return
		}
		// Empty response terminates the pull loop.
		json.NewEncoder(w).Encode(SyncPullResponse{})
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{MachineID: laptop, SyncURL: srv.URL, APIKey: "test"}
	e := NewEngine(db, cfg)
	if err := e.pull(ctx); err != nil {
		t.Fatalf("pull: %v", err)
	}

	// Graph memory is imported.
	var nodeCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph.nodes WHERE id = $1`, nodeID).Scan(&nodeCount); err != nil {
		t.Fatalf("count node: %v", err)
	}
	if nodeCount != 1 {
		t.Fatalf("expected graph node imported by pull, got count %d", nodeCount)
	}

	// Flat memory is NOT imported: pull is graph-only now.
	var obsCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM observations WHERE memory_session_id = $1`, obsMarker).Scan(&obsCount); err != nil {
		t.Fatalf("count observation: %v", err)
	}
	if obsCount != 0 {
		t.Fatalf("pull imported a flat observation; want graph-only, got count %d", obsCount)
	}

	// The pull request must not carry any flat or jobs cursor, and must still
	// carry graph cursors.
	for _, banned := range []string{"obs_after", "sum_after", "prompt_after", "sess_after", "g_jobs_after"} {
		if strings.Contains(firstURL, banned) {
			t.Errorf("pull URL still carries %q: %s", banned, firstURL)
		}
	}
	if !strings.Contains(firstURL, "g_nodes_after") {
		t.Errorf("pull URL missing g_nodes_after: %s", firstURL)
	}
}

// TestEngine_PushSendsFlatNotGraph pins the read-replica topology on the push
// side: push() carries flat memory only. An unsynced local graph node must not
// appear in the pushed payload and must not be marked synced afterward.
func TestEngine_PushSendsFlatNotGraph(t *testing.T) {
	pool := openTestPool(t)
	truncateGraphSyncTables(t, pool)

	ctx := context.Background()
	db := database.NewDB(pool)

	const laptop = "test-laptop-push"
	const nodeID = "jira:TST-PUSH-1"

	if _, err := pool.Exec(ctx, `
		INSERT INTO graph.nodes
			(id, type, natural_key, body, body_revision, body_ts, updated_at, machine_id)
		VALUES ($1, 'jira', 'TST-PUSH-1', 'push body', 1, NOW(), NOW(), $2)
		ON CONFLICT (id) DO NOTHING`, nodeID, laptop); err != nil {
		t.Fatalf("insert local graph node: %v", err)
	}

	// Precondition: the node is unsynced.
	pre, err := db.GetUnsyncedGraphNodes(ctx, 100)
	if err != nil {
		t.Fatalf("GetUnsyncedGraphNodes: %v", err)
	}
	if !containsNode(pre, nodeID) {
		t.Fatalf("expected node %q unsynced before push", nodeID)
	}

	var reqBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(SyncPushResponse{Received: 0})
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{MachineID: laptop, SyncURL: srv.URL, APIKey: "test"}
	e := NewEngine(db, cfg)
	if err := e.push(ctx); err != nil {
		t.Fatalf("push: %v", err)
	}

	// Wire check: the payload carries no graph fields at all.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(reqBody, &raw); err != nil {
		t.Fatalf("decode pushed payload: %v", err)
	}
	for _, k := range []string{
		"graph_people", "graph_nodes", "graph_edges", "graph_artifact_index",
		"graph_artifact_bodies", "graph_slack_groups", "graph_entities",
		"graph_jobs", "graph_user_affinity_config",
	} {
		if _, ok := raw[k]; ok {
			t.Errorf("push payload still carries graph field %q", k)
		}
	}

	// Behavioral check: the local graph node was not marked synced.
	post, err := db.GetUnsyncedGraphNodes(ctx, 100)
	if err != nil {
		t.Fatalf("GetUnsyncedGraphNodes after push: %v", err)
	}
	if !containsNode(post, nodeID) {
		t.Errorf("push marked local graph node %q synced; graph must not sync from this side", nodeID)
	}
}

func containsNode(nodes []database.SyncableGraphNode, id string) bool {
	for _, n := range nodes {
		if n.ID == id {
			return true
		}
	}
	return false
}
