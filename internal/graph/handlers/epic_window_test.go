package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/bfs"
	"github.com/agent-mem/agent-mem/internal/graph/temporal"
)

func winExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec: %v\n%s", err, sql)
	}
}

// winNode inserts a node; created is "" for NULL created_at. first_seen_at is set explicitly.
func winNode(t *testing.T, pool *pgxpool.Pool, id, typ, created, firstSeen string) {
	t.Helper()
	var c any
	if created != "" {
		c = created
	}
	winExec(t, pool, `INSERT INTO graph.nodes (id, type, natural_key, title, metadata, machine_id, created_at, first_seen_at)
	  VALUES ($1,$2,$1,$1,'{}'::jsonb,'test',$3::timestamptz,$4::timestamptz)`, id, typ, c, firstSeen)
}

func winMap(t *testing.T, pool *pgxpool.Pool, issue, epic string) {
	t.Helper()
	winExec(t, pool, `INSERT INTO graph.jira_epic_map (issue_key, epic_key, epic_summary, machine_id) VALUES ($1,$2,'e','test')`, issue, epic)
}

func winRebuild(t *testing.T, pool *pgxpool.Pool, epics ...string) {
	t.Helper()
	m := map[string]int{}
	for _, e := range epics {
		m[e] = 0
	}
	if err := rebuildEpicHierarchy(context.Background(), pool, "test", "PAY", m); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
}

func winReset(t *testing.T) *pgxpool.Pool {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	t.Cleanup(func() { truncateGraphHandlerTables(t, pool) })
	return pool
}

func winWindow(t *testing.T, pool *pgxpool.Pool, nodeID, epicKey string) (first, last *time.Time) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT first_at, last_at FROM graph.epic_membership WHERE node_id=$1 AND epic_key=$2`, nodeID, epicKey).Scan(&first, &last)
	if err != nil {
		t.Fatalf("window row %s/%s: %v", nodeID, epicKey, err)
	}
	return
}

func TestEpicWindows_IgnoreStubTimes(t *testing.T) {
	today := time.Now().UTC().Format(time.RFC3339)
	d1, d2 := "2026-08-03T10:00:00Z", "2026-08-20T10:00:00Z"
	eq := func(t *testing.T, got *time.Time, want string) {
		t.Helper()
		if got == nil {
			t.Fatalf("got NULL, want %s", want)
		}
		if w, _ := time.Parse(time.RFC3339, want); !got.Equal(w) {
			t.Fatalf("got %s, want %s", got, want)
		}
	}

	t.Run("dated_members", func(t *testing.T) {
		pool := winReset(t)
		winNode(t, pool, "jira:PAY-100", "jira", "", today)
		winNode(t, pool, "jira:PAY-101", "jira", d1, today)
		winNode(t, pool, "jira:PAY-102", "jira", d2, today)
		winMap(t, pool, "PAY-100", "PAY-100")
		winMap(t, pool, "PAY-101", "PAY-100")
		winMap(t, pool, "PAY-102", "PAY-100")
		winRebuild(t, pool, "PAY-100")
		f, l := winWindow(t, pool, "jira:PAY-100", "PAY-100")
		eq(t, f, d1)
		eq(t, l, d2)
	})
	t.Run("undated_members", func(t *testing.T) {
		pool := winReset(t)
		winNode(t, pool, "jira:PAY-100", "jira", "", today)
		winNode(t, pool, "jira:PAY-101", "jira", "", today)
		winMap(t, pool, "PAY-100", "PAY-100")
		winMap(t, pool, "PAY-101", "PAY-100")
		winRebuild(t, pool, "PAY-100")
		f, l := winWindow(t, pool, "jira:PAY-100", "PAY-100")
		if f != nil || l != nil {
			t.Fatalf("want NULL window, got %v %v", f, l)
		}
	})
	t.Run("self_only", func(t *testing.T) {
		pool := winReset(t)
		winNode(t, pool, "jira:PAY-100", "jira", "", today)
		winMap(t, pool, "PAY-100", "PAY-100")
		winRebuild(t, pool, "PAY-100")
		f, l := winWindow(t, pool, "jira:PAY-100", "PAY-100")
		if f != nil || l != nil {
			t.Fatalf("want NULL window, got %v %v", f, l)
		}
	})
	t.Run("business_root_empty", func(t *testing.T) {
		pool := winReset(t)
		winNode(t, pool, businessRootID, "business", "", today)
		winRebuild(t, pool)
		f, l := winWindow(t, pool, businessRootID, businessRootID)
		if f != nil || l != nil {
			t.Fatalf("want NULL window, got %v %v", f, l)
		}
	})
	t.Run("business_root_dated", func(t *testing.T) {
		pool := winReset(t)
		winNode(t, pool, businessRootID, "business", "", today)
		winNode(t, pool, "jira:PAY-100", "jira", "", today)
		winNode(t, pool, "jira:PAY-101", "jira", d1, today)
		winNode(t, pool, "jira:PAY-102", "jira", d2, today)
		winMap(t, pool, "PAY-100", "PAY-100")
		winMap(t, pool, "PAY-101", "PAY-100")
		winMap(t, pool, "PAY-102", "PAY-100")
		winRebuild(t, pool, "PAY-100")
		f, l := winWindow(t, pool, businessRootID, businessRootID)
		eq(t, f, d1)
		eq(t, l, d2)
	})
}

func TestTemporalArm_UndatedStubEpic(t *testing.T) {
	w := temporal.Window{
		Start: time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC),
	}
	const day = "2026-10-03T12:00:00Z"
	run := func(t *testing.T, pool *pgxpool.Pool) map[string]bool {
		t.Helper()
		hits, err := temporalArm(context.Background(), pool, bfs.NewExpander(pool), w, nil, searchFilter{})
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, h := range hits {
			got[h.ID] = true
		}
		return got
	}

	t.Run("undated_stub", func(t *testing.T) {
		pool := winReset(t)
		winNode(t, pool, "jira:PAY-100", "jira", "", day)
		winMap(t, pool, "PAY-100", "PAY-100")
		winRebuild(t, pool, "PAY-100")
		if got := run(t, pool); got["jira:PAY-100"] {
			t.Fatalf("stub epic returned: %v", got)
		}
	})
	t.Run("dated_member", func(t *testing.T) {
		pool := winReset(t)
		winNode(t, pool, "jira:PAY-100", "jira", "", day)
		winNode(t, pool, "jira:PAY-101", "jira", day, day)
		winMap(t, pool, "PAY-100", "PAY-100")
		winMap(t, pool, "PAY-101", "PAY-100")
		winRebuild(t, pool, "PAY-100")
		got := run(t, pool)
		if !got["jira:PAY-100"] || !got["jira:PAY-101"] {
			t.Fatalf("want epic and member, got %v", got)
		}
	})
	t.Run("root_dated", func(t *testing.T) {
		pool := winReset(t)
		winNode(t, pool, businessRootID, "business", "", day)
		winNode(t, pool, "jira:PAY-100", "jira", "", "2026-01-01T00:00:00Z")
		winNode(t, pool, "jira:PAY-101", "jira", day, "2026-01-01T00:00:00Z")
		winMap(t, pool, "PAY-100", "PAY-100")
		winMap(t, pool, "PAY-101", "PAY-100")
		winRebuild(t, pool, "PAY-100")
		if got := run(t, pool); !got[businessRootID] {
			t.Fatalf("root not returned: %v", got)
		}
	})
	t.Run("root_empty", func(t *testing.T) {
		pool := winReset(t)
		winNode(t, pool, businessRootID, "business", "", day)
		winRebuild(t, pool)
		if got := run(t, pool); got[businessRootID] {
			t.Fatalf("empty root returned: %v", got)
		}
	})
	t.Run("expansion", func(t *testing.T) {
		pool := winReset(t)
		winNode(t, pool, "jira:PAY-100", "jira", "", day)
		winNode(t, pool, "jira:PAY-101", "jira", "", day)
		winMap(t, pool, "PAY-100", "PAY-100")
		winMap(t, pool, "PAY-101", "PAY-100")
		winRebuild(t, pool, "PAY-100")
		got := run(t, pool)
		if got["jira:PAY-100"] {
			t.Fatalf("stub epic returned via expansion: %v", got)
		}
	})
	t.Run("root_leak", func(t *testing.T) {
		pool := winReset(t)
		winNode(t, pool, businessRootID, "business", "", day)
		winNode(t, pool, "jira:PAY-100", "jira", "", day)
		winNode(t, pool, "jira:PAY-101", "jira", "2026-01-05T00:00:00Z", day)
		winNode(t, pool, "jira:PAY-200", "jira", "", day)
		winNode(t, pool, "jira:PAY-201", "jira", day, day)
		winMap(t, pool, "PAY-100", "PAY-100")
		winMap(t, pool, "PAY-101", "PAY-100")
		winMap(t, pool, "PAY-200", "PAY-200")
		winMap(t, pool, "PAY-201", "PAY-200")
		winRebuild(t, pool, "PAY-100", "PAY-200")
		got := run(t, pool)
		if got["jira:PAY-100"] || got["jira:PAY-101"] {
			t.Fatalf("business-root membership leaked undated/out-of-window nodes: %v", got)
		}
		if !got["jira:PAY-201"] {
			t.Fatalf("in-window member missing: %v", got)
		}
	})
}
