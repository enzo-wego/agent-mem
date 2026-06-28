package handlers

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/acl"
)

// askerScopeSet resolves the X-Asker-User header to an eeid and builds the set
// of scopes that asker may read (their real memberships plus "public").
//
// eeid 0 means no asker was asserted — the trusted dashboard/integration calling
// behind the API key — and the caller should treat it as the unfiltered view
// (see scopeVisible's noFilter). A real asker (eeid != 0) is always filtered,
// even with zero memberships. On an ACL build error we fail closed (the asker
// still gets only "public"/unscoped, never the whole graph). See Mount's trust
// note in router.go.
func askerScopeSet(ctx context.Context, db *pgxpool.Pool, bld *acl.Builder, askerRef string) (eeid int, scopeSet map[string]bool) {
	eeid = lookupAskerEEID(ctx, db, askerRef)
	if eeid == 0 {
		return 0, nil
	}
	scopeSet = map[string]bool{"public": true}
	scopes, err := bld.For(ctx, eeid)
	if err != nil {
		return eeid, scopeSet // fail closed
	}
	for _, s := range scopes {
		scopeSet[s] = true
	}
	return eeid, scopeSet
}

// scopeVisible reports whether a node with the given scope is readable. It is
// the single scope rule shared by /search (mirrored in SQL), /resolve, /node and
// /neighbors. noFilter is true only when no asker principal was asserted (eeid
// 0). Unscoped (NULL/empty) and "public" nodes are visible to everyone; any
// other scope requires explicit membership in scopeSet.
func scopeVisible(scope *string, scopeSet map[string]bool, noFilter bool) bool {
	if noFilter {
		return true
	}
	if scope == nil || *scope == "" || *scope == "public" {
		return true
	}
	return scopeSet[*scope]
}
