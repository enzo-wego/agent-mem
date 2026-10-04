package handlers

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func seedKeywordNode(t *testing.T, pool *pgxpool.Pool, id, title, scope, summary string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO graph.nodes (id, type, natural_key, title, scope, metadata, machine_id)
		VALUES ($1, 'jira', $1, $2, NULLIF($3, ''), '{}', 'test')`, id, title, scope); err != nil {
		t.Fatalf("seed node %s: %v", id, err)
	}
	if summary != "" {
		if _, err := pool.Exec(ctx, `INSERT INTO graph.artifact_index (node_id, summary, summary_kind, machine_id)
			VALUES ($1, $2, 'heuristic', 'test')`, id, summary); err != nil {
			t.Fatalf("seed index %s: %v", id, err)
		}
	}
}

func TestKeywordArm_Scoring(t *testing.T) {
	pool := openTestDB(t)
	ctx := context.Background()
	unfiltered := searchFilter{}

	scores := func(t *testing.T, q string, f searchFilter) map[string]float64 {
		t.Helper()
		hits, err := keywordArm(ctx, pool, q, f, 30)
		if err != nil {
			t.Fatalf("keywordArm: %v", err)
		}
		got := map[string]float64{}
		for _, h := range hits {
			if _, dup := got[h.ID]; dup {
				t.Errorf("%s returned twice", h.ID)
			}
			got[h.ID] = h.Score
		}
		return got
	}
	rank := func(t *testing.T, id, q string) float64 {
		t.Helper()
		var r float64
		if err := pool.QueryRow(ctx, `SELECT ts_rank_cd(tsv, websearch_to_tsquery('simple', $2)) FROM graph.artifact_index WHERE node_id = $1`, id, q).Scan(&r); err != nil {
			t.Fatalf("ts_rank_cd: %v", err)
		}
		return r
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

	t.Run("title_only", func(t *testing.T) {
		truncateGraphHandlerTables(t, pool)
		seedKeywordNode(t, pool, "jira:T-1", "zebra rollout", "", "unrelated summary")
		got := scores(t, "zebra", unfiltered)
		if s, ok := got["jira:T-1"]; !ok || !near(s, 0.5) || len(got) != 1 {
			t.Fatalf("got %v, want only jira:T-1 at 0.5", got)
		}
	})

	t.Run("text_only", func(t *testing.T) {
		truncateGraphHandlerTables(t, pool)
		seedKeywordNode(t, pool, "jira:T-2", "plain title", "", "quokka habitat notes")
		got := scores(t, "quokka", unfiltered)
		want := rank(t, "jira:T-2", "quokka")
		if s, ok := got["jira:T-2"]; !ok || want <= 0 || !near(s, want) || len(got) != 1 {
			t.Fatalf("got %v, want only jira:T-2 at ts_rank_cd %v", got, want)
		}
	})

	t.Run("both", func(t *testing.T) {
		truncateGraphHandlerTables(t, pool)
		seedKeywordNode(t, pool, "jira:T-3", "okapi diet", "", "okapi eats leaves")
		got := scores(t, "okapi", unfiltered)
		want := rank(t, "jira:T-3", "okapi") + 0.5
		if s, ok := got["jira:T-3"]; !ok || !near(s, want) || len(got) != 1 {
			t.Fatalf("got %v, want jira:T-3 once at %v", got, want)
		}
	})

	t.Run("body_only", func(t *testing.T) {
		truncateGraphHandlerTables(t, pool)
		seedKeywordNode(t, pool, "jira:B-body", "plain one", "", "")
		if _, err := pool.Exec(ctx, `UPDATE graph.nodes SET body = 'the narwhal appears only in this body' WHERE id = 'jira:B-body'`); err != nil {
			t.Fatal(err)
		}
		seedKeywordNode(t, pool, "jira:B-sum", "plain two", "", "narwhal summary mention")
		hits, err := keywordArm(ctx, pool, "narwhal", unfiltered, 30)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 2 || hits[0].ID != "jira:B-sum" || hits[1].ID != "jira:B-body" || hits[1].Score != 0 {
			t.Fatalf("hits = %+v, want summary node first, then body-only node at score 0", hits)
		}
	})

	t.Run("acl_filtered", func(t *testing.T) {
		truncateGraphHandlerTables(t, pool)
		seedKeywordNode(t, pool, "jira:OK-title", "gnu migration", "slack:OK", "")
		seedKeywordNode(t, pool, "jira:OK-text", "plain", "slack:OK", "gnu herd notes")
		seedKeywordNode(t, pool, "jira:NO-title", "gnu secret plan", "slack:SECRET", "")
		seedKeywordNode(t, pool, "jira:NO-text", "plain two", "slack:SECRET", "gnu secret notes")
		got := scores(t, "gnu", searchFilter{scope: []string{"slack:OK"}})
		if len(got) != 2 {
			t.Fatalf("got %v, want exactly the two slack:OK nodes", got)
		}
		for _, id := range []string{"jira:OK-title", "jira:OK-text"} {
			if _, ok := got[id]; !ok {
				t.Errorf("missing %s in %v", id, got)
			}
		}
	})

	t.Run("literal_percent", func(t *testing.T) {
		truncateGraphHandlerTables(t, pool)
		seedKeywordNode(t, pool, "jira:P-1", "50% off", "", "")
		seedKeywordNode(t, pool, "jira:P-2", "500 off", "", "")
		got := scores(t, "50%", unfiltered)
		if _, ok := got["jira:P-1"]; !ok || len(got) != 1 {
			t.Fatalf("got %v, want only jira:P-1 (a literal %%, not a wildcard)", got)
		}
	})
}

func TestKeywordArm_UsesGINIndex(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `INSERT INTO graph.nodes (id, type, natural_key, title, body, metadata, machine_id)
		SELECT 'fill:' || i, 'jira', 'fill:' || i, 'filler title ' || i,
		       CASE WHEN i <= 3 THEN 'body zqxjtoken marker' ELSE 'filler body number ' || i END, '{}', 'test'
		FROM generate_series(1, 5000) i`); err != nil {
		t.Fatalf("seed nodes: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO graph.artifact_index (node_id, summary, summary_kind, machine_id)
		SELECT 'fill:' || i,
		       CASE WHEN i <= 3 THEN 'zqxjtoken unique marker' ELSE 'filler text number ' || i END,
		       'heuristic', 'test'
		FROM generate_series(1, 5000) i`); err != nil {
		t.Fatalf("seed index: %v", err)
	}
	// Rows inserted after the index sit in the GIN pending list, which the
	// planner prices as extra pages to scan; flush it so the plan reflects a
	// settled index (autovacuum does this in production).
	if _, err := pool.Exec(ctx, `SELECT gin_clean_pending_list('graph.idx_artifact_index_tsv'::regclass)`); err != nil {
		t.Fatalf("flush pending list: %v", err)
	}
	if _, err := pool.Exec(ctx, `SELECT gin_clean_pending_list('graph.idx_nodes_body_tsv'::regclass)`); err != nil {
		t.Fatalf("flush body pending list: %v", err)
	}
	for _, q := range []string{`ANALYZE graph.artifact_index`, `ANALYZE graph.nodes`} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	f := searchFilter{}
	rows, err := pool.Query(ctx, "EXPLAIN "+keywordArmSQL(f), keywordArmArgs("zqxjtoken", f, 30)...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	text := strings.Join(plan, "\n")
	t.Log("\n" + text)
	for _, idx := range []string{"idx_artifact_index_tsv", "idx_nodes_body_tsv"} {
		if !strings.Contains(text, "Bitmap Index Scan on "+idx) {
			t.Fatalf("plan has no bitmap index scan on %s", idx)
		}
	}
}

func TestKeywordArm_FoldOrder(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	ctx := context.Background()
	// Six threads, budget three. Thread i's root summary repeats the term i
	// times, so lexicographically earlier thread ids score lower; each thread
	// also has a reply that matches only by body (score 0).
	for i := 1; i <= 6; i++ {
		root := fmt.Sprintf("slack:CFO:%d.000001", i)
		reply := fmt.Sprintf("slack:CFO:%d.000002", i)
		for _, id := range []string{root, reply} {
			if _, err := pool.Exec(ctx, `INSERT INTO graph.nodes (id, type, natural_key, body, scope, metadata, machine_id)
				VALUES ($1, 'slack', $1, 'flaxseed body', 'slack:CFO', jsonb_build_object('thread_ts', $2::text), 'test')`,
				id, fmt.Sprintf("%d.000001", i)); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := pool.Exec(ctx, `INSERT INTO graph.artifact_index (node_id, summary, summary_kind, machine_id)
			VALUES ($1, repeat('flaxseed filler ', $2::int), 'heuristic', 'test')`, root, i); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := keywordArmFolded(ctx, pool, "flaxseed", searchFilter{}, 3)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, h := range hits {
		got = append(got, h.Key)
		if h.ID != h.Key {
			t.Errorf("best member of %s = %s, want the root (highest score)", h.Key, h.ID)
		}
	}
	want := []string{"slack:CFO:6.000001", "slack:CFO:5.000001", "slack:CFO:4.000001"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("threads = %v, want the three highest-scoring %v in rank order (scores %+v)", got, want, hits)
	}
}
