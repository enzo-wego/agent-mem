package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/extractor"
	"github.com/agent-mem/agent-mem/internal/graph/fetchers"
	"github.com/agent-mem/agent-mem/internal/graph/identity"
	"github.com/agent-mem/agent-mem/internal/graph/normalizer"
)

type jiraStubIssue struct {
	desc        string
	attachments []string // attachment ids
}

// jiraStubDeps serves the given issues from a stub Jira and returns Deps wired
// to the pool.
func jiraStubDeps(t *testing.T, pool *pgxpool.Pool, issues map[string]jiraStubIssue) Deps {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The fetcher also reads the comment and remote-link sub-resources.
		if strings.HasSuffix(r.URL.Path, "/comment") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"total":0,"comments":[]}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/remotelink") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
			return
		}
		key := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		is, ok := issues[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		var atts []map[string]any
		for _, id := range is.attachments {
			atts = append(atts, map[string]any{"id": id, "filename": id + ".png", "mimeType": "image/png", "size": 10, "content": "http://x/" + id})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"key": key, "fields": map[string]any{
			"summary": key,
			"description": map[string]any{"type": "doc", "content": []any{map[string]any{"type": "paragraph",
				"content": []any{map[string]any{"type": "text", "text": is.desc}}}}},
			"status":     map[string]any{"name": "Open"},
			"created":    "2026-03-01T10:00:00.000+0000",
			"updated":    "2026-03-05T10:00:00.000+0000",
			"attachment": atts,
		}})
	}))
	t.Cleanup(srv.Close)
	norms := normalizer.NewRegistry()
	norms.Register(normalizer.NewJiraNormalizer())
	return Deps{
		DB:          pool,
		Logger:      zerolog.Nop(),
		MachineID:   "test-machine",
		Fetchers:    fetchers.NewRegistry(fetchers.Config{JiraBaseURL: srv.URL, HTTPClient: srv.Client()}, zerolog.Nop()),
		Normalizers: norms,
		Extractor:   extractor.New(pool, zerolog.Nop()),
		Identity:    identity.NewService(pool, zerolog.Nop()),
	}
}

func runFetch(t *testing.T, deps Deps, p fetchBodyPayload) {
	t.Helper()
	raw, _ := json.Marshal(p)
	if err := NewFetchBodyHandler(deps).Handler(context.Background(), raw); err != nil {
		t.Fatalf("fetch_body %s: %v", p.NodeID, err)
	}
}

func countJobs(t *testing.T, pool *pgxpool.Pool, typ string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM graph.jobs WHERE type=$1`, typ).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func resetFetchTest(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	t.Cleanup(func() { truncateGraphHandlerTables(t, pool) })
	return pool
}

func seedJiraStub(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO graph.nodes (id, type, natural_key, machine_id) VALUES ($1,'jira',$2,'test')
		ON CONFLICT (id) DO NOTHING`, "jira:"+key, key); err != nil {
		t.Fatal(err)
	}
}

func seedAttBody(t *testing.T, pool *pgxpool.Pool, attID, body string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO graph.nodes (id, type, natural_key, machine_id)
		VALUES ($1,'jira_attachment',$2,'test') ON CONFLICT (id) DO NOTHING`, attID, strings.TrimPrefix(attID, "jira_attachment:")); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO graph.artifact_bodies (node_id, body_full, machine_id)
		VALUES ($1,$2,'test') ON CONFLICT (node_id) DO UPDATE SET body_full=EXCLUDED.body_full`, attID, body); err != nil {
		t.Fatal(err)
	}
}

func TestFetchBody_SkipAttachmentsFlag(t *testing.T) {
	pool := resetFetchTest(t)
	ctx := context.Background()
	deps := jiraStubDeps(t, pool, map[string]jiraStubIssue{
		"PAY-3000": {desc: "Related: PAY-3001", attachments: []string{"9001", "9002"}},
		"PAY-3001": {desc: "child", attachments: []string{"9003"}},
	})
	seedJiraStub(t, pool, "PAY-3000")

	runFetch(t, deps, fetchBodyPayload{NodeID: "jira:PAY-3000", SkipAttachments: true})

	var nodes, edges int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM graph.nodes WHERE id IN ('jira_attachment:9001','jira_attachment:9002')`).Scan(&nodes)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM graph.edges WHERE from_node_id='jira:PAY-3000' AND to_node_id IN ('jira_attachment:9001','jira_attachment:9002')`).Scan(&edges)
	if nodes != 2 || edges != 2 {
		t.Errorf("attachment nodes=%d edges=%d, want 2/2", nodes, edges)
	}
	if n := countJobs(t, pool, "describe_attachment"); n != 0 {
		t.Fatalf("describe_attachment jobs = %d, want 0", n)
	}

	var childRaw []byte
	if err := pool.QueryRow(ctx, `SELECT payload FROM graph.jobs WHERE type='fetch_body' AND payload->>'node_id'='jira:PAY-3001'`).Scan(&childRaw); err != nil {
		t.Fatalf("child fetch_body job missing: %v", err)
	}
	var child map[string]any
	_ = json.Unmarshal(childRaw, &child)
	if child["skip_attachments"] != true {
		t.Fatalf("child payload = %s, want skip_attachments true", childRaw)
	}

	var cp fetchBodyPayload
	_ = json.Unmarshal(childRaw, &cp)
	runFetch(t, deps, cp)
	if n := countJobs(t, pool, "describe_attachment"); n != 0 {
		t.Errorf("after child fetch: describe_attachment jobs = %d, want 0", n)
	}
	var childAtt int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM graph.nodes WHERE id='jira_attachment:9003'`).Scan(&childAtt)
	if childAtt != 1 {
		t.Errorf("child attachment node missing")
	}
}

func TestBackfillJiraMetadata_SetsSkipAttachments(t *testing.T) {
	pool := resetFetchTest(t)
	seedJiraStub(t, pool, "PAY-3100")
	seedJiraStub(t, pool, "PAY-3101")

	enq := BackfillJiraMetadata(context.Background(), pool, zerolog.Nop(), "test", jiraBackfillParams{Limit: 10}).Enqueued
	if enq != 2 {
		t.Fatalf("enqueued = %d, want 2", enq)
	}
	rows, err := pool.Query(context.Background(), `SELECT payload FROM graph.jobs WHERE type='fetch_body'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var raw []byte
		_ = rows.Scan(&raw)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		if m["skip_attachments"] != true {
			t.Errorf("payload %s lacks skip_attachments:true", raw)
		}
		n++
	}
	if n != 2 {
		t.Errorf("fetch_body jobs = %d, want 2", n)
	}
}

func seedUndescribed(t *testing.T, pool *pgxpool.Pool, attID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO graph.nodes (id, type, natural_key, machine_id)
		VALUES ($1,'jira_attachment',$1,'test') ON CONFLICT (id) DO NOTHING`, attID); err != nil {
		t.Fatal(err)
	}
}

func descPayload(attID string) map[string]string {
	return map[string]string{"node_id": attID, "external_url": "http://x", "mime": "image/png", "source": "jira"}
}

func TestEnqueueDescribe_Concurrent(t *testing.T) {
	pool := resetFetchTest(t)
	deps := Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "test"}
	const att = "jira_attachment:7001"
	seedUndescribed(t, pool, att)

	orig := describeEligibilityCheck
	describeEligibilityCheck = func(ctx context.Context, tx pgx.Tx, attID string) (bool, error) {
		time.Sleep(50 * time.Millisecond)
		return orig(ctx, tx, attID)
	}
	t.Cleanup(func() { describeEligibilityCheck = orig })

	// Warm the pool so all goroutines hold a connection before the sleep;
	// otherwise lazy dialing staggers them and the race window can close.
	var warm []*pgxpool.Conn
	for i := 0; i < int(pool.Config().MaxConns) && i < 8; i++ {
		c, err := pool.Acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		warm = append(warm, c)
	}
	for _, c := range warm {
		c.Release()
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			enqueueDescribeIfNeeded(context.Background(), deps, att, descPayload(att))
		}()
	}
	close(start)
	wg.Wait()
	if n := countJobs(t, pool, "describe_attachment"); n != 1 {
		t.Errorf("describe_attachment jobs = %d, want 1", n)
	}
}

func TestEnqueueDescribe_Eligibility(t *testing.T) {
	const att = "jira_attachment:7002"
	setup := func(t *testing.T) (*pgxpool.Pool, Deps) {
		pool := resetFetchTest(t)
		seedUndescribed(t, pool, att)
		return pool, Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "test"}
	}
	insertJob := func(t *testing.T, pool *pgxpool.Pool, status string) {
		t.Helper()
		if _, err := pool.Exec(context.Background(), `INSERT INTO graph.jobs (type, payload, status, machine_id)
			VALUES ('describe_attachment', jsonb_build_object('node_id',$1::text), $2, 'test')`, att, status); err != nil {
			t.Fatal(err)
		}
	}
	want := func(t *testing.T, pool *pgxpool.Pool, n int) {
		t.Helper()
		if got := countJobs(t, pool, "describe_attachment"); got != n {
			t.Errorf("describe_attachment jobs = %d, want %d", got, n)
		}
	}
	run := func(t *testing.T, pool *pgxpool.Pool, deps Deps, wantQueued bool) {
		t.Helper()
		jobID, queued := enqueueDescribeIfNeeded(context.Background(), deps, att, descPayload(att))
		if queued != wantQueued {
			t.Errorf("queued = %v, want %v", queued, wantQueued)
		}
		if !queued {
			if jobID != 0 {
				t.Errorf("skipped/error job ID = %d, want 0", jobID)
			}
			return
		}
		var committedID int64
		if err := pool.QueryRow(context.Background(), `
			SELECT id FROM graph.jobs
			WHERE type='describe_attachment' AND payload->>'node_id'=$1`, att).
			Scan(&committedID); err != nil {
			t.Fatalf("committed describe job: %v", err)
		}
		if jobID <= 0 || jobID != committedID {
			t.Errorf("job ID = %d, want committed ID %d", jobID, committedID)
		}
	}

	t.Run("queued", func(t *testing.T) {
		pool, deps := setup(t)
		insertJob(t, pool, "queued")
		run(t, pool, deps, false)
		want(t, pool, 1)
	})
	t.Run("running", func(t *testing.T) {
		pool, deps := setup(t)
		insertJob(t, pool, "running")
		run(t, pool, deps, false)
		want(t, pool, 1)
	})
	t.Run("described", func(t *testing.T) {
		pool, deps := setup(t)
		seedAttBody(t, pool, att, "a description")
		run(t, pool, deps, false)
		want(t, pool, 0)
	})
	t.Run("empty_body", func(t *testing.T) {
		pool, deps := setup(t)
		seedAttBody(t, pool, att, "")
		run(t, pool, deps, true)
		want(t, pool, 1)
	})
	t.Run("check_error", func(t *testing.T) {
		pool, deps := setup(t)
		orig := describeEligibilityCheck
		describeEligibilityCheck = func(ctx context.Context, tx pgx.Tx, attID string) (bool, error) {
			return false, context.DeadlineExceeded
		}
		t.Cleanup(func() { describeEligibilityCheck = orig })
		run(t, pool, deps, false)
		want(t, pool, 0)
	})
	t.Run("begin_error", func(t *testing.T) {
		pool, deps := setup(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		jobID, queued := enqueueDescribeIfNeeded(ctx, deps, att, descPayload(att))
		if queued || jobID != 0 {
			t.Errorf("cancelled begin returned (%d, %v), want (0, false)", jobID, queued)
		}
		want(t, pool, 0)
	})
	t.Run("enqueue_error", func(t *testing.T) {
		pool, deps := setup(t)
		orig := describeEligibilityCheck
		describeEligibilityCheck = func(ctx context.Context, tx pgx.Tx, attID string) (bool, error) {
			// The existing seam closes the transaction, so the real enqueue
			// fails without adding a production seam or changing graph schema.
			if err := tx.Rollback(ctx); err != nil {
				t.Fatalf("close transaction: %v", err)
			}
			return false, nil
		}
		t.Cleanup(func() { describeEligibilityCheck = orig })
		run(t, pool, deps, false)
		want(t, pool, 0)
	})
	t.Run("commit_error", func(t *testing.T) {
		pool, deps := setup(t)
		orig := describeEligibilityCheck
		describeEligibilityCheck = func(ctx context.Context, tx pgx.Tx, attID string) (bool, error) {
			// A temporary deferred constraint rejects Commit after the real
			// enqueue succeeds. Rollback also removes the temporary table.
			// This covers a rejected commit, not connection-loss ambiguity.
			if _, err := tx.Exec(ctx, `
				CREATE TEMP TABLE describe_commit_failure (
					value integer UNIQUE DEFERRABLE INITIALLY DEFERRED
				)`); err != nil {
				t.Fatalf("create deferred constraint: %v", err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO describe_commit_failure VALUES (1), (1)`); err != nil {
				t.Fatalf("seed deferred violation: %v", err)
			}
			return false, nil
		}
		t.Cleanup(func() { describeEligibilityCheck = orig })
		run(t, pool, deps, false)
		want(t, pool, 0)
	})
	// Proves the eligibility check sees work committed after the transaction
	// began (READ COMMITTED, no snapshot reuse). The single-statement property
	// is held by the code, not by this test.
	t.Run("completed_between", func(t *testing.T) {
		pool, deps := setup(t)
		insertJob(t, pool, "running")
		orig := describeEligibilityCheck
		describeEligibilityCheck = func(ctx context.Context, tx pgx.Tx, attID string) (bool, error) {
			// Separate connection: the running job finishes and its body lands
			// after the helper began but before the check runs.
			if _, err := pool.Exec(ctx, `UPDATE graph.jobs SET status='done' WHERE type='describe_attachment'`); err != nil {
				t.Errorf("mark done: %v", err)
			}
			seedAttBody(t, pool, att, "described meanwhile")
			return orig(ctx, tx, attID)
		}
		t.Cleanup(func() { describeEligibilityCheck = orig })
		run(t, pool, deps, false)
		want(t, pool, 1) // only the pre-existing (now done) job
		var queued int
		_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM graph.jobs WHERE type='describe_attachment' AND status='queued'`).Scan(&queued)
		if queued != 0 {
			t.Errorf("queued describe jobs = %d, want 0", queued)
		}
	})
}

func TestFetchBody_SkipsDescribedAttachments(t *testing.T) {
	pool := resetFetchTest(t)
	deps := jiraStubDeps(t, pool, map[string]jiraStubIssue{
		"PAY-3200": {desc: "two files", attachments: []string{"8001", "8002"}},
	})
	seedJiraStub(t, pool, "PAY-3200")
	seedAttBody(t, pool, "jira_attachment:8001", "already described")
	seedUndescribed(t, pool, "jira_attachment:8002")

	runFetch(t, deps, fetchBodyPayload{NodeID: "jira:PAY-3200"})

	rows, err := pool.Query(context.Background(), `SELECT payload->>'node_id' FROM graph.jobs WHERE type='describe_attachment'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		got = append(got, id)
	}
	if len(got) != 1 || got[0] != "jira_attachment:8002" {
		t.Errorf("describe jobs = %v, want [jira_attachment:8002]", got)
	}
}

func TestFetchBody_RefetchEnqueuesNoDescribe(t *testing.T) {
	pool := resetFetchTest(t)
	deps := jiraStubDeps(t, pool, map[string]jiraStubIssue{
		"PAY-3300": {desc: "one file", attachments: []string{"8101"}},
	})
	seedJiraStub(t, pool, "PAY-3300")

	runFetch(t, deps, fetchBodyPayload{NodeID: "jira:PAY-3300"})
	if n := countJobs(t, pool, "describe_attachment"); n != 1 {
		t.Fatalf("first fetch: describe jobs = %d, want 1", n)
	}
	runFetch(t, deps, fetchBodyPayload{NodeID: "jira:PAY-3300"})
	if n := countJobs(t, pool, "describe_attachment"); n != 1 {
		t.Errorf("refetch with queued job: describe jobs = %d, want 1", n)
	}

	if _, err := pool.Exec(context.Background(), `UPDATE graph.jobs SET status='done' WHERE type='describe_attachment'`); err != nil {
		t.Fatal(err)
	}
	seedAttBody(t, pool, "jira_attachment:8101", "now described")
	runFetch(t, deps, fetchBodyPayload{NodeID: "jira:PAY-3300"})
	if n := countJobs(t, pool, "describe_attachment"); n != 1 {
		t.Errorf("refetch after done+body: describe jobs = %d, want 1", n)
	}
}
