package handlers

import (
	"context"
	"errors"

	"github.com/agent-mem/agent-mem/internal/graph/hydrate"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TraceRepresentation is the current route-specific representation of one traced item.
type TraceRepresentation struct {
	Title   string
	Summary string
	Body    string
	Found   bool
}

// RenderTraceRepresentation uses the same renderers as resolve and search.
func RenderTraceRepresentation(ctx context.Context, db *pgxpool.Pool, route, mode, nodeID string, bodyBytes *int) (TraceRepresentation, error) {
	if route == "resolve" {
		rows, _, err := hydrate.Greedy(ctx, db, []hydrate.Candidate{{NodeID: nodeID, Score: 1}}, int(^uint(0)>>1)/4)
		if err != nil {
			return TraceRepresentation{}, err
		}
		if len(rows) == 0 {
			var exists bool
			if err := db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM graph.nodes WHERE id=$1 AND deleted_at IS NULL)`, nodeID).Scan(&exists); err != nil {
				return TraceRepresentation{}, err
			}
			return TraceRepresentation{Found: exists}, nil
		}
		body := rows[0].Body
		if bodyBytes != nil {
			body = hydrate.TruncateRunes(body, *bodyBytes)
		}
		return TraceRepresentation{Title: rows[0].Title, Body: body, Found: true}, nil
	}

	var result searchResult
	err := db.QueryRow(ctx, `
SELECT n.id,n.id,n.type,COALESCE(n.title,''),COALESCE(n.url,''),COALESCE(ai.summary,''),n.created_at
FROM graph.nodes n
LEFT JOIN graph.artifact_index ai ON ai.node_id=n.id
WHERE n.id=$1 AND n.deleted_at IS NULL`, nodeID).Scan(
		&result.NodeID, &result.ID, &result.Type, &result.Title, &result.URL, &result.Summary, &result.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return TraceRepresentation{}, nil
	}
	if err != nil {
		return TraceRepresentation{}, err
	}
	if mode != "hybrid" {
		return TraceRepresentation{Title: result.Title, Summary: result.Summary, Found: true}, nil
	}
	rows, err := hybridDisplay(ctx, db, []searchResult{result}, askerACL{noFilter: true})
	if err != nil {
		return TraceRepresentation{}, err
	}
	if len(rows) == 0 {
		return TraceRepresentation{}, nil
	}
	return TraceRepresentation{Title: rows[0].Title, Summary: rows[0].Summary, Found: true}, nil
}
