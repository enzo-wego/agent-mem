package handlers

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// epicScopePredicate is the WHERE fragment read endpoints add next to the ACL
// predicate. %s is the positional parameter holding the epic_key array (nil
// = unscoped). Keys are graph.epic_membership.epic_key values: a Jira epic
// key or businessRootID.
const epicScopePredicate = `(%[1]s::text[] IS NULL OR n.id IN
  (SELECT em.node_id FROM graph.epic_membership em WHERE em.epic_key = ANY(%[1]s)))`

// epicScopeKeys turns the repeatable `epic` param and `business` into the
// membership keys to scope to. nil means unscoped. An unknown business name
// is an error (a typo must not silently return the whole graph).
func epicScopeKeys(epics []string, business string) ([]string, error) {
	var keys []string
	for _, e := range epics {
		if e = strings.ToUpper(strings.TrimSpace(e)); e != "" {
			keys = append(keys, e)
		}
	}
	switch strings.ToLower(strings.TrimSpace(business)) {
	case "":
	case "payments":
		keys = append(keys, businessRootID)
	default:
		return nil, fmt.Errorf("unknown business %q (want payments)", business)
	}
	return keys, nil
}

// epicScopeFromQuery is epicScopeKeys over URL query params.
func epicScopeFromQuery(q url.Values) ([]string, error) {
	return epicScopeKeys(q["epic"], q.Get("business"))
}

// epicScopeArg is the SQL parameter for epicScopePredicate: a nil interface
// (SQL NULL) when unscoped, else the key array.
func epicScopeArg(keys []string) any {
	if len(keys) == 0 {
		return nil
	}
	return keys
}

// epicMembers returns the subset of ids that belong to any of keys. With no
// keys every id passes (unscoped).
func epicMembers(ctx context.Context, db *pgxpool.Pool, keys, ids []string) (map[string]bool, error) {
	out := make(map[string]bool, len(ids))
	if len(keys) == 0 {
		for _, id := range ids {
			out[id] = true
		}
		return out, nil
	}
	rows, err := db.Query(ctx, `
SELECT DISTINCT node_id FROM graph.epic_membership
WHERE epic_key = ANY($1) AND node_id = ANY($2)`, keys, ids)
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
	return out, rows.Err()
}
