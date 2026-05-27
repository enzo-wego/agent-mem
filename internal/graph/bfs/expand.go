package bfs

import (
	"context"

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
	defer rows.Close()
	var out []Neighbor
	for rows.Next() {
		var n Neighbor
		if err := rows.Scan(&n.NodeID, &n.EdgeKind); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
