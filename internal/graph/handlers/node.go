package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/acl"
)

// Node handles GET /api/graph/node?url=... or ?id=...
type Node struct {
	db     *pgxpool.Pool
	aclBld *acl.Builder
}

// NewNode creates a new Node handler.
func NewNode(db *pgxpool.Pool) *Node {
	return &Node{db: db, aclBld: acl.NewBuilder(db, 5*time.Minute)}
}

type nodeResponse struct {
	NodeID     string    `json:"node_id"`
	Type       string    `json:"type"`
	URL        string    `json:"url"`
	Title      string    `json:"title"`
	Summary    string    `json:"summary,omitempty"`
	Body       string    `json:"body,omitempty"`
	AuthorName string    `json:"author_name,omitempty"`
	UpdatedAt  string    `json:"updated_at"`
	EdgesIn    []edgeRef `json:"edges_in"`
	EdgesOut   []edgeRef `json:"edges_out"`
}

type edgeRef struct {
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	Kind string `json:"kind"`
}

func (h *Node) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	url := r.URL.Query().Get("url")
	id := r.URL.Query().Get("id")
	if url == "" && id == "" {
		http.Error(w, "url or id required", http.StatusBadRequest)
		return
	}
	// A pasted Slack permalink resolves to its canonical node id from the path
	// alone, so ?thread_ts=…&cid=… and #fragments (which Slack's "Copy link"
	// always appends) don't defeat the match. Looking up by id drops the
	// dependence on the stored url column for Slack entirely.
	if id == "" {
		if sid := slackNodeIDFromURL(url); sid != "" {
			id, url = sid, ""
		}
	}
	// Non-Slack fallback: also match the url with its ?query/#fragment stripped,
	// so a Jira/Confluence/GitHub link with tracking params resolves. The raw url
	// is compared too, so a stored url that legitimately carries a query still
	// matches.
	urlStripped := stripQueryFragment(url)
	// Slack thread titles live in graph.thread_summaries, not n.title (often
	// empty); fall back to the summary so the node detail shows readable text,
	// never a raw slack:CHANNEL:TS id. Same join the neighbor rows use.
	row := h.db.QueryRow(ctx, `
SELECT n.id, n.type, COALESCE(n.url,''),
       COALESCE(
         NULLIF(n.title,''),
         CASE WHEN n.type IN ('slack','slack_thread') THEN NULLIF(ts.summary,'') END,
         ''),
       COALESCE(ai.summary, ''),
       COALESCE(ab.body_full, ''),
       COALESCE(p.display_name, ''),
       n.updated_at,
       n.scope
FROM graph.nodes n
LEFT JOIN graph.artifact_index ai ON ai.node_id = n.id
LEFT JOIN graph.artifact_bodies ab ON ab.node_id = n.id
LEFT JOIN graph.people p ON p.id = n.author_person_id
LEFT JOIN graph.thread_summaries ts
  ON ts.channel_id = REPLACE(n.scope,'slack:','')
  AND ts.thread_ts = COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3))
WHERE ($1 = '' OR n.url = $1 OR n.url = $3) AND ($2 = '' OR n.id = $2)
  AND n.deleted_at IS NULL
LIMIT 1
`, url, id, urlStripped)
	var resp nodeResponse
	var updatedAt time.Time
	var scope *string
	err := row.Scan(&resp.NodeID, &resp.Type, &resp.URL, &resp.Title,
		&resp.Summary, &resp.Body, &resp.AuthorName, &updatedAt, &scope)
	if errors.Is(err, pgx.ErrNoRows) || err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// ACL: a real asker (eeid != 0) may only read nodes in scope; eeid 0 is the
	// trusted unfiltered view. Hidden nodes return 404 (not 403) so their
	// existence/body is not disclosed by id or url.
	eeid, scopeSet := askerScopeSet(ctx, h.db, h.aclBld, r.Header.Get("X-Asker-User"))
	if !scopeVisible(scope, scopeSet, eeid == 0) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	resp.UpdatedAt = updatedAt.Format(time.RFC3339)
	resp.EdgesIn, _ = h.edges(ctx, resp.NodeID, "in")
	resp.EdgesOut, _ = h.edges(ctx, resp.NodeID, "out")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *Node) edges(ctx context.Context, id string, dir string) ([]edgeRef, error) {
	var q string
	if dir == "in" {
		q = `SELECT from_node_id, kind FROM graph.edges WHERE to_node_id=$1`
	} else {
		q = `SELECT to_node_id, kind FROM graph.edges WHERE from_node_id=$1`
	}
	rows, err := h.db.Query(ctx, q, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []edgeRef
	for rows.Next() {
		var ref edgeRef
		var nbr string
		if err := rows.Scan(&nbr, &ref.Kind); err != nil {
			return nil, err
		}
		if dir == "in" {
			ref.From = nbr
		} else {
			ref.To = nbr
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}
