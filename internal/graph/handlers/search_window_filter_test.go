package handlers

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/gemini"
	"github.com/agent-mem/agent-mem/internal/graph/bfs"
	"github.com/agent-mem/agent-mem/internal/graph/temporal"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	"strings"
)

const whfBounds = "&since=2026-09-28&until=2026-10-05"
const whfInside = "2026-09-30T12:00:00+07:00"
const whfOutside = "2026-01-01T00:00:00Z"

type whfEmbedder struct{}

func (whfEmbedder) Embed(context.Context, string) ([]float32, error) {
	v := make([]float32, GraphEmbeddingDims)
	v[0] = 1
	return v, nil
}
func (e whfEmbedder) EmbedWithOptions(ctx context.Context, q string, _ gemini.EmbedOptions) ([]float32, error) {
	return e.Embed(ctx, q)
}

func whfNode(t *testing.T, db *pgxpool.Pool, id, at, title string, semantic bool) {
	t.Helper()
	winNode(t, db, id, "jira", at, whfInside)
	winExec(t, db, `UPDATE graph.nodes SET title=$2 WHERE id=$1`, id, title)
	if semantic {
		v, _ := (whfEmbedder{}).Embed(context.Background(), "")
		winExec(t, db, `INSERT INTO graph.artifact_index(node_id,summary,summary_kind,embedding,machine_id) VALUES($1,'unrelated','heuristic',$2,'test')`, id, pgvector.NewVector(v))
	}
}
func whfSearch(t *testing.T, db *pgxpool.Pool, semantic bool) *Search {
	t.Helper()
	s, err := NewSearch(db)
	if err != nil {
		t.Fatal(err)
	}
	if semantic {
		s.embed = whfEmbedder{}
	}
	s.now = func() time.Time { return time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC) }
	var previous string
	err = db.QueryRow(context.Background(), `SELECT value FROM settings WHERE key='graph.temporal.timezone'`).Scan(&previous)
	if err != nil && err != pgx.ErrNoRows {
		t.Fatal(err)
	}
	if previous != "Asia/Ho_Chi_Minh" {
		had := err == nil
		t.Cleanup(func() {
			if had {
				winExec(t, db, `UPDATE settings SET value=$1 WHERE key='graph.temporal.timezone'`, previous)
			} else {
				winExec(t, db, `DELETE FROM settings WHERE key='graph.temporal.timezone'`)
			}
		})
	}
	winExec(t, db, `INSERT INTO settings(key,value) VALUES('graph.temporal.timezone','Asia/Ho_Chi_Minh') ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`)
	return s
}

type whfResponse struct {
	Results []struct {
		NodeID         string `json:"node_id"`
		ID             string `json:"id"`
		ScoreBreakdown struct {
			Ranks map[string]int `json:"ranks"`
		} `json:"score_breakdown"`
	} `json:"results"`
	Window map[string]any `json:"window"`
}

func whfRequest(t *testing.T, s *Search, q string) whfResponse {
	t.Helper()
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/api/graph/search?"+q, nil))
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var r whfResponse
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	return r
}
func whfHas(r whfResponse, id string) bool {
	for _, n := range r.Results {
		if n.NodeID == id || n.ID == id {
			return true
		}
	}
	return false
}
func whfExpect(t *testing.T, r whfResponse, id string, want bool) {
	t.Helper()
	if got := whfHas(r, id); got != want {
		t.Errorf("node %s present=%t, want %t", id, got, want)
	}
}
func whfHard(t *testing.T, r whfResponse, want bool) {
	t.Helper()
	v, ok := r.Window["hard"]
	if want {
		if v != true {
			t.Errorf("window.hard=%v, want true", v)
		}
	} else if ok {
		t.Errorf("window.hard must be absent, got %v", v)
	}
}

func TestWindowFilter_Arms(t *testing.T) {
	for _, mode := range []string{"default", "hybrid"} {
		for _, arm := range []string{"semantic", "keyword"} {
			t.Run(mode+"/"+arm, func(t *testing.T) {
				db := winReset(t)
				semantic := arm == "semantic"
				title := "unrelated"
				if !semantic {
					title = "needle"
				}
				whfNode(t, db, "ordinary:outside", whfOutside, title, semantic)
				whfNode(t, db, "ordinary:inside", whfInside, title, semantic)
				s := whfSearch(t, db, semantic)
				q := "q=needle&limit=50"
				if mode == "hybrid" {
					q += "&match=hybrid"
				} else {
					q += "&arms=" + arm
				}
				control := whfRequest(t, s, q)
				whfExpect(t, control, "ordinary:outside", true)
				whfExpect(t, control, "ordinary:inside", true)
				r := whfRequest(t, s, q+whfBounds)
				whfExpect(t, r, "ordinary:outside", false)
				whfExpect(t, r, "ordinary:inside", true)
				whfHard(t, r, true)
			})
		}
	}
}
func TestWindowFilter_GraphNeighbour(t *testing.T) {
	db := winReset(t)
	whfNode(t, db, "seed", whfInside, "unrelated", true)
	whfNode(t, db, "neighbour", whfOutside, "unrelated", false)
	winExec(t, db, `INSERT INTO graph.edges(from_node_id,to_node_id,kind,machine_id) VALUES('seed','neighbour','REFERENCES','test')`)
	s := whfSearch(t, db, true)
	q := "q=needle&arms=semantic,graph"
	control := whfRequest(t, s, q)
	whfExpect(t, control, "neighbour", true)
	for _, r := range control.Results {
		if r.NodeID == "neighbour" && (r.ScoreBreakdown.Ranks["graph"] == 0 || len(r.ScoreBreakdown.Ranks) != 1) {
			t.Fatalf("neighbour not graph-only: %v", r.ScoreBreakdown.Ranks)
		}
	}
	whfExpect(t, whfRequest(t, s, q+whfBounds), "neighbour", false)
}
func TestWindowFilter_Boundaries(t *testing.T) {
	db := winReset(t)
	s := whfSearch(t, db, true)
	fixtures := []struct {
		id, at string
		want   bool
	}{{"start", "2026-09-28T00:00:00+07:00", true}, {"last-second", "2026-10-05T23:59:59+07:00", true}, {"end", "2026-10-06T00:00:00+07:00", false}, {"fallback", "", true}}
	for _, f := range fixtures {
		whfNode(t, db, f.id, f.at, "unrelated", true)
	}
	q := "q=needle&arms=semantic&limit=50"
	control := whfRequest(t, s, q)
	r := whfRequest(t, s, q+whfBounds)
	for _, f := range fixtures {
		whfExpect(t, control, f.id, true)
		whfExpect(t, r, f.id, f.want)
	}
}
func TestWindowFilter_OpenBounds(t *testing.T) {
	for _, bound := range []string{"since", "until"} {
		t.Run(bound, func(t *testing.T) {
			db := winReset(t)
			s := whfSearch(t, db, true)
			fixtures := []struct {
				id, at       string
				since, until bool
			}{{"before-epoch", "1969-12-31T23:59:59Z", false, false}, {"old", "2020-01-01T00:00:00Z", false, true}, {"yesterday", "2026-10-04T12:00:00Z", true, true}, {"future", "2026-10-07T12:00:00Z", false, false}}
			for _, f := range fixtures {
				whfNode(t, db, f.id, f.at, "unrelated", true)
			}
			q := "q=needle&arms=semantic&limit=50"
			c := whfRequest(t, s, q)
			value := "2026-09-28"
			if bound == "until" {
				value = "2026-10-05"
			}
			r := whfRequest(t, s, q+"&"+bound+"="+value)
			for _, f := range fixtures {
				whfExpect(t, c, f.id, true)
				want := f.since
				if bound == "until" {
					want = f.until
				}
				whfExpect(t, r, f.id, want)
			}
		})
	}
}
func TestWindowFilter_PhraseSoft(t *testing.T) {
	db := winReset(t)
	whfNode(t, db, "outside", whfOutside, "unrelated", true)
	s := whfSearch(t, db, true)
	for _, suffix := range []string{"", "&since=", "&since=+++&until="} {
		r := whfRequest(t, s, "q="+url.QueryEscape("needle last week")+"&arms=semantic"+suffix)
		whfExpect(t, r, "outside", true)
		whfHard(t, r, false)
	}
}
func TestWindowFilter_PinExemption(t *testing.T) {
	for _, mode := range []string{"", "&match=hybrid"} {
		t.Run(mode, func(t *testing.T) {
			db := winReset(t)
			whfNode(t, db, "jira:PAY-1", whfOutside, "PAY-1", true)
			s := whfSearch(t, db, true)
			r := whfRequest(t, s, "q=PAY-1"+mode+whfBounds)
			whfExpect(t, r, "jira:PAY-1", true)
			if len(r.Results) == 0 || (r.Results[0].NodeID != "jira:PAY-1" && r.Results[0].ID != "jira:PAY-1") {
				t.Fatal("pin not rank 1")
			}
		})
	}
}
func TestWindowFilter_EmptyMarker(t *testing.T) {
	for _, mode := range []string{"&arms=semantic", "&match=hybrid"} {
		t.Run(mode, func(t *testing.T) {
			db := winReset(t)
			whfNode(t, db, "outside", whfOutside, "unrelated", true)
			s := whfSearch(t, db, true)
			q := "q=needle" + mode
			whfExpect(t, whfRequest(t, s, q), "outside", true)
			r := whfRequest(t, s, q+whfBounds)
			whfExpect(t, r, "outside", false)
			if len(r.Results) != 0 {
				t.Errorf("want empty results, got %v", r.Results)
			}
			whfHard(t, r, true)
		})
	}
}

func TestWindowFilter_HybridCanonicalisation(t *testing.T) {
	db := winReset(t)
	const root = "slack:CWHF:100.000001"
	const reply = "slack:CWHF:100.000002"
	whfNode(t, db, root, whfOutside, "unrelated", false)
	whfNode(t, db, reply, whfInside, "unrelated", true)
	for _, id := range []string{root, reply} {
		winExec(t, db, `UPDATE graph.nodes SET type='slack', scope='slack:CWHF', metadata='{"thread_ts":"100.000001"}'::jsonb WHERE id=$1`, id)
	}
	s := whfSearch(t, db, true)
	q := "q=needle&match=hybrid"
	whfExpect(t, whfRequest(t, s, q), root, true)
	whfExpect(t, whfRequest(t, s, q+whfBounds), root, false)
}

func TestWindowFilter_EpicInheritanceAndParity(t *testing.T) {
	db := winReset(t)
	s := whfSearch(t, db, true)
	for _, id := range []string{"jira:ACTIVE", "jira:INACTIVE", "active-member", "inactive-member", "ordinary"} {
		at := whfOutside
		if id == "ordinary" {
			at = whfInside
		}
		whfNode(t, db, id, at, "unrelated", true)
	}
	for _, key := range []string{"ACTIVE", "INACTIVE"} {
		at := whfInside
		if key == "INACTIVE" {
			at = whfOutside
		}
		for _, id := range []string{"jira:" + key, strings.ToLower(key) + "-member"} {
			winExec(t, db, `INSERT INTO graph.epic_membership(node_id,epic_key,via,confidence,first_at,last_at) VALUES($1,$2,'epic_self',1,$3::timestamptz,$3::timestamptz)`, id, key, at)
		}
	}
	q := "q=needle&arms=semantic&limit=50"
	c := whfRequest(t, s, q)
	r := whfRequest(t, s, q+whfBounds)
	ids := []string{"jira:ACTIVE", "jira:INACTIVE", "active-member", "inactive-member", "ordinary"}
	for _, id := range ids {
		whfExpect(t, c, id, true)
		whfExpect(t, r, id, id != "jira:INACTIVE" && id != "inactive-member")
	}
	loc, _ := time.LoadLocation("Asia/Ho_Chi_Minh")
	win, _, _, err := s.window(url.Values{"since": {"2026-09-28"}, "until": {"2026-10-05"}}, "needle", s.now(), loc, true)
	if err != nil {
		t.Fatal(err)
	}
	eligible, err := nodesEligibleInWindow(context.Background(), db, ids, win)
	if err != nil {
		t.Fatal(err)
	}
	hits, err := temporalArm(context.Background(), db, bfs.NewExpander(db), win, nil, searchFilter{})
	if err != nil {
		t.Fatal(err)
	}
	// No edges in this fixture: all hits precede one-hop spread.
	for _, h := range hits {
		if !eligible[h.ID] {
			t.Errorf("temporal id %s missing from eligibility", h.ID)
		}
	}
	if len(hits) != 3 {
		t.Errorf("temporal hits=%v, want active self/member and ordinary", hits)
	}
	empty, err := nodesEligibleInWindow(context.Background(), nil, nil, temporal.Window{})
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty lookup=%v, %v", empty, err)
	}
}

func TestSearchWindow_ExplicitFirstValue(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values url.Values
		want   bool
	}{
		{"absent", nil, false}, {"empty", url.Values{"since": {""}}, false}, {"spaces", url.Values{"since": {" \t "}, "until": {" "}}, false},
		{"since", url.Values{"since": {" 2026-09-28 "}}, true}, {"until", url.Values{"until": {"2026-10-05"}}, true},
		{"ignore-later-since", url.Values{"since": {"", "2026-09-28"}}, false}, {"ignore-later-until", url.Values{"until": {" ", "2026-10-05"}}, false},
		{"first-since", url.Values{"since": {"2026-09-28", ""}}, true}, {"other-bound", url.Values{"since": {"", "later"}, "until": {"2026-10-05"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := explicitWindow(tc.values); got != tc.want {
				t.Errorf("explicit=%t, want %t", got, tc.want)
			}
		})
	}
}

// Cancel only the eligibility SQL, after hydration and fusion have succeeded.
type whfEligibilityFailure struct{ reached bool }

func (f *whfEligibilityFailure) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, "SELECT n.id FROM graph.nodes n") && strings.Contains(d.SQL, "em.epic_key = ANY(ARRAY(") {
		f.reached = true
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		return cancelled
	}
	return ctx
}
func (*whfEligibilityFailure) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
func TestWindowFilter_FailClosed(t *testing.T) {
	for _, mode := range []string{"&arms=semantic", "&match=hybrid"} {
		t.Run(mode, func(t *testing.T) {
			db := winReset(t)
			whfNode(t, db, "inside", whfInside, "unrelated", true)
			control := whfSearch(t, db, true)
			whfExpect(t, whfRequest(t, control, "q=needle"+mode+whfBounds), "inside", true)
			cfg := db.Config()
			failure := &whfEligibilityFailure{}
			cfg.ConnConfig.Tracer = failure
			pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			s := whfSearch(t, pool, true)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, httptest.NewRequest("GET", "/api/graph/search?q=needle"+mode+whfBounds, nil))
			if !failure.reached {
				t.Fatal("eligibility failure seam not reached")
			}
			if w.Code != 500 {
				t.Errorf("status=%d, want 500: %s", w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), `"results"`) {
				t.Errorf("failure returned results envelope: %s", w.Body.String())
			}
		})
	}
}
