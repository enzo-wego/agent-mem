package handlers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/gemini"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

type reindexCountingClient struct {
	inputs          []string
	generate, cheap atomic.Int32
	embed           func(context.Context, string) error
}

func (c *reindexCountingClient) Embed(ctx context.Context, text string) ([]float32, error) {
	c.inputs = append(c.inputs, text)
	if c.embed != nil {
		if err := c.embed(ctx, text); err != nil {
			return nil, err
		}
	}
	v := make([]float32, GraphEmbeddingDims)
	v[0] = 1
	return v, nil
}
func (c *reindexCountingClient) EmbedWithOptions(ctx context.Context, text string, opts gemini.EmbedOptions) ([]float32, error) {
	if opts.OutputDimensionality != GraphEmbeddingDims {
		return nil, fmt.Errorf("dimensions = %d", opts.OutputDimensionality)
	}
	return c.Embed(ctx, text)
}
func (c *reindexCountingClient) Describe(context.Context, string, []byte, string) (string, string, []string, error) {
	return "", "", nil, errors.New("unexpected Describe")
}
func (c *reindexCountingClient) Generate(context.Context, string, string) (string, error) {
	c.generate.Add(1)
	return `{"same_topic":false,"confidence":0.9}`, nil
}
func (c *reindexCountingClient) GenerateCheap(context.Context, string, string) (string, error) {
	c.cheap.Add(1)
	return `{"same_topic":false,"confidence":0.9}`, nil
}

func reindexFixture(t *testing.T, n int) (Deps, ReindexHeuristicOptions, *reindexCountingClient) {
	t.Helper()
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	ctx := context.Background()
	var since time.Time
	if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&since); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("jira:PAY-%03d", i)
		reindexSeed(t, pool, id, "jira", "heuristic", fmt.Sprintf("Repair payment %03d", i), "Background\n\nReturn HTTP 409 for duplicate refunds")
	}
	client := &reindexCountingClient{}
	return Deps{DB: pool, Gemini: client, Logger: zerolog.Nop(), MachineID: "test"}, ReindexHeuristicOptions{Since: since, IntervalMS: 50}, client
}

func reindexSeed(t *testing.T, pool *pgxpool.Pool, id, typ, kind, title, body string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO graph.nodes (id,type,natural_key,title,body,machine_id) VALUES ($1,$2,$1,$3,$4,'test')`, id, typ, title, body); err != nil {
		t.Fatal(err)
	}
	if kind != "" {
		if _, err := pool.Exec(ctx, `INSERT INTO graph.artifact_index (node_id,summary,summary_kind,refreshed_at,machine_id) VALUES ($1,'Background',$2,clock_timestamp()-interval '1 hour','test')`, id, kind); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReindexHeuristicCLI_PopulationResumeAndNoChat(t *testing.T) {
	deps, opts, c := reindexFixture(t, 1)
	reindexSeed(t, deps.DB, "gh_pr:wego/payments#2", "gh_pr", "heuristic", "Fix duplicate refunds", "Background\nReturn HTTP 409 for duplicate refunds to protect the payment ledger")
	reindexSeed(t, deps.DB, "cf:3", "cf", "heuristic", "Payment runbook", "Overview\nEscalate refunds")
	reindexSeed(t, deps.DB, "jira:PAY-004", "jira", "", "Title only", "")
	reindexSeed(t, deps.DB, "jira:PAY-005", "jira", "", "Effective body empty", "ignored node body")
	if _, err := deps.DB.Exec(context.Background(), `INSERT INTO graph.artifact_bodies (node_id,body_full,machine_id) VALUES ('jira:PAY-005','','test')`); err != nil {
		t.Fatal(err)
	}
	reindexSeed(t, deps.DB, "slack:C1:1", "slack", "heuristic", "Slack", "Keep")
	reindexSeed(t, deps.DB, "jira:PAY-006", "jira", "thread_summary", "Keep summary", "Keep")
	reindexSeed(t, deps.DB, "datadog:7", "datadog", "", "Alert", "")
	reindexSeed(t, deps.DB, "jira:PAY-008", "jira", "heuristic", "New writer", "Keep")
	if _, err := deps.DB.Exec(context.Background(), `UPDATE graph.artifact_index SET refreshed_at=$1 WHERE node_id='jira:PAY-008'`, opts.Since); err != nil {
		t.Fatal(err)
	}
	reindexSeed(t, deps.DB, "jira:PAY-009", "jira", "", "\t\n ", "")
	reindexSeed(t, deps.DB, "jira:PAY-010", "jira", "", "Not empty", "Substantive unindexed body")
	var out bytes.Buffer
	opts.Output = &out
	opts.MaxRows = 2
	first, err := RunReindexHeuristic(context.Background(), deps, opts)
	if err != nil || first.Done != 2 {
		t.Fatalf("first = %+v, %v", first, err)
	}
	opts.MaxRows = 0
	second, err := RunReindexHeuristic(context.Background(), deps, opts)
	if err != nil || second.Done != 3 {
		t.Fatalf("resume = %+v, %v", second, err)
	}
	third, err := RunReindexHeuristic(context.Background(), deps, opts)
	if err != nil || third.Done != 0 {
		t.Fatalf("finished resume = %+v, %v", third, err)
	}
	seen := map[string]bool{}
	for _, input := range c.inputs {
		if input == "preflight" {
			continue
		}
		if seen[input] {
			t.Fatalf("processed twice: %q", input)
		}
		seen[input] = true
	}
	if len(seen) != 5 {
		t.Fatalf("embedding inputs = %v", c.inputs)
	}
	rows, err := deps.DB.Query(context.Background(), `SELECT payload FROM graph.jobs WHERE type='link_topics' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var payloads [][]byte
	for rows.Next() {
		var p []byte
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(payloads) != 5 {
		t.Fatalf("link jobs = %d", len(payloads))
	}
	for _, p := range payloads {
		if err := NewLinkTopicsHandler(deps).Handler(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	if c.generate.Load() != 0 || c.cheap.Load() != 0 {
		t.Fatalf("chat = %d Generate, %d GenerateCheap", c.generate.Load(), c.cheap.Load())
	}
	if !strings.Contains(out.String(), "last_id=") || !strings.Contains(out.String(), opts.Since.UTC().Format(time.RFC3339Nano)) {
		t.Fatalf("output: %s", &out)
	}
}

func TestReindexHeuristicCLI_EmptyDoesNotConsumeMaxRows(t *testing.T) {
	deps, opts, _ := reindexFixture(t, 1)
	reindexSeed(t, deps.DB, "cf:0", "cf", "heuristic", "", "")
	opts.MaxRows = 1
	var out bytes.Buffer
	opts.Output = &out
	result, err := RunReindexHeuristic(context.Background(), deps, opts)
	if err != nil || result.Done != 1 || result.LastID != "jira:PAY-001" || !strings.Contains(out.String(), "skipped (empty)") {
		t.Fatalf("empty/max rows = %+v, %v, output=%s", result, err, &out)
	}
}

func TestReindexHeuristicCLI_EmbedDeadlinesContinue(t *testing.T) {
	deps, opts, c := reindexFixture(t, 2)
	c.embed = func(ctx context.Context, text string) error {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 60*time.Second {
			t.Errorf("embedding has no bounded 60s deadline")
		}
		if text != "preflight" {
			return context.DeadlineExceeded
		}
		return nil
	}
	result, err := RunReindexHeuristic(context.Background(), deps, opts)
	if err != nil || result.Skipped != 2 || result.Done != 0 {
		t.Fatalf("embed timeouts = %+v, %v", result, err)
	}
}

func TestIndexArtifact_BodyAfterTitle(t *testing.T) {
	deps, _, c := reindexFixture(t, 0)
	ctx := context.Background()
	reindexSeed(t, deps.DB, "jira:PAY-999", "jira", "", "Refund repair", "")
	if err := indexArtifactNode(ctx, deps, "jira:PAY-999", false, true); err != nil {
		t.Fatal(err)
	}
	if _, err := deps.DB.Exec(ctx, `INSERT INTO graph.artifact_bodies (node_id,body_full,fetched_at,machine_id) VALUES ('jira:PAY-999','Return HTTP 409',clock_timestamp(),'test')`); err != nil {
		t.Fatal(err)
	}
	if err := indexArtifactNode(ctx, deps, "jira:PAY-999", false, true); err != nil {
		t.Fatal(err)
	}
	var summary string
	if err := deps.DB.QueryRow(ctx, `SELECT summary FROM graph.artifact_index WHERE node_id='jira:PAY-999'`).Scan(&summary); err != nil {
		t.Fatal(err)
	}
	if summary != "Refund repair\nReturn HTTP 409" || len(c.inputs) != 2 {
		t.Fatalf("body arrival: summary=%q inputs=%v", summary, c.inputs)
	}
}

func TestReindexHeuristicCLI_DryRunUntouched(t *testing.T) {
	deps, opts, _ := reindexFixture(t, 25)
	deps.Gemini = nil
	opts.DryRun = true
	before := reindexSnapshot(t, deps.DB)
	result, err := RunReindexHeuristic(context.Background(), deps, opts)
	if err != nil || result.Eligible != 25 || len(result.SampleIDs) != 20 || result.Done != 0 {
		t.Fatalf("dry run = %+v, %v", result, err)
	}
	if after := reindexSnapshot(t, deps.DB); before != after {
		t.Fatalf("dry run changed DB\nbefore %s\nafter %s", before, after)
	}
}

func reindexSnapshot(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), `SELECT json_build_object('index',(SELECT json_agg(a ORDER BY node_id) FROM graph.artifact_index a),'jobs',(SELECT json_agg(j ORDER BY id) FROM graph.jobs j))::text`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestReindexHeuristicCLI_PreflightAndCutoff(t *testing.T) {
	for _, failure := range []string{"nil embedder", "probe failure", "empty probe", "future since"} {
		t.Run(failure, func(t *testing.T) {
			deps, opts, c := reindexFixture(t, 1)
			before := reindexSnapshot(t, deps.DB)
			switch failure {
			case "nil embedder":
				deps.Gemini = nil
			case "probe failure":
				c.embed = func(context.Context, string) error { return errors.New("probe failed") }
			case "empty probe":
				deps.Gemini = &mockGemini{embedResult: func() ([]float32, error) { return nil, nil }}
			case "future since":
				opts.Since = opts.Since.Add(time.Hour)
			}
			var out bytes.Buffer
			opts.Output = &out
			result, err := RunReindexHeuristic(context.Background(), deps, opts)
			if err == nil || result.Done != 0 || before != reindexSnapshot(t, deps.DB) {
				t.Fatalf("preflight = %+v, %v", result, err)
			}
			if !strings.Contains(out.String(), "final") {
				t.Fatalf("missing failure summary: %s", &out)
			}
		})
	}
}

func TestReindexHeuristicCLI_MutualExclusionAndReacquire(t *testing.T) {
	deps, opts, c := reindexFixture(t, 1)
	conn, err := deps.DB.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_lock(hashtext('reindex_heuristic'))`); err != nil {
		t.Fatal(err)
	}
	result, err := RunReindexHeuristic(context.Background(), deps, opts)
	if err == nil || result.Done != 0 || len(c.inputs) != 0 {
		t.Fatalf("contender = %+v, %v, inputs %v", result, err, c.inputs)
	}
	if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext('reindex_heuristic'))`); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if _, err := RunReindexHeuristic(context.Background(), deps, opts); err != nil {
			t.Fatalf("clean reacquire %d: %v", i, err)
		}
	}
}

func TestReindexHeuristicCLI_LockSessionLostPoolUsable(t *testing.T) {
	deps, opts, c := reindexFixture(t, 3)
	c.embed = func(ctx context.Context, text string) error {
		if text == "preflight" {
			return nil
		}
		var pid int
		if err := deps.DB.QueryRow(ctx, `SELECT pid FROM pg_locks WHERE locktype='advisory' AND objid=(hashtext('reindex_heuristic')::bigint & 4294967295)::oid AND objsubid=1 AND granted`).Scan(&pid); err != nil {
			return err
		}
		var terminated bool
		if err := deps.DB.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, pid).Scan(&terminated); err != nil {
			return err
		}
		if !terminated {
			return errors.New("backend not terminated")
		}
		return nil
	}
	result, err := RunReindexHeuristic(context.Background(), deps, opts)
	if err == nil || !strings.Contains(err.Error(), "lock session lost") || result.Done != 1 || len(c.inputs) != 2 {
		t.Fatalf("lost lock = %+v, %v, inputs %v", result, err, c.inputs)
	}
	if err := deps.DB.Ping(context.Background()); err != nil {
		t.Fatalf("pool unusable: %v", err)
	}
	c.embed = nil
	if _, err := RunReindexHeuristic(context.Background(), deps, opts); err != nil {
		t.Fatalf("reacquire after lost backend: %v", err)
	}
}

func TestReindexHeuristicCLI_EmbeddingErrors(t *testing.T) {
	for _, n := range []int{1, 21} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			deps, opts, c := reindexFixture(t, n+1)
			calls := 0
			c.embed = func(_ context.Context, text string) error {
				if text == "preflight" {
					return nil
				}
				calls++
				if calls <= n {
					return errors.New("provider error")
				}
				return nil
			}
			var out bytes.Buffer
			opts.Output = &out
			result, err := RunReindexHeuristic(context.Background(), deps, opts)
			if (err != nil) != (n == 21) || result.Skipped != n || len(result.SkippedIDs) != n {
				t.Fatalf("errors = %+v, %v", result, err)
			}
			if n == 1 && result.Done != 1 {
				t.Fatalf("didn't continue: %+v", result)
			}
			if n == 21 && calls != 21 {
				t.Fatalf("calls = %d", calls)
			}
			for _, id := range result.SkippedIDs {
				if !strings.Contains(out.String(), id) {
					t.Fatalf("missing skipped %s: %s", id, &out)
				}
			}
		})
	}
}

func TestReindexHeuristicCLI_NonEmbeddingErrorLastID(t *testing.T) {
	deps, opts, c := reindexFixture(t, 3)
	c.embed = func(ctx context.Context, text string) error {
		if strings.Contains(text, "001") {
			_, err := deps.DB.Exec(ctx, `CREATE FUNCTION graph.reindex_test_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.node_id='jira:PAY-002' THEN RAISE EXCEPTION 'reindex DB failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reindex_test_fail BEFORE INSERT OR UPDATE ON graph.artifact_index FOR EACH ROW EXECUTE FUNCTION graph.reindex_test_fail()`)
			return err
		}
		return nil
	}
	t.Cleanup(func() {
		if _, err := deps.DB.Exec(context.Background(), `DROP TRIGGER IF EXISTS reindex_test_fail ON graph.artifact_index; DROP FUNCTION IF EXISTS graph.reindex_test_fail()`); err != nil {
			t.Error(err)
		}
	})
	var out bytes.Buffer
	opts.Output = &out
	result, err := RunReindexHeuristic(context.Background(), deps, opts)
	if err == nil || result.Done != 1 || result.Skipped != 0 || result.LastID != "jira:PAY-002" {
		t.Fatalf("DB error = %+v, %v", result, err)
	}
	if !strings.Contains(out.String(), "last_id=jira:PAY-002") {
		t.Fatalf("missing last ID: %s", &out)
	}
}

func TestReindexHeuristicCLI_Cancellation(t *testing.T) {
	t.Run("sleep", func(t *testing.T) {
		deps, opts, c := reindexFixture(t, 3)
		opts.IntervalMS = 5000
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		c.embed = func(_ context.Context, text string) error {
			if text != "preflight" {
				time.AfterFunc(100*time.Millisecond, cancel)
			}
			return nil
		}
		start := time.Now()
		result, err := RunReindexHeuristic(ctx, deps, opts)
		if !errors.Is(err, context.Canceled) || result.Done != 1 || result.Skipped != 0 || time.Since(start) > time.Second {
			t.Fatalf("cancel sleep = %+v, %v", result, err)
		}
	})
	t.Run("embedding", func(t *testing.T) {
		deps, opts, c := reindexFixture(t, 1)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		c.embed = func(_ context.Context, text string) error {
			if text != "preflight" {
				cancel()
				return errors.New("wrapped provider cancellation")
			}
			return nil
		}
		result, err := RunReindexHeuristic(ctx, deps, opts)
		if !errors.Is(err, context.Canceled) || result.Skipped != 0 {
			t.Fatalf("cancel embed = %+v, %v", result, err)
		}
	})
}

func TestReindexHeuristicCLI_RateAndProgress(t *testing.T) {
	deps, opts, _ := reindexFixture(t, 10)
	opts.IntervalMS = 100
	start := time.Now()
	result, err := RunReindexHeuristic(context.Background(), deps, opts)
	if err != nil || result.Done != 10 || time.Since(start) < 900*time.Millisecond {
		t.Fatalf("rate = %+v, %v, elapsed %s", result, err, time.Since(start))
	}
	deps, opts, _ = reindexFixture(t, 51)
	var out bytes.Buffer
	opts.Output = &out
	result, err = RunReindexHeuristic(context.Background(), deps, opts)
	if err != nil || result.Done != 51 || !strings.Contains(out.String(), "progress done=50") || !strings.Contains(out.String(), "final done=51") {
		t.Fatalf("progress = %+v, %v, %s", result, err, &out)
	}
}

func TestReindexHeuristicCLI_Validation(t *testing.T) {
	for _, opts := range []ReindexHeuristicOptions{{IntervalMS: 300}, {Since: time.Now(), IntervalMS: 49}, {Since: time.Now(), IntervalMS: 5001}, {Since: time.Now(), IntervalMS: 300, MaxRows: -1}} {
		if _, err := RunReindexHeuristic(context.Background(), Deps{}, opts); err == nil {
			t.Fatalf("accepted %+v", opts)
		}
	}
}
