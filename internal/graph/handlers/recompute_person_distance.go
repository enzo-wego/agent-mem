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
)
SELECT
  LEAST(a.start_eeid, b.start_eeid) AS a_eeid,
  GREATEST(a.start_eeid, b.start_eeid) AS b_eeid,
  a.depth + b.depth AS hops,
  a.ancestor AS lca_eeid
FROM ancestors a
JOIN ancestors b
  ON a.ancestor = b.ancestor AND a.start_eeid < b.start_eeid
ON CONFLICT (a_eeid, b_eeid) DO UPDATE SET
  hops     = LEAST(graph.person_distance.hops, EXCLUDED.hops),
  lca_eeid = CASE WHEN EXCLUDED.hops < graph.person_distance.hops
                  THEN EXCLUDED.lca_eeid
                  ELSE graph.person_distance.lca_eeid END;
`
		_, err := db.Exec(ctx, q)
		return err
	}
}
