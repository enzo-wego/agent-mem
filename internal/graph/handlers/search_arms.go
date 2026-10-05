package handlers

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"github.com/agent-mem/agent-mem/internal/graph/bfs"
	"github.com/agent-mem/agent-mem/internal/graph/scoring"
	"github.com/agent-mem/agent-mem/internal/graph/temporal"
)

// Arm names, also the keys of score_breakdown.ranks and the `arms` param.
const (
	armSemantic = "semantic"
	armKeyword  = "keyword"
	armGraph    = "graph"
	armTemporal = "temporal"
)

var allArms = []string{armSemantic, armKeyword, armGraph, armTemporal}

// semanticMinCosine uses the human-selected F=0.65. Calibration E_min=0.6672
// and N_max=0.6245 leave thin margins: 0.017 below good, 0.025 above noise.
// See docs/ai/report-semantic-floor.md.
const semanticMinCosine = 0.65

// aboveSemanticFloor filters in place, preserving arm rank order.
func aboveSemanticFloor(hits []armHit) []armHit {
	kept := 0
	for _, hit := range hits {
		if hit.Score >= semanticMinCosine {
			hits[kept] = hit
			kept++
		}
	}
	clear(hits[kept:])
	return hits[:kept]
}

// armHit is one ranked candidate from an arm. Score is arm-local (cosine,
// ts_rank, edge weight…) and only orders the arm's list; fusion is by rank.
type armHit struct {
	ID    string
	Score float64
	At    time.Time
	// Key is the Slack thread key, set only by the folded (hybrid) arms.
	Key string
}

// searchFilter is the WHERE every arm shares: soft-deleted rows out, optional
// type list, ACL scopes, epic/business membership.
type searchFilter struct {
	types    any // []string or nil
	scope    any // []string or nil (nil = trusted unfiltered view)
	epic     any // []string or nil
	resolved bool
	win      *temporal.Window // optional arm-only window; sql and args ignore it
}

// sql binds the arrays at t, s and e, and resolved immediately after e.
func (f searchFilter) sql(t, s, e int) string {
	return fmt.Sprintf(`n.deleted_at IS NULL
  AND ($%[1]d::text[] IS NULL OR n.type = ANY($%[1]d))
  AND `, t) + aclVisibleSQL("n", s, e+1) + `
  AND ` + fmt.Sprintf(epicScopePredicate, fmt.Sprintf("$%d", e))
}

func (f searchFilter) args() []any { return []any{f.types, f.scope, f.epic, f.resolved} }

// semanticArm orders indexed nodes by cosine to the query vector.
func semanticArm(ctx context.Context, db *pgxpool.Pool, vec []float32, f searchFilter, limit int) ([]armHit, error) {
	rows, err := semanticRows(ctx, db, vec, f, limit, "")
	if err != nil {
		return nil, err
	}
	hits, err := scanHits(rows)
	if err != nil {
		return nil, err
	}
	return aboveSemanticFloor(hits), nil
}

// semanticRows runs the semantic query; extra is an optional additional
// select-list expression (", <expr>") appended after the three base columns.
// An arm-only window is bound at $7 and $8, after the shared filter values.
func semanticRows(ctx context.Context, db *pgxpool.Pool, vec []float32, f searchFilter, limit int, extra string) (pgx.Rows, error) {
	predicate := f.sql(3, 4, 5)
	args := append([]any{pgvector.NewVector(vec), limit}, f.args()...)
	if f.win != nil {
		predicate += "\n  AND " + windowEligibleSQLAt(7, 8)
		args = append(args, f.win.Start, f.win.End)
	}
	return db.Query(ctx, `
SELECT n.id, 1.0 - (ai.embedding <=> $1) AS cosine,
       COALESCE(n.created_at, n.first_seen_at)`+extra+`
FROM graph.artifact_index ai
JOIN graph.nodes n ON n.id = ai.node_id
WHERE ai.embedding IS NOT NULL
  AND `+predicate+`
ORDER BY ai.embedding <=> $1
LIMIT $2`, args...)
}

// ilikeEscaper escapes the LIKE metacharacters so a query such as "50%" is
// matched literally. strings.Replacer scans once, so an inserted backslash is
// never escaped again.
var ilikeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// keywordArmSQL is the complete keyword-arm statement. Candidates come from
// three index-friendly sources (the GIN index on artifact_index.tsv, a title
// ILIKE, and the GIN index on the first 20k characters of nodes.body), UNIONed,
// so no match kind forces a sequential scan through an OR across a join. The
// candidate arm repeats the tsquery expression instead of joining the tq CTE:
// a CTE scan is not a parameter, so the planner could not use the GIN index
// through it. The body expression must match idx_nodes_body_tsv exactly. The
// artifact_index join is a LEFT JOIN so nodes without an index row qualify.
// Parameters: $1 query, $2 limit, $3-$5 filter arrays, $6 resolved, $7 escaped query.
// An arm-only window appends $8 start and $9 end.
func keywordArmSQL(f searchFilter) string {
	predicate := f.sql(3, 4, 5)
	if f.win != nil {
		predicate += "\n  AND " + windowEligibleSQLAt(8, 9)
	}
	return keywordCTE + `
SELECT n.id,
       ` + keywordRankSQL + `,
       COALESCE(n.created_at, n.first_seen_at)
FROM candidates c
JOIN graph.nodes n ON n.id = c.id
CROSS JOIN tq
LEFT JOIN graph.artifact_index ai ON ai.node_id = n.id
WHERE ` + predicate + `
ORDER BY rank DESC, n.updated_at DESC
LIMIT $2`
}

// keywordCTE is the shared tq + candidates prefix of the keyword statements.
const keywordCTE = `
WITH tq AS (SELECT websearch_to_tsquery('simple', $1) AS q),
candidates AS (
  SELECT node_id AS id FROM graph.artifact_index WHERE tsv @@ websearch_to_tsquery('simple', $1)
  UNION
  SELECT id FROM graph.nodes WHERE title ILIKE '%' || $7 || '%' ESCAPE '\'
  UNION
  -- ponytail: body hits are not ranked by body relevance; add ts_rank_cd over the body expression if eval shows body-only hits ordered badly.
  SELECT id FROM graph.nodes WHERE to_tsvector('simple'::regconfig, left(coalesce(body, ''), 20000)) @@ websearch_to_tsquery('simple', $1)
)`

// keywordRankSQL is the keyword score: ts_rank_cd plus a flat title bonus.
const keywordRankSQL = `COALESCE(ts_rank_cd(ai.tsv, tq.q), 0)
         + CASE WHEN n.title ILIKE '%' || $7 || '%' ESCAPE '\' THEN 0.5 ELSE 0 END AS rank`

// keywordArmArgs binds keywordArmSQL's parameters.
func keywordArmArgs(q string, f searchFilter, limit int) []any {
	args := append([]any{q, limit}, append(f.args(), ilikeEscaper.Replace(q))...)
	if f.win != nil {
		args = append(args, f.win.Start, f.win.End)
	}
	return args
}

// keywordArm matches websearch syntax against artifact_index.tsv (summary +
// identifiers, 'simple' config so ticket keys survive) and, as a second cheap
// signal, the node title. Ranked by ts_rank_cd with a flat bonus for a title
// hit so an exact title beats a passing mention in a summary.
func keywordArm(ctx context.Context, db *pgxpool.Pool, q string, f searchFilter, limit int) ([]armHit, error) {
	rows, err := db.Query(ctx, keywordArmSQL(f), keywordArmArgs(q, f, limit)...)
	if err != nil {
		return nil, err
	}
	return scanHits(rows)
}

// epicSelfIDSQL is the node id of an epic's own membership row: the business
// root for its own key, 'jira:<key>' for every other epic.
func epicSelfIDSQL(alias string) string {
	return fmt.Sprintf("CASE WHEN %[1]s.epic_key = '%[2]s' THEN '%[2]s' ELSE 'jira:' || %[1]s.epic_key END", alias, businessRootID)
}

// directWindowSQL is the one rule for "node n is in window [$2,$3)" shared by
// the temporal arm and graph expansion. Ordinary nodes match on their own
// event time. Epic self nodes and the business root never match on time
// (stub epics carry a placeholder first_seen_at); they match only when their
// epic window overlaps.
var directWindowSQL = directWindowSQLAt(2, 3)

func directWindowSQLAt(start, end int) string {
	return fmt.Sprintf(`(
  (NOT EXISTS (SELECT 1 FROM graph.epic_membership es WHERE es.node_id = n.id AND es.node_id = `+epicSelfIDSQL("es")+`)
   AND COALESCE(n.created_at, n.first_seen_at) >= $%[1]d AND COALESCE(n.created_at, n.first_seen_at) < $%[2]d)
  OR EXISTS (SELECT 1 FROM graph.epic_membership es WHERE es.node_id = n.id AND es.node_id = `+epicSelfIDSQL("es")+`
             AND es.first_at < $%[2]d AND es.last_at >= $%[1]d)
)`, start, end)
}

// windowEligibleSQL includes direct time eligibility and, for non-Slack nodes,
// inheritance from an active epic self row. Slack uses only its own time.
// Keep the active epic set uncorrelated.
var windowEligibleSQL = windowEligibleSQLAt(2, 3)

func windowEligibleSQLAt(start, end int) string {
	return `(
  ` + directWindowSQLAt(start, end) + fmt.Sprintf(`
  OR (n.type <> 'slack' AND EXISTS (
    SELECT 1 FROM graph.epic_membership em
    WHERE em.node_id = n.id
      AND em.epic_key = ANY(ARRAY(
        SELECT ep.epic_key FROM graph.epic_membership ep
        WHERE ep.node_id = `+epicSelfIDSQL("ep")+`
          AND ep.epic_key <> '`+businessRootID+`'
          AND ep.first_at < $%[2]d AND ep.last_at >= $%[1]d))))
)`, start, end)
}

// nodesEligibleInWindow checks the same eligibility as temporal retrieval,
// against the canonical ids that hydration used.
func nodesEligibleInWindow(ctx context.Context, db *pgxpool.Pool, ids []string, w temporal.Window) (map[string]bool, error) {
	out := make(map[string]bool, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := db.Query(ctx, `
SELECT n.id FROM graph.nodes n
WHERE n.id = ANY($1) AND `+windowEligibleSQL, ids, w.Start, w.End)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

const (
	temporalCandidates = 60
	temporalBuckets    = 8
	temporalSpread     = 0.7
)

// temporalArm returns nodes whose event time falls in the window, or whose
// epic's activity window overlaps it (non-Slack nodes only). Slack threads use
// only their own time. Results are ordered by cosine to the topic vector
// (event time when there is no vector). The top 60 are spread across 8 time
// buckets so one busy week cannot crowd out the rest, then each is expanded
// one hop over REFERENCES/PART_OF at score × 0.7 with the window re-applied.
func temporalArm(ctx context.Context, db *pgxpool.Pool, exp *bfs.Expander, w temporal.Window, vec []float32, f searchFilter) ([]armHit, error) {
	var vecArg any
	if vec != nil {
		vecArg = pgvector.NewVector(vec)
	}
	rows, err := db.Query(ctx, `
SELECT n.id,
       CASE WHEN $1::vector IS NULL OR ai.embedding IS NULL THEN 0
            ELSE 1.0 - (ai.embedding <=> $1) END AS cosine,
       COALESCE(n.created_at, n.first_seen_at) AS at
FROM graph.nodes n
LEFT JOIN graph.artifact_index ai ON ai.node_id = n.id
WHERE `+f.sql(5, 6, 7)+`
  AND `+windowEligibleSQL+`
ORDER BY cosine DESC, at DESC
LIMIT $4`, append([]any{vecArg, w.Start, w.End, temporalCandidates}, f.args()...)...)
	if err != nil {
		return nil, err
	}
	hits, err := scanHits(rows)
	if err != nil {
		return nil, err
	}
	dated := make([]temporal.Dated, len(hits))
	for i, h := range hits {
		dated[i] = temporal.Dated{ID: h.ID, At: h.At, Score: h.Score}
	}
	dated = temporal.RoundRobin(dated, w, temporalBuckets)

	out := make([]armHit, 0, len(dated)*2)
	seen := make(map[string]bool, len(dated)*2)
	for _, d := range dated {
		out = append(out, armHit{ID: d.ID, Score: d.Score, At: d.At})
		seen[d.ID] = true
	}
	// One-hop spread: a dated thread pulls in the ticket/PR it references
	// (and vice versa) as long as that node is itself inside the window.
	var spread []armHit
	for _, d := range dated {
		nbrs, err := exp.Expand(ctx, d.ID, []string{"REFERENCES", "PART_OF"})
		if err != nil {
			return nil, err
		}
		for _, n := range nbrs {
			if seen[n.NodeID] {
				continue
			}
			seen[n.NodeID] = true
			spread = append(spread, armHit{ID: n.NodeID, Score: d.Score * temporalSpread})
		}
	}
	if len(spread) > 0 {
		ids := make([]string, len(spread))
		for i, s := range spread {
			ids[i] = s.ID
		}
		inWindow, err := nodesInWindow(ctx, db, ids, w, f)
		if err != nil {
			return nil, err
		}
		for _, s := range spread {
			if at, ok := inWindow[s.ID]; ok {
				s.At = at
				out = append(out, s)
			}
		}
	}
	return out, nil
}

// nodesInWindow returns the event time of each id whose time is inside w and
// which passes the filter.
func nodesInWindow(ctx context.Context, db *pgxpool.Pool, ids []string, w temporal.Window, f searchFilter) (map[string]time.Time, error) {
	rows, err := db.Query(ctx, `
SELECT n.id, COALESCE(n.created_at, n.first_seen_at) AS at
FROM graph.nodes n
WHERE n.id = ANY($1)
  AND `+directWindowSQL+`
  AND `+f.sql(4, 5, 6), append([]any{ids, w.Start, w.End}, f.args()...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]time.Time, len(ids))
	for rows.Next() {
		var id string
		var at time.Time
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		out[id] = at
	}
	return out, rows.Err()
}

const graphSeeds = 10

// graphArm expands the top semantic hits one hop over REFERENCES, SAME_TOPIC
// and PART_OF. Score = scoring.Edge(1) × edge confidence (1.0 for the
// deterministic kinds). Neighbours that expandableThrough rejects (people,
// tags, the business root, hub tickets) are skipped: they connect everything.
// Seeds are not re-emitted; the semantic arm already ranks them.
func graphArm(ctx context.Context, db *pgxpool.Pool, exp *bfs.Expander, seeds []armHit) ([]armHit, error) {
	if len(seeds) > graphSeeds {
		seeds = seeds[:graphSeeds]
	}
	seedSet := make(map[string]bool, len(seeds))
	for _, s := range seeds {
		seedSet[s.ID] = true
	}
	best := make(map[string]float64)
	var order []string
	for _, s := range seeds {
		nbrs, err := exp.Expand(ctx, s.ID, []string{"REFERENCES", "SAME_TOPIC", "PART_OF"})
		if err != nil {
			return nil, err
		}
		for _, n := range nbrs {
			if seedSet[n.NodeID] {
				continue
			}
			conf := 1.0
			if n.EdgeKind == "SAME_TOPIC" && n.Confidence > 0 {
				conf = n.Confidence
			}
			score := scoring.Edge(1) * conf
			if prev, ok := best[n.NodeID]; ok {
				if score > prev {
					best[n.NodeID] = score
				}
				continue
			}
			if !expandableThrough(ctx, db, n.NodeID) {
				continue
			}
			best[n.NodeID] = score
			order = append(order, n.NodeID)
		}
	}
	out := make([]armHit, 0, len(order))
	for _, id := range order {
		out = append(out, armHit{ID: id, Score: best[id]})
	}
	sortHits(out)
	return out, nil
}

// scanHits reads (id, score, at) rows in order.
func scanHits(rows interface {
	Next() bool
	Scan(...any) error
	Close()
	Err() error
}) ([]armHit, error) {
	defer rows.Close()
	var out []armHit
	for rows.Next() {
		var h armHit
		if err := rows.Scan(&h.ID, &h.Score, &h.At); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// sortHits orders best-first, stable so the arm's own tie order survives.
func sortHits(hs []armHit) {
	sort.SliceStable(hs, func(i, j int) bool { return hs[i].Score > hs[j].Score })
}
