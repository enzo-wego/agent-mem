package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/bfs"
	"github.com/agent-mem/agent-mem/internal/graph/temporal"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Capture the statement actually executed by temporalArm, rather than testing
// a second copy of its SQL. Expansion queries must not replace this capture.
type temporalQueryCapture struct {
	sql  string
	args []any
}

func (c *temporalQueryCapture) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if c.sql == "" {
		c.sql = d.SQL
		c.args = append([]any(nil), d.Args...)
	}
	return ctx
}
func (*temporalQueryCapture) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func temporalCapturePool(t *testing.T, db *pgxpool.Pool) (*pgxpool.Pool, *temporalQueryCapture) {
	t.Helper()
	cfg := db.Config()
	capture := &temporalQueryCapture{}
	cfg.ConnConfig.Tracer = capture
	cfg.ConnConfig.RuntimeParams["work_mem"] = "64kB"
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, capture
}

func temporalEpicWindow() temporal.Window {
	return temporal.Window{Start: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
}

func TestTemporalArm_EpicInheritanceEquivalence(t *testing.T) {
	db := winReset(t)
	const outside = "2026-01-01T00:00:00Z"
	const inside = "2026-08-12T00:00:00Z"
	const start = "2026-08-01T00:00:00Z"
	const end = "2026-09-01T00:00:00Z"
	var ids, want []string
	add := func(id string, included bool, event string) {
		// Distinct event times keep every fixture below LIMIT without ranking ties.
		stamp, err := time.Parse(time.RFC3339, event)
		if err != nil {
			t.Fatal(err)
		}
		stamp = stamp.Add(time.Duration(len(ids)) * time.Second)
		winNode(t, db, id, "jira", stamp.Format(time.RFC3339), outside)
		ids = append(ids, id)
		if included {
			want = append(want, id)
		}
	}
	member := func(id, key string, first, last any) {
		winExec(t, db, `INSERT INTO graph.epic_membership (node_id,epic_key,via,confidence,first_at,last_at) VALUES ($1,$2,'epic_self',1,$3::timestamptz,$4::timestamptz)`, id, key, first, last)
	}
	epics := []struct {
		key         string
		first, last any
		included    bool
	}{
		{"ACTIVE", inside, inside, true}, {"INACTIVE", outside, outside, false},
		{"NULL-FIRST", nil, inside, false}, {"NULL-LAST", inside, nil, false}, {"NULL-BOTH", nil, nil, false},
		{"END", end, end, false}, {"START", outside, start, true},
	}
	for _, e := range epics {
		add("jira:"+e.key, e.included, outside)
		member("jira:"+e.key, e.key, e.first, e.last)
		id := "member:" + e.key
		add(id, e.included, outside)
		// Member-local bounds must not grant inheritance from an inactive self row.
		member(id, e.key, inside, inside)
	}
	add(businessRootID, true, outside)
	member(businessRootID, businessRootID, inside, inside)
	add("root-only", false, outside)
	member("root-only", businessRootID, inside, inside)
	add("missing-self", false, outside)
	member("missing-self", "MISSING", inside, inside)
	add("ordinary", true, inside)
	add("own-time", true, inside)
	member("own-time", "INACTIVE", outside, outside)
	add("several", true, outside)
	member("several", "ACTIVE", nil, nil)
	member("several", "START", nil, nil)
	// An otherwise inactive epic self node may inherit another epic's window.
	member("jira:INACTIVE", "ACTIVE", nil, nil)
	want = append(want, "jira:INACTIVE")
	add("jira:STUB", false, inside)
	member("jira:STUB", "STUB", nil, nil)
	hits, err := temporalArm(context.Background(), db, bfs.NewExpander(db), temporalEpicWindow(), nil, searchFilter{})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(hits))
	for _, h := range hits {
		got = append(got, h.ID)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("temporal ids = %v; want %v", got, want)
	}
	direct, err := nodesInWindow(context.Background(), db, []string{"member:ACTIVE", "jira:ACTIVE", "jira:STUB"}, temporalEpicWindow(), searchFilter{})
	if err != nil {
		t.Fatal(err)
	}
	directIDs := make([]string, 0, len(direct))
	for id := range direct {
		directIDs = append(directIDs, id)
	}
	sort.Strings(directIDs)
	if !reflect.DeepEqual(directIDs, []string{"jira:ACTIVE"}) {
		t.Fatalf("direct-only ids = %v", directIDs)
	}
}

type temporalExplainNode struct {
	NodeType string                `json:"Node Type"`
	Subplan  string                `json:"Subplan Name"`
	Relation string                `json:"Relation Name"`
	Alias    string                `json:"Alias"`
	Loops    int                   `json:"Actual Loops"`
	Plans    []temporalExplainNode `json:"Plans"`
}

func TestTemporalArm_ActiveEpicPlanOnce(t *testing.T) {
	db := openTestDB(t)
	truncateGraphHandlerTables(t, db)
	t.Cleanup(func() {
		truncateGraphHandlerTables(t, db)
		// Remove dead HNSW entries and restore statistics before later search
		// tests: DELETE alone leaves this large fixture visible to the planner.
		for _, table := range []string{"graph.artifact_index", "graph.nodes", "graph.epic_membership"} {
			winExec(t, db, "VACUUM (ANALYZE) "+table)
		}
	})
	// 5,000 candidate nodes plus 50 epic self nodes; exactly 20,000 memberships.
	winExec(t, db, `INSERT INTO graph.nodes (id,type,natural_key,title,metadata,machine_id,created_at,first_seen_at)
 SELECT 'plan:'||i,'jira','plan:'||i,'plan','{}','test','2026-01-01'::timestamptz + i*interval '1 second','2026-01-01' FROM generate_series(1,5000) i`)
	winExec(t, db, `INSERT INTO graph.nodes (id,type,natural_key,title,metadata,machine_id,created_at,first_seen_at)
 SELECT 'jira:PLAN-'||i,'jira','jira:PLAN-'||i,'epic','{}','test','2026-01-01','2026-01-01' FROM generate_series(0,49) i`)
	winExec(t, db, `INSERT INTO graph.epic_membership (node_id,epic_key,via,confidence,first_at,last_at)
 SELECT 'jira:PLAN-'||i,'PLAN-'||i,'epic_self',1,'2026-08-02','2026-08-20' FROM generate_series(0,49) i`)
	winExec(t, db, `INSERT INTO graph.epic_membership (node_id,epic_key,via,confidence,first_at,last_at)
 SELECT 'plan:'||i,'PLAN-'||((i+j)%50),'key',1,'2026-08-02','2026-08-20'
 FROM generate_series(1,5000) i CROSS JOIN generate_series(0,3) j WHERE NOT (i<=50 AND j=3)`)
	var count int
	if err := db.QueryRow(context.Background(), `SELECT count(*) FROM graph.epic_membership`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 20000 {
		t.Fatalf("membership count = %d", count)
	}
	winExec(t, db, `INSERT INTO graph.artifact_index (node_id,summary,summary_kind,embedding,machine_id)
 SELECT id,'plan','heuristic',array_fill(0.1::real, ARRAY[$1::int])::halfvec,'test' FROM graph.nodes`, GraphEmbeddingDims)
	winExec(t, db, `ANALYZE graph.nodes; ANALYZE graph.epic_membership; ANALYZE graph.artifact_index`)
	// At minimum supported work_mem the old node-id hash cannot fit, while the
	// 50-key active-epic array still evaluates once. This exposes the correlated
	// fallback rather than letting PostgreSQL hide it behind an alternative plan.
	for _, withVector := range []bool{false, true} {
		t.Run(fmt.Sprintf("vector=%t", withVector), func(t *testing.T) {
			pool, capture := temporalCapturePool(t, db)
			var vec []float32
			if withVector {
				vec = make([]float32, GraphEmbeddingDims)
				vec[0] = 1
			}
			if _, err := temporalArm(context.Background(), pool, bfs.NewExpander(pool), temporalEpicWindow(), vec, searchFilter{}); err != nil {
				t.Fatal(err)
			}
			var raw []byte
			if err := pool.QueryRow(context.Background(), "EXPLAIN (ANALYZE, FORMAT JSON) "+capture.sql, capture.args...).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var result []struct {
				Plan          temporalExplainNode
				ExecutionTime float64 `json:"Execution Time"`
			}
			if err := json.Unmarshal(raw, &result); err != nil {
				t.Fatal(err)
			}
			if len(result) != 1 {
				t.Fatalf("explain result count = %d", len(result))
			}
			found := 0
			var walk func(temporalExplainNode, string, int)
			walk = func(n temporalExplainNode, owner string, loops int) {
				if strings.HasPrefix(n.Subplan, "InitPlan") || strings.HasPrefix(n.Subplan, "SubPlan") {
					owner = n.Subplan
					loops = n.Loops
				}
				if n.Relation == "epic_membership" && n.Alias == "ep" {
					found++
					t.Logf("active-epic subplan: %s loops=%d; scan=%s loops=%d; execution=%.3f ms", owner, loops, n.NodeType, n.Loops, result[0].ExecutionTime)
					if owner == "" || loops != 1 {
						t.Errorf("active-epic subplan %q loops=%d, want 1", owner, loops)
					}
					if n.Loops != 1 {
						t.Errorf("active-epic scan loops=%d, want 1", n.Loops)
					}
				}
				for _, child := range n.Plans {
					walk(child, owner, loops)
				}
			}
			walk(result[0].Plan, "", 0)
			if found != 1 {
				t.Fatalf("found %d active-epic scans, want 1", found)
			}
		})
	}
}
