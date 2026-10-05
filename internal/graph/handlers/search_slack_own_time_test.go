package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/bfs"
	"github.com/agent-mem/agent-mem/internal/graph/temporal"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
)

const sotBounds = "&since=2026-08-01&until=2026-08-31"
const sotInside = "2026-08-15T00:00:00+07:00"
const sotOutside = "2026-05-06T00:00:00+07:00"

type sotCase struct {
	id, typ, created, firstSeen, epic string
	want                              bool
}

var sotCases = []sotCase{
	{"slack:CSOT:1778029200.000001", "slack", sotOutside, sotInside, "EPIC-1", false},
	{"jira:OLD-1", "jira", sotOutside, sotInside, "EPIC-1", true},
	{"slack:CSOT:1785517200.000001", "slack", "2026-08-01T00:00:00+07:00", sotInside, "EPIC-1", true},
	{"slack:CSOT:1788195600.000001", "slack", "2026-09-01T00:00:00+07:00", sotInside, "EPIC-1", false},
	{"slack:CSOT:1786726800.000001", "slack", sotInside, sotInside, "EPIC-1", true},
	{"slack:CSOT:1786726800.000002", "slack", sotInside, sotInside, "", true},
	{"slack:CSOT:1786294800.000001", "slack", "", "2026-08-10T00:00:00+07:00", "EPIC-1", true},
	{"slack:CSOT:1783616400.000001", "slack", "", "2026-07-10T00:00:00+07:00", "EPIC-1", false},
	{"slack:CSOT:1783616400.000002", "slack", "2026-07-10T00:00:00+07:00", "2026-08-10T00:00:00+07:00", "EPIC-1", false},
	{"slack:CSOT:1778029200.000002", "slack", sotOutside, sotInside, "EPIC-2", false},
	{"jira:OLD-2", "jira", sotOutside, sotInside, "EPIC-2", false},
}

func sotNode(t *testing.T, db *pgxpool.Pool, f sotCase) {
	t.Helper()
	winNode(t, db, f.id, f.typ, f.created, f.firstSeen)
	winExec(t, db, `UPDATE graph.nodes SET title='needle' WHERE id=$1`, f.id)
	v, _ := (whfEmbedder{}).Embed(context.Background(), "")
	winExec(t, db, `INSERT INTO graph.artifact_index(node_id,summary,summary_kind,embedding,machine_id) VALUES($1,'needle','heuristic',$2,'test')`, f.id, pgvector.NewVector(v))
	if f.epic != "" {
		winExec(t, db, `INSERT INTO graph.epic_membership(node_id,epic_key,via,confidence,first_at,last_at) VALUES($1,$2,'epic_self',1,$3::timestamptz,$3::timestamptz)`, f.id, f.epic, sotOutside)
	}
}

func sotFixture(t *testing.T, db *pgxpool.Pool) []string {
	t.Helper()
	var ids []string
	for _, e := range []struct{ key, at string }{{"EPIC-1", sotInside}, {"EPIC-2", sotOutside}} {
		id := "jira:" + e.key
		sotNode(t, db, sotCase{id: id, typ: "jira", created: sotOutside, firstSeen: sotOutside})
		winExec(t, db, `INSERT INTO graph.epic_membership(node_id,epic_key,via,confidence,first_at,last_at) VALUES($1,$2,'epic_self',1,$3::timestamptz,$3::timestamptz)`, id, e.key, e.at)
		ids = append(ids, id)
	}
	for _, f := range sotCases {
		sotNode(t, db, f)
		ids = append(ids, f.id)
	}
	return ids
}

func TestSlackOwnTime_Handler(t *testing.T) {
	for _, mode := range []string{"default", "hybrid"} {
		for _, arm := range []string{"semantic", "keyword"} {
			t.Run(mode+"/"+arm, func(t *testing.T) {
				db := winReset(t)
				sotFixture(t, db)
				s := whfSearch(t, db, true)
				q := "q=needle&limit=50"
				if mode == "hybrid" {
					q += "&match=hybrid"
				} else {
					q += "&arms=" + arm
				}
				control := whfRequest(t, s, q)
				for _, f := range sotCases {
					whfExpect(t, control, f.id, true)
					ranked := false
					for _, n := range control.Results {
						if (n.NodeID == f.id || n.ID == f.id) && n.ScoreBreakdown.Ranks[arm] > 0 {
							ranked = true
						}
					}
					if !ranked {
						t.Fatalf("control %s missing %s rank", f.id, arm)
					}
				}
				r := whfRequest(t, s, q+sotBounds)
				for _, f := range sotCases {
					whfExpect(t, r, f.id, f.want)
				}
			})
		}
	}
}

func sotWindow(t *testing.T, start, end string) temporal.Window {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Ho_Chi_Minh")
	if err != nil {
		t.Fatal(err)
	}
	a, err := time.ParseInLocation("2006-01-02", start, loc)
	if err != nil {
		t.Fatal(err)
	}
	b, err := time.ParseInLocation("2006-01-02", end, loc)
	if err != nil {
		t.Fatal(err)
	}
	return temporal.Window{Start: a, End: b}
}

func TestSlackOwnTime_TemporalArmAndParity(t *testing.T) {
	db := winReset(t)
	ids := sotFixture(t, db)
	ctx := context.Background()
	exp := bfs.NewExpander(db)
	control, err := temporalArm(ctx, db, exp, sotWindow(t, "2026-05-01", "2026-06-01"), nil, searchFilter{})
	if err != nil {
		t.Fatal(err)
	}
	has := func(hits []armHit, id string) bool {
		for _, h := range hits {
			if h.ID == id {
				return true
			}
		}
		return false
	}
	if !has(control, sotCases[0].id) {
		t.Fatal("May control missing outside Slack member")
	}
	w := sotWindow(t, "2026-08-01", "2026-09-01")
	hits, err := temporalArm(ctx, db, exp, w, nil, searchFilter{})
	if err != nil {
		t.Fatal(err)
	}
	eligible, err := nodesEligibleInWindow(ctx, db, ids, w)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if !eligible[h.ID] {
			t.Errorf("temporal id %s missing from eligibility", h.ID)
		}
	}
	for _, f := range sotCases {
		if got := has(hits, f.id); got != f.want {
			t.Errorf("temporal node %s present=%t, want %t", f.id, got, f.want)
		}
		if got := eligible[f.id]; got != f.want {
			t.Errorf("eligible node %s present=%t, want %t", f.id, got, f.want)
		}
	}
}
