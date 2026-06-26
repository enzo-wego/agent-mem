package bfs

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Neighbor is one row returned from an expansion.
type Neighbor struct {
	NodeID   string
	EdgeKind string
}

// Expander runs the SQL that finds direct neighbours of a node.
type Expander struct {
	db *pgxpool.Pool
}

// NewExpander creates a new Expander backed by the given pool.
func NewExpander(db *pgxpool.Pool) *Expander {
	return &Expander{db: db}
}

// Expand returns direct neighbours (in both directions) of nodeID,
// optionally filtered by edge kinds.
func (e *Expander) Expand(ctx context.Context, nodeID string, kinds []string) ([]Neighbor, error) {
	const q = `
SELECT to_node_id AS nbr, kind FROM graph.edges
  WHERE from_node_id = $1 AND ($2::text[] IS NULL OR kind = ANY($2))
UNION
SELECT from_node_id AS nbr, kind FROM graph.edges
  WHERE to_node_id = $1 AND ($2::text[] IS NULL OR kind = ANY($2))
`
	var kindArg any
	if len(kinds) > 0 {
		kindArg = kinds
	}
	rows, err := e.db.Query(ctx, q, nodeID, kindArg)
	if err != nil {
		return nil, err
	}
	var out []Neighbor
	seen := map[string]bool{}
	for rows.Next() {
		var n Neighbor
		if err := rows.Scan(&n.NodeID, &n.EdgeKind); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, n)
		seen[n.NodeID] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Thread cohesion: a Slack message's thread siblings (same channel + thread_ts)
	// are reachable even though no edge connects them, so opening any one message's
	// graph surfaces the whole thread — and resources linked to a *reply* (e.g. a
	// Jira ticket mentioned mid-thread) show up when you open the root. Only when
	// unfiltered, so a kind-scoped traversal stays edge-only.
	if len(kinds) == 0 && strings.HasPrefix(nodeID, "slack:") {
		sibs, serr := e.threadSiblings(ctx, nodeID)
		if serr != nil {
			return nil, serr
		}
		for _, id := range sibs {
			if !seen[id] {
				out = append(out, Neighbor{NodeID: id, EdgeKind: "THREAD"})
				seen[id] = true
			}
		}
	}
	return out, nil
}

// threadSiblings returns the other Slack messages in the same thread as nodeID
// (same channel and thread_ts; the root's thread key is its own ts).
func (e *Expander) threadSiblings(ctx context.Context, nodeID string) ([]string, error) {
	const q = `
WITH me AS (
  SELECT REPLACE(scope,'slack:','') AS ch,
         COALESCE(NULLIF(metadata->>'thread_ts',''), split_part(id,':',3)) AS tt
  FROM graph.nodes WHERE id = $1
)
SELECT n.id FROM graph.nodes n, me
WHERE n.scope = 'slack:' || me.ch
  AND COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3)) = me.tt
  AND n.id <> $1 AND n.deleted_at IS NULL`
	rows, err := e.db.Query(ctx, q, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
