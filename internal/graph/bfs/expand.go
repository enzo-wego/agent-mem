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
	// Score is the embedding cosine similarity for SIMILAR edges (0 otherwise);
	// surfaced so the UI can explain *why* a semantically-matched row appears.
	Score float64
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

// similarThreadLimit / similarThreadMinCosine bound the semantic "related threads"
// lookup. 0.45 mirrors the hot-topic detector's tuned cosine threshold.
// ponytail: hard-coded; make configurable if the threshold needs per-deploy tuning.
const (
	similarThreadLimit     = 8
	similarThreadMinCosine = 0.45
)

// SimilarThreads returns other Slack *thread roots* whose indexed embedding is
// semantically close to nodeID's. The graph only has explicit reference/thread
// edges, so two threads about the same subject in different channels (e.g. several
// "blocked PK IP" incidents) never connect — this bridges them by topic. Edge kind
// "SIMILAR". Returns nil when nodeID has no embedding yet.
func (e *Expander) SimilarThreads(ctx context.Context, nodeID string) ([]Neighbor, error) {
	const q = `
WITH me AS (
  SELECT ai.embedding AS emb,
         REPLACE(n.scope,'slack:','') AS ch,
         COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3)) AS tt
  FROM graph.nodes n
  JOIN graph.artifact_index ai ON ai.node_id = n.id
  WHERE n.id = $1
)
SELECT n.id, (1.0 - (ai.embedding <=> me.emb)) AS cosine
FROM graph.artifact_index ai
JOIN graph.nodes n ON n.id = ai.node_id
CROSS JOIN me
WHERE n.type IN ('slack','slack_thread')
  AND n.deleted_at IS NULL
  AND n.scope NOT LIKE 'slack:D%'
  -- thread roots only: thread_ts equals the message's own ts
  AND COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3)) = split_part(n.id,':',3)
  -- exclude the opened thread itself (same channel + thread key)
  AND NOT (REPLACE(n.scope,'slack:','') = me.ch
       AND COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3)) = me.tt)
  AND (1.0 - (ai.embedding <=> me.emb)) >= $2
ORDER BY ai.embedding <=> me.emb
LIMIT $3`
	rows, err := e.db.Query(ctx, q, nodeID, similarThreadMinCosine, similarThreadLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Neighbor
	for rows.Next() {
		var id string
		var cosine float64
		if err := rows.Scan(&id, &cosine); err != nil {
			return nil, err
		}
		out = append(out, Neighbor{NodeID: id, EdgeKind: "SIMILAR", Score: cosine})
	}
	return out, rows.Err()
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
