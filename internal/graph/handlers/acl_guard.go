package handlers

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/acl"
)

// askerScopeSet resolves the X-Asker-User header to an eeid and builds the set
// of scopes that asker may read (their real memberships plus "public").
//
// An absent (trimmed empty) header is the trusted unfiltered view. A present
// header is always filtered; unresolved identities and lookup/build errors see
// only public and unscoped nodes. Identity remains advisory; see router.go.
func askerScopeSet(ctx context.Context, db *pgxpool.Pool, bld *acl.Builder, askerRef string) (eeid int, scopeSet map[string]bool, noFilter bool) {
	if strings.TrimSpace(askerRef) == "" {
		return 0, nil, true
	}
	eeid, err := lookupAsker(ctx, db, askerRef)
	if err != nil || eeid == 0 {
		return 0, map[string]bool{"public": true}, false
	}
	scopeSet = map[string]bool{"public": true}
	scopes, err := bld.For(ctx, eeid)
	if err != nil {
		return eeid, scopeSet, false // fail closed
	}
	for _, s := range scopes {
		scopeSet[s] = true
	}
	return eeid, scopeSet, false
}

// scopeVisible reports whether a node with the given scope is readable. It is
// the single scope rule shared by /search (mirrored in SQL), /resolve, /node and
// /neighbors. For header-based endpoints noFilter is true only when the trimmed
// header is absent; /resolve retains its numeric eeid 0 convention. Unscoped
// (NULL/empty) and "public" nodes are visible to everyone; other scopes require membership.
func scopeVisible(scope *string, scopeSet map[string]bool, noFilter bool) bool {
	if noFilter {
		return true
	}
	if scope == nil || *scope == "" || *scope == "public" {
		return true
	}
	return scopeSet[*scope]
}

type askerACL struct {
	noFilter bool
	resolved bool
	scopes   map[string]bool
}

func attachmentType(nodeType string) bool {
	return nodeType == "slack_file" || nodeType == "jira_attachment"
}

func (a askerACL) scopeArg() any {
	if a.noFilter {
		return nil
	}
	scopes := make([]string, 0, len(a.scopes)+1)
	scopes = append(scopes, "public")
	for s := range a.scopes {
		if s != "public" && a.scopes[s] {
			scopes = append(scopes, s)
		}
	}
	return scopes
}

func parentVisibleSQL(alias string, scopeParam int) string {
	return fmt.Sprintf(`EXISTS (
 SELECT 1 FROM graph.edges e JOIN graph.nodes p ON p.id = e.from_node_id
 WHERE e.to_node_id = %s.id AND e.kind = 'REFERENCES'
   AND p.deleted_at IS NULL AND p.type NOT IN ('slack_file','jira_attachment')
   AND (p.scope IS NULL OR p.scope = '' OR p.scope = 'public' OR p.scope = ANY($%d::text[]))
)`, alias, scopeParam)
}

// ponytail: per-row EXISTS over REFERENCES parents; materialize if it shows up in p95.
func aclVisibleSQL(alias string, scopeParam, resolvedParam int) string {
	return fmt.Sprintf(`($%d::text[] IS NULL OR
 (%s.type NOT IN ('slack_file','jira_attachment') AND
  (%s.scope IS NULL OR %s.scope = '' OR %s.scope = 'public' OR %s.scope = ANY($%d::text[]))) OR
 (%s.type IN ('slack_file','jira_attachment') AND $%d::boolean AND %s))`,
		scopeParam, alias, alias, alias, alias, alias, scopeParam,
		alias, resolvedParam, parentVisibleSQL(alias, scopeParam))
}

func nodeVisible(ctx context.Context, db *pgxpool.Pool, a askerACL, id, nodeType string, scope *string) (bool, error) {
	if a.noFilter {
		return true, nil
	}
	if !attachmentType(nodeType) {
		return scopeVisible(scope, a.scopes, false), nil
	}
	if !a.resolved {
		return false, nil
	}
	var visible bool
	err := db.QueryRow(ctx, `SELECT `+parentVisibleSQL("n", 2)+` FROM graph.nodes n WHERE n.id=$1`, id, a.scopeArg()).Scan(&visible)
	return err == nil && visible, err
}

// Query only attachment candidates, after callers have closed their result rows.
func visibleAttachments(ctx context.Context, db *pgxpool.Pool, a askerACL, ids []string) (map[string]bool, error) {
	out := make(map[string]bool, len(ids))
	if a.noFilter {
		for _, id := range ids {
			out[id] = true
		}
		return out, nil
	}
	if !a.resolved || len(ids) == 0 {
		return out, nil
	}
	rows, err := db.Query(ctx, `SELECT n.id FROM graph.nodes n WHERE n.id=ANY($1) AND n.type IN ('slack_file','jira_attachment') AND `+aclVisibleSQL("n", 2, 3), ids, a.scopeArg(), a.resolved)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
