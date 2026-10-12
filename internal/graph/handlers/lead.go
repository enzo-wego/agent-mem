package handlers

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/redact"
)

// Lead mode (R1a): an opt-in response shape for resolve, node and search that
// returns short, attributable, access-checked items. It runs only when the
// worker flag is on AND the request carries `X-Graph-Lead: 1`; every other
// request takes the unchanged code path.

const (
	leadHeader          = "X-Graph-Lead"
	leadSummaryMaxRunes = 600
	leadDefaultMaxItems = 30
	leadMaxItemsLimit   = 100
)

// leadGate decides, before any body is decoded, whether this request runs in
// lead mode. A request that asks for lead mode while the flag is off gets
// `X-Graph-Lead: off` and otherwise today's behavior.
func leadGate(w http.ResponseWriter, r *http.Request, enabled bool) bool {
	if r.Header.Get(leadHeader) != "1" {
		return false
	}
	if !enabled {
		w.Header().Set(leadHeader, "off")
		return false
	}
	return true
}

// errLeadLookup marks a lead-mode database failure: the request fails closed.
var errLeadLookup = errors.New("lead_lookup_failed")

func writeLeadLookupFailed(w http.ResponseWriter) {
	http.Error(w, "lead_lookup_failed", http.StatusServiceUnavailable)
}

// leadFields are the lead-mode additions to an item.
type leadFields struct {
	LeadSummary    string `json:"lead_summary"`
	LastActivityAt string `json:"last_activity_at"`
	AuthorSlackID  string `json:"author_slack_id"`
	AuthoredByBot  string `json:"authored_by_bot"`
	BotInThread    string `json:"bot_in_thread,omitempty"`
	ChannelID      string `json:"channel_id"`
}

// capRunes cuts s to at most n runes including a trailing "…".
func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// leadText masks (non-admin callers only) and then caps to 600 runes, so a
// sensitive token crossing the cut is masked whole before it can be split.
func leadText(admin bool, s string) string {
	if !admin {
		s = redact.Mask(s)
	}
	return capRunes(s, leadSummaryMaxRunes)
}

// leadRoot describes one live node: Root is the Slack thread root id for Slack
// message nodes ("" otherwise); Eligible follows the A2a rule.
type leadRoot struct {
	Root     string
	Eligible bool
}

// leadRoots maps ids to their thread roots and decides eligibility (A2a): the
// root exists, is not deleted, lives in the same scope as the member, passes
// the asker's ACL and, unless exempt, the epic/business filter. Ids that are not
// live nodes are absent from the result. Non-Slack nodes are always eligible
// (their own authorization is the caller's job).
func leadRoots(ctx context.Context, db *pgxpool.Pool, acl askerACL, ids, epicKeys []string, exempt map[string]bool) (map[string]leadRoot, error) {
	out := make(map[string]leadRoot, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := db.Query(ctx, `/* lead:roots */
SELECT n.id, n.type = 'slack', COALESCE(n.scope,''),
       'slack:' || split_part(n.id,':',2) || ':' || COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3))
FROM graph.nodes n WHERE n.id = ANY($1) AND n.deleted_at IS NULL`, ids)
	if err != nil {
		return nil, errLeadLookup
	}
	type member struct{ root, scope string }
	members := map[string]member{}
	var pairRoots, pairScopes []string
	seen := map[string]bool{}
	for rows.Next() {
		var id, scope, root string
		var slack bool
		if err := rows.Scan(&id, &slack, &scope, &root); err != nil {
			rows.Close()
			return nil, errLeadLookup
		}
		if !slack {
			out[id] = leadRoot{Eligible: true}
			continue
		}
		members[id] = member{root: root, scope: scope}
		if key := root + "\x00" + scope; !seen[key] {
			seen[key] = true
			pairRoots, pairScopes = append(pairRoots, root), append(pairScopes, scope)
		}
	}
	rows.Close()
	if rows.Err() != nil {
		return nil, errLeadLookup
	}
	if len(members) == 0 {
		return out, nil
	}
	rrows, err := db.Query(ctx, `/* lead:roots_eligible */
SELECT x.id, x.scope FROM unnest($1::text[], $2::text[]) AS x(id, scope)
JOIN graph.nodes r ON r.id = x.id
WHERE r.deleted_at IS NULL AND r.type = 'slack' AND COALESCE(r.scope,'') = x.scope
  AND `+aclVisibleSQL("r", 3, 4), pairRoots, pairScopes, acl.scopeArg(), acl.resolved)
	if err != nil {
		return nil, errLeadLookup
	}
	ok := map[string]bool{}
	for rrows.Next() {
		var id, scope string
		if err := rrows.Scan(&id, &scope); err != nil {
			rrows.Close()
			return nil, errLeadLookup
		}
		ok[id+"\x00"+scope] = true
	}
	rrows.Close()
	if rrows.Err() != nil {
		return nil, errLeadLookup
	}
	var needEpic []string
	needSeen := map[string]bool{}
	for _, m := range members {
		if ok[m.root+"\x00"+m.scope] && !exempt[m.root] && !needSeen[m.root] {
			needSeen[m.root] = true
			needEpic = append(needEpic, m.root)
		}
	}
	inEpic, err := epicMembersTagged(ctx, db, "/* lead:epic */", epicKeys, needEpic)
	if err != nil {
		return nil, errLeadLookup
	}
	for id, m := range members {
		eligible := ok[m.root+"\x00"+m.scope] && (exempt[m.root] || inEpic[m.root])
		out[id] = leadRoot{Root: m.root, Eligible: eligible}
	}
	return out, nil
}

// leadItem is one item to enrich: Root is its eligible thread root for Slack
// message nodes, "" otherwise.
type leadItem struct{ ID, Root string }

type leadNodeData struct {
	typ, scope, authorSlack, aiSummary string
	updated                            time.Time
	hasPerson, isBot                   bool
}

// leadEnrich computes the A4 fields for items. Slack items take every value
// from their eligible root (A2a); the caller must have verified eligibility.
// Any query failure returns errLeadLookup.
func leadEnrich(ctx context.Context, db *pgxpool.Pool, acl askerACL, admin bool, items []leadItem) (map[string]*leadFields, error) {
	out := make(map[string]*leadFields, len(items))
	if len(items) == 0 {
		return out, nil
	}
	var dataIDs []string
	seen := map[string]bool{}
	for _, it := range items {
		id := it.ID
		if it.Root != "" {
			id = it.Root
		}
		if !seen[id] {
			seen[id] = true
			dataIDs = append(dataIDs, id)
		}
	}
	rows, err := db.Query(ctx, `/* lead:enrich */
SELECT n.id, n.type, COALESCE(n.scope,''), n.updated_at, COALESCE(p.slack_user_id,''), p.id IS NOT NULL,
       (COALESCE(p.is_bot,false) OR ab.slack_user_id IS NOT NULL),
       COALESCE(ai.summary,'')
FROM graph.nodes n
LEFT JOIN graph.people p ON p.id = n.author_person_id
LEFT JOIN graph.answer_bots ab ON ab.slack_user_id = p.slack_user_id
LEFT JOIN graph.artifact_index ai ON ai.node_id = n.id
WHERE n.id = ANY($1) AND n.deleted_at IS NULL`, dataIDs)
	if err != nil {
		return nil, errLeadLookup
	}
	data := make(map[string]leadNodeData, len(dataIDs))
	for rows.Next() {
		var id string
		var d leadNodeData
		if err := rows.Scan(&id, &d.typ, &d.scope, &d.updated, &d.authorSlack, &d.hasPerson, &d.isBot, &d.aiSummary); err != nil {
			rows.Close()
			return nil, errLeadLookup
		}
		data[id] = d
	}
	rows.Close()
	if rows.Err() != nil {
		return nil, errLeadLookup
	}
	for _, id := range dataIDs {
		if _, ok := data[id]; !ok {
			return nil, errLeadLookup
		}
	}

	// One aggregate for every Slack root of the response (A5).
	type agg struct {
		max                time.Time
		bot, noPerson, any bool
	}
	aggs := map[string]agg{}
	var scopes, threads []string
	pairSeen := map[string]bool{}
	for _, it := range items {
		if it.Root == "" {
			continue
		}
		d := data[it.Root]
		ts := threadTSOfRoot(it.Root)
		if key := d.scope + "\x00" + ts; !pairSeen[key] {
			pairSeen[key] = true
			scopes, threads = append(scopes, d.scope), append(threads, ts)
		}
	}
	if len(scopes) > 0 {
		arows, err := db.Query(ctx, `/* lead:aggregate */
SELECT x.scope, x.thread_ts, max(n.updated_at),
       COALESCE(bool_or(COALESCE(p.is_bot,false) OR ab.slack_user_id IS NOT NULL), false),
       COALESCE(bool_or(p.id IS NULL), false)
FROM unnest($1::text[], $2::text[]) AS x(scope, thread_ts)
JOIN graph.nodes n ON n.scope = x.scope
 AND COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3)) = x.thread_ts
LEFT JOIN graph.people p ON p.id = n.author_person_id
LEFT JOIN graph.answer_bots ab ON ab.slack_user_id = p.slack_user_id
WHERE n.deleted_at IS NULL AND n.type = 'slack' AND `+aclVisibleSQL("n", 3, 4)+`
GROUP BY x.scope, x.thread_ts`, scopes, threads, acl.scopeArg(), acl.resolved)
		if err != nil {
			return nil, errLeadLookup
		}
		for arows.Next() {
			var scope, ts string
			var a agg
			if err := arows.Scan(&scope, &ts, &a.max, &a.bot, &a.noPerson); err != nil {
				arows.Close()
				return nil, errLeadLookup
			}
			a.any = true
			aggs[scope+"\x00"+ts] = a
		}
		arows.Close()
		if arows.Err() != nil {
			return nil, errLeadLookup
		}
	}

	for _, it := range items {
		id := it.ID
		if it.Root != "" {
			id = it.Root
		}
		d := data[id]
		f := &leadFields{AuthorSlackID: d.authorSlack}
		switch {
		case !d.hasPerson:
			f.AuthoredByBot = "unknown"
		case d.isBot:
			f.AuthoredByBot = "bot"
		default:
			f.AuthoredByBot = "human"
		}
		summary := d.aiSummary
		last := d.updated
		if it.Root != "" {
			f.BotInThread = "unknown"
			if a, ok := aggs[d.scope+"\x00"+threadTSOfRoot(it.Root)]; ok {
				last = a.max
				switch {
				case a.bot:
					f.BotInThread = "yes"
				case a.noPerson:
					f.BotInThread = "unknown"
				default:
					f.BotInThread = "no"
				}
			}
		}
		f.LeadSummary = leadText(admin, summary)
		f.LastActivityAt = last.UTC().Format(time.RFC3339)
		if d.typ == "slack" || d.typ == "slack_file" {
			f.ChannelID = strings.Replace(d.scope, "slack:", "", 1)
		}
		out[it.ID] = f
	}
	return out, nil
}

// threadTSOfRoot returns the ts part of "slack:<channel>:<ts>".
func threadTSOfRoot(root string) string {
	parts := strings.SplitN(root, ":", 3)
	if len(parts) < 3 {
		return ""
	}
	return parts[2]
}
