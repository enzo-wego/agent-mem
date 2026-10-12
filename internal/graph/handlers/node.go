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
	db          *pgxpool.Pool
	aclBld      scopeBuilder
	leadEnabled bool
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

// WithLeadMode sets whether lead mode may run (worker flag GRAPH_LEAD_MODE_ENABLED).
func (h *Node) WithLeadMode(enabled bool) *Node {
	h.leadEnabled = enabled
	return h
}

// leadNodeResponse is the lead-mode node shape: no body, no edges.
type leadNodeResponse struct {
	NodeID     string `json:"node_id"`
	Type       string `json:"type"`
	URL        string `json:"url"`
	Title      string `json:"title"`
	Summary    string `json:"summary,omitempty"`
	AuthorName string `json:"author_name,omitempty"`
	UpdatedAt  string `json:"updated_at"`
	*leadFields
}

// serveLead answers a lead-mode request once the node is known visible. A
// Slack reply stays a reply id; its thread-derived values come from an eligible
// root, otherwise the node is hidden (404).
func (h *Node) serveLead(w http.ResponseWriter, r *http.Request, acl askerACL, n nodeResponse, updatedAt time.Time) {
	ctx := r.Context()
	admin := isAdminRequest(r)
	item := leadItem{ID: n.NodeID}
	if n.Type == "slack" {
		roots, err := leadRoots(ctx, h.db, acl, []string{n.NodeID}, nil, nil)
		if err != nil {
			writeLeadLookupFailed(w)
			return
		}
		lr, ok := roots[n.NodeID]
		if !ok || !lr.Eligible {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		item.Root = lr.Root
	}
	fields, err := leadEnrich(ctx, h.db, acl, admin, []leadItem{item})
	if err != nil {
		writeLeadLookupFailed(w)
		return
	}
	out := leadNodeResponse{
		NodeID: n.NodeID, Type: n.Type, URL: n.URL, Title: n.Title,
		Summary: leadText(admin, n.Summary), AuthorName: n.AuthorName,
		UpdatedAt: updatedAt.Format(time.RFC3339), leadFields: fields[n.NodeID],
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (h *Node) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	lead := leadGate(w, r, h.leadEnabled)
	acl, _, ok := requireAsker(w, r, h.db, h.aclBld, askerIdentities{})
	if !ok {
		return
	}
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
	// Hidden nodes return 404 so their existence/body is not disclosed.
	if visible, err := nodeVisible(ctx, h.db, acl, resp.NodeID, resp.Type, scope); err != nil || !visible {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if lead {
		h.serveLead(w, r, acl, resp, updatedAt)
		return
	}
	resp.UpdatedAt = updatedAt.Format(time.RFC3339)
	resp.EdgesIn, _ = h.edges(ctx, resp.NodeID, "in", acl)
	resp.EdgesOut, _ = h.edges(ctx, resp.NodeID, "out", acl)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *Node) edges(ctx context.Context, id string, dir string, acl askerACL) ([]edgeRef, error) {
	var q string
	if dir == "in" {
		q = `SELECT from_node_id, kind FROM graph.edges WHERE to_node_id=$1`
	} else {
		q = `SELECT to_node_id, kind FROM graph.edges WHERE from_node_id=$1`
	}
	args := []any{id}
	if !acl.noFilter {
		// Filter the opposite endpoint through the shared node visibility rule.
		if dir == "in" {
			q = `SELECT e.from_node_id, e.kind FROM graph.edges e
LEFT JOIN graph.nodes n ON n.id = e.from_node_id WHERE e.to_node_id = $1`
		} else {
			q = `SELECT e.to_node_id, e.kind FROM graph.edges e
LEFT JOIN graph.nodes n ON n.id = e.to_node_id WHERE e.from_node_id = $1`
		}
		q += ` AND ` + aclVisibleSQL("n", 2, 3)
		args = append(args, acl.scopeArg(), acl.resolved)
	}
	rows, err := h.db.Query(ctx, q, args...)
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
