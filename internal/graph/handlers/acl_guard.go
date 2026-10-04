package handlers

import (
	"context"
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
