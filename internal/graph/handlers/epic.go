package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/acl"
)

// epicMemberLimit caps members listed per type. Slack replies are folded into
// their thread root (membership rows exist for replies so scoping works, but
// the listing shows threads, not messages).
const epicMemberLimit = 100

type epicMember struct {
	NodeID     string     `json:"node_id"`
	Title      string     `json:"title"`
	URL        string     `json:"url,omitempty"`
	Via        string     `json:"via"`
	Confidence float64    `json:"confidence"`
	Status     string     `json:"status,omitempty"`
	CreatedAt  *time.Time `json:"created_at,omitempty"`
}

type epicResponse struct {
	EpicKey  string                  `json:"epic_key"`
	NodeID   string                  `json:"node_id"`
	Title    string                  `json:"title"`
	URL      string                  `json:"url,omitempty"`
	Status   string                  `json:"status,omitempty"`
	FirstAt  *time.Time              `json:"first_at,omitempty"`
	LastAt   *time.Time              `json:"last_at,omitempty"`
	Members  map[string][]epicMember `json:"members"`
	Total    int                     `json:"total"`
	Replies  int                     `json:"replies"`
	ByVia    map[string]int          `json:"by_via"`
	Business bool                    `json:"business,omitempty"`
	// Round 3: the standing brief from graph.epic_briefs, when one exists.
	Brief          string          `json:"brief,omitempty"`
	Highlights     json.RawMessage `json:"highlights,omitempty"`
	OpenItems      json.RawMessage `json:"open_items,omitempty"`
	BriefUpdatedAt *time.Time      `json:"brief_updated_at,omitempty"`
	// ResolvedFrom is the issue key the request named when it resolved to
	// this epic through graph.jira_epic_map.
	ResolvedFrom string `json:"resolved_from,omitempty"`
}

// Epic handles GET /api/graph/epic/{key}: the epic (or `business:payments`)
// with its members grouped by node type, each with how it joined (`via`), the
// activity window from the epic's own membership row, plus the standing brief
// (graph.epic_briefs) when one exists. A key with no membership rows is a 404
// (unknown epic or the rebuild has not run yet).
//
// Access control: a request carrying a non-empty X-Asker-User sees only what
// that asker can read. An unresolvable header fails closed to public-only.
type Epic struct {
	db     *pgxpool.Pool
	aclBld *acl.Builder
}

func NewEpic(db *pgxpool.Pool) *Epic {
	return &Epic{db: db, aclBld: acl.NewBuilder(db, 5*time.Minute)}
}

// epicSourceScopes maps node id -> scope for the given ids, including
// soft-deleted rows. Ids without a row are absent. Replaceable in tests.
var epicSourceScopes = func(ctx context.Context, db *pgxpool.Pool, ids []string) (map[string]*string, error) {
	rows, err := db.Query(ctx, `SELECT id, scope FROM graph.nodes WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]*string, len(ids))
	for rows.Next() {
		var id string
		var scope *string
		if err := rows.Scan(&id, &scope); err != nil {
			return nil, err
		}
		out[id] = scope
	}
	return out, rows.Err()
}

// briefSourcesVisible reports whether every source of a stored brief is
// readable by the asker. A query error or a source with no row withholds.
func briefSourcesVisible(ctx context.Context, db *pgxpool.Pool, sources []string, scopeSet map[string]bool) bool {
	if len(sources) == 0 {
		return true
	}
	scopes, err := epicSourceScopes(ctx, db, sources)
	if err != nil {
		return false
	}
	for _, id := range sources {
		sc, ok := scopes[id]
		if !ok || !scopeVisible(sc, scopeSet, false) {
			return false
		}
	}
	return true
}

func (h *Epic) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	key := chi.URLParam(r, "key")
	if dec, err := url.PathUnescape(key); err == nil {
		key = dec
	}
	key = strings.TrimSpace(key)
	if key == "" {
		http.Error(w, "key required", http.StatusBadRequest)
		return
	}
	nodeID := "jira:" + strings.ToUpper(key)
	business := false
	if strings.EqualFold(key, businessRootID) || strings.EqualFold(key, "payments") {
		key, nodeID, business = businessRootID, businessRootID, true
	} else {
		key = strings.ToUpper(key)
	}

	header := strings.TrimSpace(r.Header.Get("X-Asker-User"))
	noFilter := header == ""
	var scopeSet map[string]bool
	if !noFilter {
		eeid, set := askerScopeSet(ctx, h.db, h.aclBld, header)
		if eeid == 0 {
			set = map[string]bool{"public": true} // unresolved asker: fail closed
		}
		scopeSet = set
	}

	// requested stays the key as asked (upper-cased): every 404 below names
	// it, never an alias-resolved epic, so a denial cannot reveal the epic.
	requested := key
	resp := epicResponse{
		EpicKey: key, NodeID: nodeID, Business: business,
		Members: map[string][]epicMember{}, ByVia: map[string]int{},
	}
	var epicScope *string
	lookup := func(epicKey, epicNode string) error {
		return h.db.QueryRow(ctx, `
SELECT COALESCE(n.title,''), COALESCE(n.url,''), COALESCE(n.metadata->>'status',''),
       m.first_at, m.last_at, n.scope
FROM graph.epic_membership m
JOIN graph.nodes n ON n.id = m.node_id
WHERE m.epic_key = $1 AND m.node_id = $2`, epicKey, epicNode).Scan(
			&resp.Title, &resp.URL, &resp.Status, &resp.FirstAt, &resp.LastAt, &epicScope)
	}
	err := lookup(key, nodeID)
	if err != nil && !business {
		// Not a known epic: an issue key resolves to its epic through
		// jira_epic_map (a laptop session only knows its branch's issue).
		var epicKey string
		if e := h.db.QueryRow(ctx, `SELECT epic_key FROM graph.jira_epic_map WHERE issue_key = $1 AND epic_key <> ''`,
			requested).Scan(&epicKey); e == nil && !strings.EqualFold(epicKey, requested) {
			epicKey = strings.ToUpper(epicKey)
			if err = lookup(epicKey, "jira:"+epicKey); err == nil {
				key, nodeID = epicKey, "jira:"+epicKey
				resp.EpicKey, resp.NodeID, resp.ResolvedFrom = key, nodeID, requested
			}
		}
	}
	if err != nil || !scopeVisible(epicScope, scopeSet, noFilter) {
		http.Error(w, "unknown epic "+requested, http.StatusNotFound)
		return
	}

	if !noFilter {
		// The window aggregates over all members regardless of scope.
		resp.FirstAt, resp.LastAt = nil, nil
	}

	rows, err := h.db.Query(ctx, `
SELECT n.id, n.type, COALESCE(n.title,''), COALESCE(n.url,''), m.via, m.confidence,
       COALESCE(n.metadata->>'status',''), COALESCE(n.created_at, n.first_seen_at),
       (n.type IN ('slack','slack_thread')
        AND COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3)) <> split_part(n.id,':',3)) AS is_reply, n.scope
FROM graph.epic_membership m
JOIN graph.nodes n ON n.id = m.node_id AND n.deleted_at IS NULL
WHERE m.epic_key = $1 AND m.node_id <> $2
ORDER BY m.last_at DESC NULLS LAST, n.id`, key, nodeID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var (
			m       epicMember
			typ     string
			created time.Time
			isReply bool
			scope   *string
		)
		if err := rows.Scan(&m.NodeID, &typ, &m.Title, &m.URL, &m.Via, &m.Confidence,
			&m.Status, &created, &isReply, &scope); err != nil {
			continue
		}
		if !scopeVisible(scope, scopeSet, noFilter) {
			continue
		}
		resp.Total++
		resp.ByVia[m.Via]++
		if isReply {
			resp.Replies++
			continue
		}
		if len(resp.Members[typ]) >= epicMemberLimit {
			continue
		}
		c := created
		m.CreatedAt = &c
		if (typ == "slack" || typ == "slack_thread") && m.URL == "" {
			m.URL = slackPermalink(m.NodeID)
		}
		resp.Members[typ] = append(resp.Members[typ], m)
	}
	if rows.Err() != nil {
		http.Error(w, rows.Err().Error(), http.StatusInternalServerError)
		return
	}
	if br, ok := loadEpicBrief(ctx, h.db, key); ok && br.Brief != "" &&
		(noFilter || briefSourcesVisible(ctx, h.db, br.Sources, scopeSet)) {
		resp.Brief, resp.Highlights, resp.OpenItems = br.Brief, br.Highlights, br.OpenItems
		u := br.UpdatedAt
		resp.BriefUpdatedAt = &u
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
