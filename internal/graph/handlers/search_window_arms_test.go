package handlers

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/temporal"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
)

// These are literal snapshots of 73b9b64, with epicSelfIDSQL expanded.
// Do not construct them from the fragments under test.
const windowArmsGoldenDirect = `(
  (NOT EXISTS (SELECT 1 FROM graph.epic_membership es WHERE es.node_id = n.id AND es.node_id = CASE WHEN es.epic_key = 'business:payments' THEN 'business:payments' ELSE 'jira:' || es.epic_key END)
   AND COALESCE(n.created_at, n.first_seen_at) >= $2 AND COALESCE(n.created_at, n.first_seen_at) < $3)
  OR EXISTS (SELECT 1 FROM graph.epic_membership es WHERE es.node_id = n.id AND es.node_id = CASE WHEN es.epic_key = 'business:payments' THEN 'business:payments' ELSE 'jira:' || es.epic_key END
             AND es.first_at < $3 AND es.last_at >= $2)
)`

const windowArmsGoldenEligible = `(
  (
  (NOT EXISTS (SELECT 1 FROM graph.epic_membership es WHERE es.node_id = n.id AND es.node_id = CASE WHEN es.epic_key = 'business:payments' THEN 'business:payments' ELSE 'jira:' || es.epic_key END)
   AND COALESCE(n.created_at, n.first_seen_at) >= $2 AND COALESCE(n.created_at, n.first_seen_at) < $3)
  OR EXISTS (SELECT 1 FROM graph.epic_membership es WHERE es.node_id = n.id AND es.node_id = CASE WHEN es.epic_key = 'business:payments' THEN 'business:payments' ELSE 'jira:' || es.epic_key END
             AND es.first_at < $3 AND es.last_at >= $2)
)
  OR (n.type <> 'slack' AND EXISTS (
    SELECT 1 FROM graph.epic_membership em
    WHERE em.node_id = n.id
      AND em.epic_key = ANY(ARRAY(
        SELECT ep.epic_key FROM graph.epic_membership ep
        WHERE ep.node_id = CASE WHEN ep.epic_key = 'business:payments' THEN 'business:payments' ELSE 'jira:' || ep.epic_key END
          AND ep.epic_key <> 'business:payments'
          AND ep.first_at < $3 AND ep.last_at >= $2))))
)`

func TestWindowArms_GoldenSQL(t *testing.T) {
	if got := directWindowSQLAt(2, 3); got != windowArmsGoldenDirect {
		t.Fatalf("direct SQL changed from 73b9b64:\n%s", got)
	}
	if got := windowEligibleSQLAt(2, 3); got != windowArmsGoldenEligible {
		t.Fatalf("eligible SQL changed from 73b9b64:\n%s", got)
	}
	if directWindowSQL != windowArmsGoldenDirect || windowEligibleSQL != windowArmsGoldenEligible {
		t.Fatal("legacy fragment variables changed from 73b9b64")
	}
	for name, got := range map[string]string{
		"direct": directWindowSQLAt(8, 9), "eligible": windowEligibleSQLAt(8, 9),
	} {
		if !strings.Contains(got, "$8") || !strings.Contains(got, "$9") || strings.Contains(got, "$2") || strings.Contains(got, "$3") {
			t.Errorf("%s placeholder relocation failed:\n%s", name, got)
		}
	}
}

// These fixtures prove SQL eligibility before LIMIT, not HNSW recall under
// selective filters. Use an exact scan independently of earlier fixture stats.
func windowArmsDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	base := winReset(t)
	config := base.Config()
	config.ConnConfig.RuntimeParams["enable_indexscan"] = "off"
	db, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

func windowArmsWindow() temporal.Window {
	zone := time.FixedZone("+07", 7*60*60)
	return temporal.Window{
		Start: time.Date(2026, 9, 28, 0, 0, 0, 0, zone),
		End:   time.Date(2026, 10, 6, 0, 0, 0, 0, zone),
	}
}

func windowArmsVector(cosine float64) []float32 {
	v := make([]float32, GraphEmbeddingDims)
	v[0], v[1] = float32(cosine), float32(math.Sqrt(1-cosine*cosine))
	return v
}

func windowArmsIndex(t *testing.T, db *pgxpool.Pool, id, summary string, vec []float32) {
	t.Helper()
	var embedding any
	if vec != nil {
		embedding = pgvector.NewVector(vec)
	}
	winExec(t, db, `INSERT INTO graph.artifact_index(node_id,summary,summary_kind,embedding,machine_id) VALUES($1,$2,'heuristic',$3,'test')`, id, summary, embedding)
}

// Every ordinary Jira node folds to itself. Fifteen superior outsiders fill
// even semanticArmFolded's 3*5 pre-fold fetch. Insiders have cosines 0.80-0.84,
// all above the 0.65 floor, versus outsiders at 0.95. Keyword outsiders have
// both a title bonus and full-text relevance; insiders have only the title
// bonus. Explicit, distinct updated_at values make ties deterministic.
func windowArmsFixture(t *testing.T, db *pgxpool.Pool, semantic bool, outsiders int) []string {
	t.Helper()
	for i := range outsiders {
		id := fmt.Sprintf("ordinary:outside:%02d", i)
		whfNode(t, db, id, whfOutside, "needle", false)
		vec := []float32(nil)
		if semantic {
			vec = windowArmsVector(0.95)
		}
		windowArmsIndex(t, db, id, "needle", vec)
		winExec(t, db, `UPDATE graph.nodes SET updated_at=$2::timestamptz WHERE id=$1`, id, time.Date(2026, 10, 7, 0, i, 0, 0, time.UTC))
	}
	var inside []string
	for i := range 5 {
		id := fmt.Sprintf("ordinary:inside:%02d", i)
		whfNode(t, db, id, whfInside, "needle", false)
		vec := []float32(nil)
		if semantic {
			vec = windowArmsVector(0.80 + float64(i)*0.01)
		}
		windowArmsIndex(t, db, id, "unrelated", vec)
		winExec(t, db, `UPDATE graph.nodes SET updated_at=$2::timestamptz WHERE id=$1`, id, time.Date(2026, 9, 30, 0, i, 0, 0, time.UTC))
		inside = append(inside, id)
	}
	return inside
}

type windowArmsCall func(context.Context, *pgxpool.Pool, searchFilter, int) ([]armHit, error)

func windowArmsCalls(t *testing.T) map[string]windowArmsCall {
	t.Helper()
	// Reuse the fake embedder used by the handler fixtures, not a real service.
	vec, err := (whfEmbedder{}).Embed(context.Background(), "needle")
	if err != nil {
		t.Fatal(err)
	}
	return map[string]windowArmsCall{
		"semanticArm": func(ctx context.Context, db *pgxpool.Pool, f searchFilter, budget int) ([]armHit, error) {
			return semanticArm(ctx, db, vec, f, budget)
		},
		"semanticArmFolded": func(ctx context.Context, db *pgxpool.Pool, f searchFilter, budget int) ([]armHit, error) {
			return semanticArmFolded(ctx, db, vec, f, budget)
		},
		"keywordArm": func(ctx context.Context, db *pgxpool.Pool, f searchFilter, budget int) ([]armHit, error) {
			return keywordArm(ctx, db, "needle", f, budget)
		},
		"keywordArmFolded": func(ctx context.Context, db *pgxpool.Pool, f searchFilter, budget int) ([]armHit, error) {
			return keywordArmFolded(ctx, db, "needle", f, budget)
		},
	}
}

func windowArmsExpectIDs(t *testing.T, hits []armHit, want []string) {
	t.Helper()
	got := make(map[string]bool)
	for _, hit := range hits {
		got[hit.ID] = true
	}
	expected := make(map[string]bool)
	for _, id := range want {
		expected[id] = true
	}
	if len(hits) != len(want) || !reflect.DeepEqual(got, expected) {
		t.Fatalf("raw arm IDs=%v (%d hits), want %v", got, len(hits), expected)
	}
}

func windowArmsParity(t *testing.T, db *pgxpool.Pool, hits []armHit, win temporal.Window) {
	t.Helper()
	ids := make([]string, len(hits))
	for i, hit := range hits {
		ids[i] = hit.ID // deliberately before any canonicalisation
	}
	eligible, err := nodesEligibleInWindow(context.Background(), db, ids, win)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if !eligible[id] {
			t.Errorf("raw arm hit %s is not eligible in the same window", id)
		}
	}
}

func windowArmsRefill(t *testing.T, name string, semantic bool) {
	t.Helper()
	db := windowArmsDB(t)
	want := windowArmsFixture(t, db, semantic, 15)
	call := windowArmsCalls(t)[name]
	control, err := call(context.Background(), db, searchFilter{}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(control) != 5 {
		t.Fatalf("unwindowed control returned %d hits, want 5", len(control))
	}
	for _, hit := range control {
		if !strings.HasPrefix(hit.ID, "ordinary:outside:") {
			t.Fatalf("unwindowed budget was not exhausted by stronger outsiders: %v", control)
		}
	}
	win := windowArmsWindow()
	hits, err := call(context.Background(), db, searchFilter{win: &win}, 5)
	if err != nil {
		t.Fatal(err)
	}
	windowArmsExpectIDs(t, hits, want)
	windowArmsParity(t, db, hits, win)
}

// Separate entry points make an individual arm's removed predicate fail its
// own refill assertion, without relying on fusion or the post-fusion filter.
func TestWindowArms_SemanticRefill(t *testing.T) {
	windowArmsRefill(t, "semanticArm", true)
}
func TestWindowArms_SemanticFoldedRefill(t *testing.T) {
	windowArmsRefill(t, "semanticArmFolded", true)
}
func TestWindowArms_KeywordRefill(t *testing.T) {
	windowArmsRefill(t, "keywordArm", false)
}
func TestWindowArms_KeywordFoldedRefill(t *testing.T) {
	windowArmsRefill(t, "keywordArmFolded", false)
}

func TestWindowArms_FilterBindingIsolation(t *testing.T) {
	win := windowArmsWindow()
	f := searchFilter{types: []string{"jira"}, scope: []string{"jira:PAY"}, epic: []string{"PAY-100"}, resolved: true}
	windowed := f
	windowed.win = &win
	if f.sql(3, 4, 5) != windowed.sql(3, 4, 5) || !reflect.DeepEqual(f.args(), windowed.args()) {
		t.Fatal("arm-only window changed shared filter SQL or arguments")
	}
	base := keywordArmArgs(`a%_\b`, f, 5)
	got := keywordArmArgs(`a%_\b`, windowed, 5)
	if len(base) != 7 || len(got) != 9 || !reflect.DeepEqual(got[:7], base) || !reflect.DeepEqual(got[7:], []any{win.Start, win.End}) {
		t.Fatalf("keyword binding layout changed: base=%v windowed=%v", base, got)
	}
}

func TestWindowArms_KeywordEscaping(t *testing.T) {
	for _, folded := range []bool{false, true} {
		for _, fixture := range []struct{ query, decoy string }{
			{"rate%done", "rateXdone"}, {"rate_done", "rateXdone"}, {`rate\done`, "ratedone"},
		} {
			t.Run(fmt.Sprintf("folded=%t/%s", folded, fixture.query), func(t *testing.T) {
				db := windowArmsDB(t)
				// No index rows or bodies: only title ILIKE can supply a hit.
				whfNode(t, db, "literal", whfInside, fixture.query, false)
				whfNode(t, db, "decoy", whfInside, fixture.decoy, false)
				whfNode(t, db, "old-literal", whfOutside, fixture.query, false)
				call := keywordArm
				if folded {
					call = keywordArmFolded
				}
				control, err := call(context.Background(), db, fixture.query, searchFilter{}, 5)
				if err != nil {
					t.Fatal(err)
				}
				windowArmsExpectIDs(t, control, []string{"literal", "old-literal"})
				win := windowArmsWindow()
				hits, err := call(context.Background(), db, fixture.query, searchFilter{win: &win}, 5)
				if err != nil {
					t.Fatal(err)
				}
				windowArmsExpectIDs(t, hits, []string{"literal"})
				windowArmsParity(t, db, hits, win)
			})
		}
	}
}

func windowArmsHandlerIDs(t *testing.T, r whfResponse, want []string, ranks ...string) {
	t.Helper()
	if len(r.Results) != len(want) {
		t.Fatalf("handler returned %d results, want %d: %v", len(r.Results), len(want), r.Results)
	}
	for _, id := range want {
		if !whfHas(r, id) {
			t.Errorf("handler did not refill with %s", id)
		}
	}
	for _, result := range r.Results {
		for _, arm := range ranks {
			if result.ScoreBreakdown.Ranks[arm] <= 0 {
				t.Errorf("result %s/%s lacks %s rank evidence: %v", result.NodeID, result.ID, arm, result.ScoreBreakdown.Ranks)
			}
		}
	}
	whfHard(t, r, true)
}

func TestWindowArms_HandlerDefault(t *testing.T) {
	for _, arm := range []string{armSemantic, armKeyword} {
		t.Run(arm, func(t *testing.T) {
			db := windowArmsDB(t)
			want := windowArmsFixture(t, db, arm == armSemantic, 50)
			s := whfSearch(t, db, arm == armSemantic)
			r := whfRequest(t, s, "q=needle&limit=5&arms="+arm+whfBounds)
			windowArmsHandlerIDs(t, r, want, arm)
			for _, hit := range r.Results {
				if len(hit.ScoreBreakdown.Ranks) != 1 {
					t.Errorf("isolated %s request has other contributing arms: %v", arm, hit.ScoreBreakdown.Ranks)
				}
			}
		})
	}
}

func TestWindowArms_HandlerHybrid(t *testing.T) {
	db := windowArmsDB(t)
	// Fifty semantic-only outsiders exhaust the folded fetch of 3*(3*5)=45;
	// fifty separate keyword-only outsiders exhaust its 15-thread budget.
	// The five insiders match both arms, so rank evidence detects either arm
	// losing its window even if the other arm rescues the final result list.
	want := windowArmsFixture(t, db, true, 50)
	winExec(t, db, `UPDATE graph.nodes SET title='unrelated' WHERE id LIKE 'ordinary:outside:%'`)
	winExec(t, db, `UPDATE graph.artifact_index SET summary='unrelated' WHERE node_id LIKE 'ordinary:outside:%'`)
	for i := range 50 {
		id := fmt.Sprintf("keyword:outside:%02d", i)
		whfNode(t, db, id, whfOutside, "needle", false)
		windowArmsIndex(t, db, id, "needle", nil)
		winExec(t, db, `UPDATE graph.nodes SET updated_at=$2::timestamptz WHERE id=$1`, id, time.Date(2026, 10, 7, 0, i, 0, 0, time.UTC))
	}
	s := whfSearch(t, db, true)
	r := whfRequest(t, s, "q=needle&limit=5&match=hybrid"+whfBounds)
	windowArmsHandlerIDs(t, r, want, armSemantic, armKeyword)
}

func TestWindowArms_PhraseWindowUnchanged(t *testing.T) {
	db := windowArmsDB(t)
	whfNode(t, db, "phrase:outside", whfOutside, "unrelated", true)
	s := whfSearch(t, db, true)
	r := whfRequest(t, s, "q="+url.QueryEscape("needle last week")+"&arms=semantic&limit=5")
	whfExpect(t, r, "phrase:outside", true)
	whfHard(t, r, false)
	if len(r.Window) == 0 {
		t.Fatal("phrase did not produce a soft window")
	}
	for _, hit := range r.Results {
		if hit.ScoreBreakdown.Ranks[armSemantic] <= 0 {
			t.Errorf("phrase result lacks semantic rank: %v", hit.ScoreBreakdown.Ranks)
		}
	}
}

func TestWindowArms_EpicInheritanceAndParity(t *testing.T) {
	db := windowArmsDB(t)
	for _, fixture := range []struct{ id, typ, at string }{
		{"jira:PAY-100", "jira", whfOutside},
		{"epic:non-slack", "jira", whfOutside},
		{"slack:CWINDOW:100.000001", "slack", whfOutside},
		{"boundary:start", "jira", "2026-09-28T00:00:00+07:00"},
		{"boundary:end", "jira", "2026-10-06T00:00:00+07:00"},
		{"fallback", "jira", ""},
	} {
		winNode(t, db, fixture.id, fixture.typ, fixture.at, whfInside)
		winExec(t, db, `UPDATE graph.nodes SET title='needle' WHERE id=$1`, fixture.id)
		windowArmsIndex(t, db, fixture.id, "unrelated", windowArmsVector(0.9))
	}
	for _, id := range []string{"jira:PAY-100", "epic:non-slack", "slack:CWINDOW:100.000001"} {
		winExec(t, db, `INSERT INTO graph.epic_membership(node_id,epic_key,via,confidence,first_at,last_at) VALUES($1,'PAY-100',$2,1,$3::timestamptz,$3::timestamptz)`, id, viaEpicSelf, whfInside)
	}
	win := windowArmsWindow()
	for name, call := range windowArmsCalls(t) {
		t.Run(name, func(t *testing.T) {
			hits, err := call(context.Background(), db, searchFilter{win: &win}, 20)
			if err != nil {
				t.Fatal(err)
			}
			windowArmsExpectIDs(t, hits, []string{"jira:PAY-100", "epic:non-slack", "boundary:start", "fallback"})
			windowArmsParity(t, db, hits, win)
		})
	}
}
