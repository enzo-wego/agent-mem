package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestBackfillJiraMetadata_EnqueuesMissingPaced(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		INSERT INTO graph.nodes (id, type, natural_key, metadata, machine_id, first_seen_at) VALUES
		('jira:PAY-1', 'jira', 'PAY-1', '{}', 'test', now() - interval '3 day'),
		('jira:PAY-2', 'jira', 'PAY-2', '{"status":"Done"}', 'test', now() - interval '2 day'),
		('jira:PAY-3', 'jira', 'PAY-3', '{}', 'test', now() - interval '1 day'),
		('jira:PAY-4', 'jira', 'PAY-4', '{}', 'test', now()),
		('slack:C1:1.0', 'slack', 'C1:1.0', '{}', 'test', now())`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// PAY-3 already has a queued fetch_body: must be skipped.
	if _, err := pool.Exec(ctx, `INSERT INTO graph.jobs (type, payload, priority, max_attempts, target_runner, machine_id)
		VALUES ('fetch_body', '{"node_id":"jira:PAY-3"}', 5, 5, 'any', 'test')`); err != nil {
		t.Fatalf("seed job: %v", err)
	}

	matched, enqueued, remaining := BackfillJiraMetadata(ctx, pool, zerolog.Nop(), "test", 1, 10*time.Second, false)
	if matched != 1 || enqueued != 1 || remaining != 1 {
		t.Fatalf("page 1: matched=%d enqueued=%d remaining=%d, want 1/1/1", matched, enqueued, remaining)
	}
	matched, enqueued, remaining = BackfillJiraMetadata(ctx, pool, zerolog.Nop(), "test", 10, 10*time.Second, false)
	if matched != 1 || enqueued != 1 || remaining != 0 {
		t.Fatalf("page 2: matched=%d enqueued=%d remaining=%d, want 1/1/0", matched, enqueued, remaining)
	}

	rows, err := pool.Query(ctx, `SELECT payload->>'node_id', priority, available_at FROM graph.jobs
		WHERE type='fetch_body' AND machine_id='test' AND payload->>'node_id' <> 'jira:PAY-3' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id string
		var prio int
		var at time.Time
		if err := rows.Scan(&id, &prio, &at); err != nil {
			t.Fatal(err)
		}
		if prio != 7 {
			t.Errorf("%s priority = %d, want 7", id, prio)
		}
		got = append(got, id)
	}
	// Oldest first; PAY-2 (has status) and PAY-3 (already queued) skipped.
	if strings.Join(got, ",") != "jira:PAY-1,jira:PAY-4" {
		t.Errorf("enqueued = %v, want [jira:PAY-1 jira:PAY-4]", got)
	}

	// force re-fetches PAY-2 too.
	matched, _, _ = BackfillJiraMetadata(ctx, pool, zerolog.Nop(), "test", 10, 10*time.Second, true)
	if matched != 1 {
		t.Errorf("force matched = %d, want 1 (PAY-2)", matched)
	}

	// Pacing: two jobs enqueued in one page are spaced by `spacing`.
	truncateGraphHandlerTables(t, pool)
	if _, err := pool.Exec(ctx, `INSERT INTO graph.nodes (id, type, natural_key, metadata, machine_id) VALUES
		('jira:PAY-1', 'jira', 'PAY-1', '{}', 'test'), ('jira:PAY-2', 'jira', 'PAY-2', '{}', 'test')`); err != nil {
		t.Fatal(err)
	}
	BackfillJiraMetadata(ctx, pool, zerolog.Nop(), "test", 10, 10*time.Second, false)
	var span float64
	if err := pool.QueryRow(ctx, `SELECT EXTRACT(EPOCH FROM max(available_at) - min(available_at)) FROM graph.jobs WHERE type='fetch_body'`).Scan(&span); err != nil {
		t.Fatal(err)
	}
	if span < 9.9 || span > 10.1 {
		t.Errorf("available_at spread = %.2fs, want 10s between two paced jobs", span)
	}
}

func TestBackfillJiraMetadataHandler_RejectsLargeLimit(t *testing.T) {
	h := NewBackfillJiraMetadataHandler(Deps{})
	req := httptest.NewRequest(http.MethodPost, "/api/graph/backfill/jira-metadata", strings.NewReader(`{"limit": 501}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestBackfillCreatedAt_FillsSlackFromTs(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		INSERT INTO graph.nodes (id, type, natural_key, metadata, machine_id, created_at) VALUES
		('slack:C1:1700000000.123456', 'slack', 'C1:1700000000.123456', '{}', 'test', NULL),
		('slack:C1:1600000000.000000', 'slack', 'C1:1600000000.000000', '{}', 'test', '2020-01-01T00:00:00Z'),
		('jira:PAY-9', 'jira', 'PAY-9', '{}', 'test', NULL)`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	deps := Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "test"}
	if err := NewBackfillCreatedAtHandler(deps).Handler(ctx, nil); err != nil {
		t.Fatalf("handler: %v", err)
	}

	var created time.Time
	if err := pool.QueryRow(ctx, `SELECT created_at FROM graph.nodes WHERE id='slack:C1:1700000000.123456'`).Scan(&created); err != nil {
		t.Fatalf("created_at still NULL: %v", err)
	}
	if want := time.Unix(1700000000, 123456000); !created.Equal(want) {
		t.Errorf("created_at = %v, want %v", created, want)
	}
	if err := pool.QueryRow(ctx, `SELECT created_at FROM graph.nodes WHERE id='slack:C1:1600000000.000000'`).Scan(&created); err != nil {
		t.Fatal(err)
	}
	if !created.Equal(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("existing created_at overwritten: %v", created)
	}
	// Slack rows are filled in SQL, never fetched; the jira stub is still
	// routed through fetch_body.
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph.jobs WHERE type='fetch_body'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("fetch_body jobs = %d, want 1 (jira:PAY-9 only)", n)
	}
}

func TestBackfillSubtreeIndex_SelectsPaySubtreeWithoutEmbedding(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM graph.thread_summaries`); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO graph.nodes (id, type, natural_key, scope, metadata, machine_id, created_at) VALUES
		('jira:PAY-2307', 'jira', 'PAY-2307', 'jira', '{}', 'test', now() - interval '10 day'),
		('jira:PAY-2400', 'jira', 'PAY-2400', 'jira', '{}', 'test', now() - interval '9 day'),
		('jira:OPS-1',    'jira', 'OPS-1',    'jira', '{}', 'test', now() - interval '8 day'),
		('gh_pr:wego/pay#1', 'gh_pr', 'wego/pay#1', 'github:wego/pay', '{}', 'test', now() - interval '7 day'),
		('gh_pr:wego/pay#2', 'gh_pr', 'wego/pay#2', 'github:wego/pay', '{}', 'test', now() - interval '6 day'),
		('slack:C1:100.0', 'slack', 'C1:100.0', 'slack:C1', '{"thread_ts":"100.0"}', 'test', now() - interval '5 day'),
		('slack:C1:100.1', 'slack', 'C1:100.1', 'slack:C1', '{"thread_ts":"100.0"}', 'test', now() - interval '5 day'),
		('slack:C1:200.0', 'slack', 'C1:200.0', 'slack:C1', '{"thread_ts":"200.0"}', 'test', now() - interval '4 day'),
		('slack:C1:300.0', 'slack', 'C1:300.0', 'slack:C1', '{"thread_ts":"300.0"}', 'test', now() - interval '3 day'),
		('slack:C1:400.0', 'slack', 'C1:400.0', 'slack:C1', '{"thread_ts":"400.0"}', 'test', now() - interval '2 day'),
		('slack:C1:500.0', 'slack', 'C1:500.0', 'slack:C1', '{"thread_ts":"500.0"}', 'test', now() - interval '1 day')`); err != nil {
		t.Fatalf("seed nodes: %v", err)
	}
	// PR#1 and reply 100.1 reference PAY issues; PR#2 references OPS only.
	if _, err := pool.Exec(ctx, `
		INSERT INTO graph.edges (from_node_id, to_node_id, kind, machine_id) VALUES
		('gh_pr:wego/pay#1', 'jira:PAY-2400', 'REFERENCES', 'test'),
		('slack:C1:100.1',   'jira:PAY-2307', 'REFERENCES', 'test'),
		('gh_pr:wego/pay#2', 'jira:OPS-1',    'REFERENCES', 'test')`); err != nil {
		t.Fatalf("seed edges: %v", err)
	}
	// 200.0: substantive summary → in. 300.0: chatter → out. 400.0: empty → out.
	// 500.0: substantive but already embedded → out.
	if _, err := pool.Exec(ctx, `
		INSERT INTO graph.thread_summaries (channel_id, thread_ts, signature, summary, kind) VALUES
		('C1', '200.0', 's', 'Refund flow for GST', 'substantive'),
		('C1', '300.0', 's', 'thanks all', 'chatter'),
		('C1', '400.0', 's', '', 'substantive'),
		('C1', '500.0', 's', 'Indexed already', 'substantive')`); err != nil {
		t.Fatalf("seed summaries: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO graph.artifact_index (node_id, summary, summary_kind, embedding, machine_id) VALUES
		('slack:C1:500.0', 'Indexed already', 'thread_summary', array_fill(0.1, ARRAY[3072])::halfvec, 'test'),
		('jira:PAY-2307', 'PAY-2307', 'heuristic', NULL, 'test')`); err != nil {
		t.Fatalf("seed index: %v", err)
	}
	// PAY-2400 already has an index_artifact job queued → skipped.
	if _, err := pool.Exec(ctx, `INSERT INTO graph.jobs (type, payload, priority, max_attempts, target_runner, machine_id)
		VALUES ('index_artifact', '{"node_id":"jira:PAY-2400"}', 5, 5, 'any', 'test')`); err != nil {
		t.Fatal(err)
	}

	matched, enqueued, remaining := BackfillSubtreeIndex(ctx, pool, zerolog.Nop(), "test", "PAY", 500)
	if matched != enqueued {
		t.Errorf("matched=%d enqueued=%d", matched, enqueued)
	}
	if remaining != 0 {
		t.Errorf("remaining = %d, want 0", remaining)
	}

	rows, err := pool.Query(ctx, `SELECT payload->>'node_id', priority FROM graph.jobs
		WHERE type='index_artifact' AND priority=3 ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id string
		var prio int
		if err := rows.Scan(&id, &prio); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	// Newest first. Excluded: OPS-1 (other project), PR#2 (refs OPS only),
	// 300.0 (chatter), 400.0 (empty summary), 500.0 (embedded), PAY-2400
	// (already queued), 100.0 (root without a summary and no PAY reference).
	// PAY-2307's heuristic dedup row (NULL embedding, non-Slack) is deliberate,
	// not a gap.
	want := "slack:C1:200.0,slack:C1:100.1,gh_pr:wego/pay#1"
	if strings.Join(got, ",") != want {
		t.Errorf("enqueued = %v\nwant %s", got, want)
	}
}

func TestBackfillSubtreeIndexHandler_RejectsBadProject(t *testing.T) {
	h := NewBackfillSubtreeIndexHandler(Deps{})
	req := httptest.NewRequest(http.MethodPost, "/api/graph/backfill/subtree-index", strings.NewReader(`{"project": "pay; drop"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}
