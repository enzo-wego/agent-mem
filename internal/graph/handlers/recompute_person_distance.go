package handlers

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

// RecomputePersonDistance is registered as the handler for the
// 'recompute_person_distance' job type. Idempotent — truncates and rebuilds.
func RecomputePersonDistance(db *pgxpool.Pool, log zerolog.Logger) func(context.Context, []byte) error {
	return func(ctx context.Context, payload []byte) error {
		log.Info().Msg("recomputing person_distance")
		// Two people share MANY common ancestors (root + every node up to their LCA),
		// so the join below emits each (a,b) pair once per shared ancestor. We must
		// collapse to the LOWEST common ancestor (min hops) per pair BEFORE inserting
		// — otherwise INSERT … ON CONFLICT tries to update the same row twice in one
		// statement (SQLSTATE 21000). DISTINCT ON (a,b) ORDER BY …, hops ASC picks the
		// LCA row.
		const q = `
TRUNCATE graph.person_distance;
INSERT INTO graph.person_distance (a_eeid, b_eeid, hops, lca_eeid)
WITH RECURSIVE chain AS (
  SELECT eeid AS start_eeid, eeid AS current_eeid, 0 AS depth
  FROM graph.people WHERE eeid IS NOT NULL AND merged_into IS NULL
  UNION ALL
  SELECT c.start_eeid, p.reports_to, c.depth + 1
  FROM chain c
  JOIN graph.people p ON p.eeid = c.current_eeid
  WHERE p.reports_to IS NOT NULL AND c.depth < 20
),
ancestors AS (
  SELECT start_eeid, current_eeid AS ancestor, depth FROM chain
),
pairs AS (
  SELECT
    LEAST(a.start_eeid, b.start_eeid)    AS a_eeid,
    GREATEST(a.start_eeid, b.start_eeid) AS b_eeid,
    a.depth + b.depth                    AS hops,
    a.ancestor                           AS lca_eeid
  FROM ancestors a
  JOIN ancestors b
    ON a.ancestor = b.ancestor AND a.start_eeid < b.start_eeid
)
SELECT DISTINCT ON (a_eeid, b_eeid) a_eeid, b_eeid, hops, lca_eeid
FROM pairs
ORDER BY a_eeid, b_eeid, hops ASC;
`
		_, err := db.Exec(ctx, q)
		return err
	}
}
