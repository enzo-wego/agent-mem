package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type scopeBuilder interface {
	For(context.Context, int) ([]string, error)
}

// adminHeader unlocks unfiltered reads. Only the exact value "1" counts.
const adminHeader = "X-Graph-Admin"

// orgScopeSources are the scope source prefixes (text before the first ':')
// every employee with an eeid may read (decision D1). Matched exactly.
var orgScopeSources = []string{"jira", "github", "confluence"}

func orgScope(scope string) bool {
	src := scope
	if i := strings.IndexByte(scope, ':'); i >= 0 {
		src = scope[:i]
	}
	for _, s := range orgScopeSources {
		if src == s {
			return true
		}
	}
	return false
}

// orgScopeSQL is the SQL twin of orgScope for the column col.
func orgScopeSQL(col string) string {
	quoted := make([]string, len(orgScopeSources))
	for i, s := range orgScopeSources {
		quoted[i] = "'" + s + "'"
	}
	return fmt.Sprintf("split_part(%s, ':', 1) IN (%s)", col, strings.Join(quoted, ","))
}

// askerError is a rejected asker; code is the stable machine-readable reason.
type askerError struct {
	status int
	code   string
}

func (e *askerError) Error() string { return e.code }

func isAdminRequest(r *http.Request) bool { return r.Header.Get(adminHeader) == "1" }

// askerIdentities are the identities a request asserts.
type askerIdentities struct {
	header  string // X-Asker-User, trimmed
	slackID string // body asker_slack_id, trimmed
	eeid    int    // body asker_eeid; 0 = absent
}

// resolveAsker applies the asker policy (plan steps 1-8). Admin returns the
// unfiltered ACL; otherwise the asker must resolve to exactly one eeid.
func resolveAsker(ctx context.Context, db *pgxpool.Pool, bld scopeBuilder, r *http.Request, ids askerIdentities) (askerACL, int, string, *askerError) {
	if isAdminRequest(r) {
		log.Printf("graph: admin read route=%s caller=%q", r.URL.Path, r.Header.Get("X-Graph-Caller"))
		return askerACL{noFilter: true}, 0, "admin", nil
	}
	ids.header = strings.TrimSpace(ids.header)
	ids.slackID = strings.TrimSpace(ids.slackID)
	if ids.header == "" && ids.slackID == "" && ids.eeid == 0 {
		return askerACL{}, 0, "rejected", &askerError{http.StatusForbidden, "asker_required"}
	}
	headerEEID := int64(0)
	headerDigits := ids.header != "" && isAllDigits(ids.header)
	if headerDigits {
		n, err := strconv.ParseInt(ids.header, 10, 32)
		if err != nil || n <= 0 {
			return askerACL{}, 0, "rejected", &askerError{http.StatusBadRequest, "asker_invalid"}
		}
		headerEEID = n
	}
	if ids.eeid < 0 || ids.eeid > math.MaxInt32 {
		return askerACL{}, 0, "rejected", &askerError{http.StatusBadRequest, "asker_invalid"}
	}

	var results []identityResult
	if ids.header != "" {
		if headerDigits {
			results = append(results, lookupPersonEEIDs(ctx, db, `eeid = $1`, int32(headerEEID)))
		} else {
			results = append(results, lookupPersonEEIDs(ctx, db,
				`slack_user_id = $1 OR email = $1 OR github_login = $1 OR jira_account_id = $1`, ids.header))
		}
	}
	if ids.slackID != "" {
		results = append(results, lookupPersonEEIDs(ctx, db, `slack_user_id = $1`, ids.slackID))
	}
	if ids.eeid != 0 {
		results = append(results, lookupPersonEEIDs(ctx, db, `eeid = $1`, int32(ids.eeid)))
	}
	reject := func(status int, code string) (askerACL, int, string, *askerError) {
		return askerACL{}, 0, "rejected", &askerError{status, code}
	}
	for _, res := range results {
		if res.err != nil {
			return reject(http.StatusServiceUnavailable, "acl_unavailable")
		}
	}
	eeid := 0
	for _, res := range results {
		if len(res.eeids) == 0 {
			return reject(http.StatusForbidden, "asker_unknown")
		}
	}
	for _, res := range results {
		for _, e := range res.eeids {
			if eeid != 0 && e != eeid {
				return reject(http.StatusBadRequest, "asker_conflict")
			}
			eeid = e
		}
	}
	scopes, err := bld.For(ctx, eeid)
	if err != nil {
		return reject(http.StatusServiceUnavailable, "acl_unavailable")
	}
	set := map[string]bool{"public": true}
	for _, s := range scopes {
		set[s] = true
	}
	return askerACL{resolved: true, scopes: set}, eeid, "filtered", nil
}

func isAllDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

type identityResult struct {
	eeids []int // distinct eeids of the matching people, after following merges
	err   error
}

const maxMergeHops = 3

// lookupPersonEEIDs returns the distinct eeids of people matching where (one
// arg), following merged_into up to maxMergeHops. Rows that end in a loop, a
// missing target or a NULL eeid contribute nothing, so an identity whose rows
// all fail has no eeids (asker_unknown).
func lookupPersonEEIDs(ctx context.Context, db *pgxpool.Pool, where string, arg any) identityResult {
	rows, err := db.Query(ctx, `SELECT eeid, merged_into FROM graph.people WHERE `+where+` LIMIT 8`, arg)
	if err != nil {
		return identityResult{err: err}
	}
	type row struct {
		eeid   *int32
		merged *int64
	}
	var found []row
	for rows.Next() {
		var rw row
		if err := rows.Scan(&rw.eeid, &rw.merged); err != nil {
			rows.Close()
			return identityResult{err: err}
		}
		found = append(found, rw)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return identityResult{err: err}
	}
	var out []int
	seen := map[int]bool{}
	for _, rw := range found {
		cur := rw
		hops := 0
		ok := true
		for cur.merged != nil {
			hops++
			if hops > maxMergeHops {
				ok = false
				break
			}
			var next row
			err := db.QueryRow(ctx, `SELECT eeid, merged_into FROM graph.people WHERE id = $1`, *cur.merged).Scan(&next.eeid, &next.merged)
			if errors.Is(err, pgx.ErrNoRows) {
				ok = false
				break
			}
			if err != nil {
				return identityResult{err: err}
			}
			cur = next
		}
		if !ok || cur.eeid == nil {
			continue
		}
		if e := int(*cur.eeid); !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	return identityResult{eeids: out}
}

// requireAsker resolves the asker and, on rejection, writes the JSON error and
// records acl_mode on the request trace. ok=false means the handler must return.
func requireAsker(w http.ResponseWriter, r *http.Request, db *pgxpool.Pool, bld scopeBuilder, ids askerIdentities) (askerACL, int, bool) {
	if ids.header == "" {
		ids.header = r.Header.Get("X-Asker-User")
	}
	acl, eeid, mode, aerr := resolveAsker(r.Context(), db, bld, r, ids)
	if diag, _ := r.Context().Value(requestTraceDiagKey{}).(*requestTraceDiag); diag != nil {
		diag.aclMode = mode
	}
	if aerr != nil {
		writeAskerError(w, aerr)
		return askerACL{}, 0, false
	}
	return acl, eeid, true
}

func writeAskerError(w http.ResponseWriter, e *askerError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": e.code})
}

// scopeVisible reports whether a node with the given scope is readable by a
// filtered asker (mirrored in SQL by aclVisibleSQL). Unscoped (NULL/empty) and
// "public" nodes, org scopes and the asker's member scopes are visible.
func scopeVisible(scope *string, scopeSet map[string]bool, noFilter bool) bool {
	if noFilter {
		return true
	}
	if scope == nil || *scope == "" || *scope == "public" {
		return true
	}
	return scopeSet[*scope] || orgScope(*scope)
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
   AND (p.scope IS NULL OR p.scope = '' OR p.scope = 'public' OR p.scope = ANY($%d::text[]) OR %s)
)`, alias, scopeParam, orgScopeSQL("p.scope"))
}

// ponytail: per-row EXISTS over REFERENCES parents; materialize if it shows up in p95.
func aclVisibleSQL(alias string, scopeParam, resolvedParam int) string {
	return fmt.Sprintf(`($%d::text[] IS NULL OR
 (%s.type NOT IN ('slack_file','jira_attachment') AND
  (%s.scope IS NULL OR %s.scope = '' OR %s.scope = 'public' OR %s.scope = ANY($%d::text[]) OR %s)) OR
 (%s.type IN ('slack_file','jira_attachment') AND $%d::boolean AND %s))`,
		scopeParam, alias, alias, alias, alias, alias, scopeParam, orgScopeSQL(alias+".scope"),
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
