package handlers

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// threadKeySQL is the Slack thread key of node alias n: the root node id
// built from the node's channel scope and thread_ts (or its own ts), for a
// Slack node; every other node is its own key. A root's key is its own id.
const threadKeySQL = `COALESCE(
  CASE WHEN n.type IN ('slack','slack_thread') AND n.scope LIKE 'slack:%' THEN
    'slack:' || substr(n.scope, 7) || ':' ||
    COALESCE(NULLIF(n.metadata->>'thread_ts',''),
             CASE WHEN array_length(string_to_array(n.id, ':'), 1) = 3
                  THEN split_part(n.id, ':', 3) END)
  END, n.id)`

// slackThreadRootSQL is threadKeySQL restricted to Slack nodes; NULL for the
// rest. The hybrid display uses it for thread_root.
const slackThreadRootSQL = `CASE WHEN n.type IN ('slack','slack_thread') AND n.scope LIKE 'slack:%' THEN ` + threadKeySQL + ` END`

// keywordArmFoldedSQL is keywordArmSQL with Slack threads folded in SQL
// before the limit: the inner DISTINCT ON keeps each thread's best member
// (DISTINCT ON must lead its ORDER BY, so ordering by rank needs the outer
// query); the outer query orders threads by that member's score.
// Parameters are keywordArmSQL's.
func keywordArmFoldedSQL(f searchFilter) string {
	predicate := f.sql(3, 4, 5)
	if f.win != nil {
		predicate += "\n  AND " + windowEligibleSQLAt(8, 9)
	}
	return keywordCTE + `,
scored AS (
  SELECT n.id,
         ` + keywordRankSQL + `,
         COALESCE(n.created_at, n.first_seen_at) AS at,
         n.updated_at AS upd,
         ` + threadKeySQL + ` AS thread_key
  FROM candidates c
  JOIN graph.nodes n ON n.id = c.id
  CROSS JOIN tq
  LEFT JOIN graph.artifact_index ai ON ai.node_id = n.id
  WHERE ` + predicate + `
),
best AS (
  SELECT DISTINCT ON (thread_key) id, rank, at, upd, thread_key
  FROM scored
  ORDER BY thread_key, rank DESC, upd DESC, id
)
SELECT id, rank, at, thread_key
FROM best
ORDER BY rank DESC, upd DESC, id
LIMIT $2`
}

// keywordArmFolded is keywordArm with one hit per Slack thread; limit counts
// distinct threads. Hit.ID is the thread's best member, Hit.Key its thread key.
func keywordArmFolded(ctx context.Context, db *pgxpool.Pool, q string, f searchFilter, limit int) ([]armHit, error) {
	rows, err := db.Query(ctx, keywordArmFoldedSQL(f), keywordArmArgs(q, f, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []armHit
	for rows.Next() {
		var h armHit
		if err := rows.Scan(&h.ID, &h.Score, &h.At, &h.Key); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// semanticArmFolded is semanticArm with one hit per Slack thread. HNSW
// ordering cannot take DISTINCT ON, so it fetches 3x the budget, keeps the
// first (best) row per thread key in Go, then truncates to the budget.
// ponytail: 3x over-fetch; a thread with more matching replies than that can still crowd others out, page further if eval shows it
func semanticArmFolded(ctx context.Context, db *pgxpool.Pool, vec []float32, f searchFilter, budget int) ([]armHit, error) {
	rows, err := semanticRows(ctx, db, vec, f, budget*3, ", "+threadKeySQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []armHit
	for rows.Next() {
		var h armHit
		if err := rows.Scan(&h.ID, &h.Score, &h.At, &h.Key); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out = aboveSemanticFloor(out)
	seen := map[string]bool{}
	kept := 0
	for _, hit := range out {
		if seen[hit.Key] {
			continue
		}
		seen[hit.Key] = true
		out[kept] = hit
		kept++
	}
	clear(out[kept:])
	out = out[:kept]
	if len(out) > budget {
		out = out[:budget]
	}
	return out, nil
}

// canonicalizeThreads rewrites each folded hit's ID to its canonical result
// id: the thread key when that root is eligible (exists, not deleted, passes
// the request's ACL/types/epic filter), else the best member's own id.
// Eligibility is decided once per root, so every arm maps a thread alike.
func canonicalizeThreads(ctx context.Context, db *pgxpool.Pool, lists map[string][]armHit, f searchFilter) error {
	set := map[string]bool{}
	var keys []string
	for _, hits := range lists {
		for _, h := range hits {
			if h.Key != "" && h.Key != h.ID && !set[h.Key] {
				set[h.Key] = true
				keys = append(keys, h.Key)
			}
		}
	}
	if len(keys) == 0 {
		return nil
	}
	rows, err := db.Query(ctx, `SELECT n.id FROM graph.nodes n WHERE n.id = ANY($1) AND `+f.sql(2, 3, 4),
		append([]any{keys}, f.args()...)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	eligible := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		eligible[id] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, hits := range lists {
		for i := range hits {
			if eligible[hits[i].Key] {
				hits[i].ID = hits[i].Key
			}
		}
	}
	return nil
}
