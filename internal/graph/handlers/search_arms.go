package handlers

import (
	"context"
	"fmt"
	"sort"
	"time"

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

// armHit is one ranked candidate from an arm. Score is arm-local (cosine,
// ts_rank, edge weight…) and only orders the arm's list; fusion is by rank.
type armHit struct {
	ID    string
	Score float64
	At    time.Time
}

// searchFilter is the WHERE every arm shares: soft-deleted rows out, optional
// type list, ACL scopes, epic/business membership.
type searchFilter struct {
	types any // []string or nil
	scope any // []string or nil (nil = trusted unfiltered view)
	epic  any // []string or nil
}

// sql renders the predicate with the filter's three arrays bound at
// positional parameters t, s and e.
func (f searchFilter) sql(t, s, e int) string {
	return fmt.Sprintf(`n.deleted_at IS NULL
  AND ($%[1]d::text[] IS NULL OR n.type = ANY($%[1]d))
  AND ($%[2]d::text[] IS NULL OR n.scope IS NULL OR n.scope = '' OR n.scope = ANY($%[2]d))
  AND `, t, s) + fmt.Sprintf(epicScopePredicate, fmt.Sprintf("$%d", e))
}

func (f searchFilter) args() []any { return []any{f.types, f.scope, f.epic} }

// semanticArm orders indexed nodes by cosine to the query vector.
func semanticArm(ctx context.Context, db *pgxpool.Pool, vec []float32, f searchFilter, limit int) ([]armHit, error) {
	rows, err := db.Query(ctx, `
SELECT n.id, 1.0 - (ai.embedding <=> $1) AS cosine,
       COALESCE(n.created_at, n.first_seen_at)
FROM graph.artifact_index ai
JOIN graph.nodes n ON n.id = ai.node_id
WHERE ai.embedding IS NOT NULL
  AND `+f.sql(3, 4, 5)+`
ORDER BY ai.embedding <=> $1
LIMIT $2`, append([]any{pgvector.NewVector(vec), limit}, f.args()...)...)
	if err != nil {
		return nil, err
	}
	return scanHits(rows)
}

// keywordArm matches websearch syntax against artifact_index.tsv (summary +
// identifiers, 'simple' config so ticket keys survive) and, as a second cheap
// signal, the node title. Ranked by ts_rank_cd with a flat bonus for a title
// hit so an exact title beats a passing mention in a summary.
func keywordArm(ctx context.Context, db *pgxpool.Pool, q string, f searchFilter, limit int) ([]armHit, error) {
	rows, err := db.Query(ctx, `
WITH tq AS (SELECT websearch_to_tsquery('simple', $1) AS q)
SELECT n.id,
       COALESCE(ts_rank_cd(ai.tsv, tq.q), 0)
         + CASE WHEN n.title ILIKE '%' || $1 || '%' THEN 0.5 ELSE 0 END AS rank,
       COALESCE(n.created_at, n.first_seen_at)
FROM graph.nodes n
CROSS JOIN tq
LEFT JOIN graph.artifact_index ai ON ai.node_id = n.id
WHERE (ai.tsv @@ tq.q OR n.title ILIKE '%' || $1 || '%')
  AND `+f.sql(3, 4, 5)+`
ORDER BY rank DESC, n.updated_at DESC
LIMIT $2`, append([]any{q, limit}, f.args()...)...)
	if err != nil {
		return nil, err
	}
	return scanHits(rows)
}

const (
	temporalCandidates = 60
	temporalBuckets    = 8
	temporalSpread     = 0.7
)

// temporalArm returns nodes whose event time falls in the window, or whose
// epic's activity window overlaps it, ordered by cosine to the topic vector
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
  AND (
    (COALESCE(n.created_at, n.first_seen_at) >= $2 AND COALESCE(n.created_at, n.first_seen_at) < $3)
    OR EXISTS (
      SELECT 1 FROM graph.epic_membership em
      JOIN graph.epic_membership ep
        ON ep.epic_key = em.epic_key AND ep.node_id = 'jira:' || em.epic_key
      WHERE em.node_id = n.id AND ep.first_at < $3 AND ep.last_at >= $2)
  )
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
  AND COALESCE(n.created_at, n.first_seen_at) >= $2
  AND COALESCE(n.created_at, n.first_seen_at) < $3
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
