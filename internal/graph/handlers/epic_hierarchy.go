package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// businessRootID is the graph node every board epic hangs off (seeded by
// migration 20260927103606). graph.epic_membership uses it as epic_key for
// the "everything payments" tier, and /search, /resolve, /neighbors accept
// `business=payments` to scope to it.
const businessRootID = "business:payments"

// businessRootProjectKey is the settings row naming the Jira project whose
// epics form the business root's subtree. Default "PAY" (the migration seeds
// it); dashboard-editable under Settings → Business root.
const businessRootProjectKey = "graph.business_root_project"

// epicLinkMethod tags the PART_OF edges this file owns, so the delete-stale
// pass never touches PART_OF edges written by anything else.
const epicLinkMethod = "jira-epic-link"

// Membership `via` values, in first-writer-wins order (see rebuildEpicMembership).
const (
	viaEpicSelf  = "epic_self"
	viaKey       = "key"
	viaTopicLink = "topic_link"
	viaEligible  = "eligible"
)

// topicLinkMinConfidence is the SAME_TOPIC edge confidence floor for a
// topic_link membership row.
const topicLinkMinConfidence = 0.8

// businessRootProject returns the Jira project key for the business root
// ("PAY" when the setting is absent or blank).
func businessRootProject(ctx context.Context, db *pgxpool.Pool) string {
	if v := strings.ToUpper(strings.TrimSpace(loadSetting(ctx, db, businessRootProjectKey))); v != "" {
		return v
	}
	return jiraBoardProject
}

// epicSet is the union of the board's epic rank map and every epic already
// named by graph.jira_epic_map for the project. The union matters: the board
// fetch is best-effort, and an empty rank map on a bad Jira day must not
// delete every epic→business edge.
func epicSet(ctx context.Context, db *pgxpool.Pool, project string, boardEpics map[string]int) ([]string, error) {
	set := map[string]bool{}
	for k := range boardEpics {
		if strings.HasPrefix(k, project+"-") {
			set[k] = true
		}
	}
	rows, err := db.Query(ctx,
		`SELECT DISTINCT epic_key FROM graph.jira_epic_map WHERE epic_key <> '' AND epic_key LIKE $1`,
		project+"-%")
	if err != nil {
		return nil, fmt.Errorf("load epic keys: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if rows.Scan(&k) == nil && k != "" {
			set[k] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out, rows.Err()
}

// rebuildEpicHierarchy is the round-1 write side run by refresh_jira_board
// after the jira_epic_map upsert: PART_OF edges (issue → epic → business root)
// and a full rebuild of graph.epic_membership. boardEpics is the board's epic
// rank map (keys only are used). Idempotent; safe to re-run.
func rebuildEpicHierarchy(ctx context.Context, db *pgxpool.Pool, machineID, project string, boardEpics map[string]int) error {
	epics, err := epicSet(ctx, db, project, boardEpics)
	if err != nil {
		return err
	}
	if err := ensureHierarchyNodes(ctx, db, machineID, project, epics); err != nil {
		return err
	}
	if err := upsertPartOfEdges(ctx, db, machineID, project, epics); err != nil {
		return err
	}
	return rebuildEpicMembership(ctx, db, project, epics)
}

// ensureHierarchyNodes makes the FK targets exist: the business root (the
// migration seeds it, but handler tests delete graph.nodes) and a stub node
// for every epic nobody has referenced yet (title from jira_epic_map so it is
// not a bare id; fetch_body enriches it later like any other jira node).
func ensureHierarchyNodes(ctx context.Context, db *pgxpool.Pool, machineID, project string, epics []string) error {
	if _, err := db.Exec(ctx, `
INSERT INTO graph.nodes (id, type, natural_key, title, scope, machine_id)
VALUES ($1, 'business', 'payments', 'Payments', 'jira', 'migration')
ON CONFLICT (id) DO NOTHING`, businessRootID); err != nil {
		return fmt.Errorf("ensure business root: %w", err)
	}
	if _, err := db.Exec(ctx, `
INSERT INTO graph.nodes (id, type, natural_key, title, scope, machine_id)
SELECT 'jira:' || k, 'jira', k,
       COALESCE((SELECT em.epic_summary FROM graph.jira_epic_map em WHERE em.epic_key = k AND em.epic_summary <> '' LIMIT 1), ''),
       'jira:' || $2, $3
FROM unnest($1::text[]) AS k
ON CONFLICT (id) DO NOTHING`, epics, project, machineID); err != nil {
		return fmt.Errorf("ensure epic nodes: %w", err)
	}
	return nil
}

// desiredPartOfCTE lists the PART_OF pairs the hierarchy wants right now:
// jira:<issue> → jira:<epic> for every mapped row (epics map to themselves in
// jira_epic_map; that self-pair is skipped), and jira:<epic> → business root
// for every epic in the set. $1 = project LIKE pattern, $2 = epic keys,
// $3 = business root id.
const desiredPartOfCTE = `
WITH desired AS (
  SELECT 'jira:' || em.issue_key AS f, 'jira:' || em.epic_key AS t
  FROM graph.jira_epic_map em
  WHERE em.epic_key <> '' AND em.issue_key <> em.epic_key AND em.epic_key LIKE $1
  UNION
  SELECT 'jira:' || k, $3::text FROM unnest($2::text[]) AS k
)`

func upsertPartOfEdges(ctx context.Context, db *pgxpool.Pool, machineID, project string, epics []string) error {
	like := project + "-%"
	if _, err := db.Exec(ctx, desiredPartOfCTE+`
INSERT INTO graph.edges (from_node_id, to_node_id, kind, metadata, machine_id)
SELECT d.f, d.t, 'PART_OF', jsonb_build_object('method', $4::text), $5
FROM desired d
JOIN graph.nodes a ON a.id = d.f
JOIN graph.nodes b ON b.id = d.t
ON CONFLICT (from_node_id, to_node_id, kind) DO UPDATE SET
  metadata = graph.edges.metadata || EXCLUDED.metadata`,
		like, epics, businessRootID, epicLinkMethod, machineID); err != nil {
		return fmt.Errorf("upsert PART_OF edges: %w", err)
	}
	if _, err := db.Exec(ctx, desiredPartOfCTE+`
DELETE FROM graph.edges e
WHERE e.kind = 'PART_OF' AND e.metadata->>'method' = $4
  AND NOT EXISTS (SELECT 1 FROM desired d WHERE d.f = e.from_node_id AND d.t = e.to_node_id)`,
		like, epics, businessRootID, epicLinkMethod); err != nil {
		return fmt.Errorf("delete stale PART_OF edges: %w", err)
	}
	return nil
}

// slackRootExpr is the SQL for a Slack message's thread-root node id. Every
// Slack node is a message; the root's thread_ts equals its own ts.
const slackRootExpr = `'slack:' || split_part(%[1]s.id, ':', 2) || ':' ||
  COALESCE(NULLIF(%[1]s.metadata->>'thread_ts', ''), split_part(%[1]s.id, ':', 3))`

// rebuildEpicMembership recomputes graph.epic_membership from scratch inside
// one transaction (readers never see a half-built table). Rows are inserted in
// tiers with ON CONFLICT DO NOTHING, so the first writer wins per
// (node_id, epic_key):
//
//  1. epic_self  – the epic node itself, and every issue mapped to it.
//  2. key        – the REFERENCES source (thread root / PR / doc / issue) of an
//     epic_self member. Slack referrers are lifted to their thread root.
//  3. topic_link – SAME_TOPIC neighbour (confidence ≥ 0.8) of a tier-1/2
//     member. Reads the pre-statement snapshot, so it is exactly one hop and
//     never chains through another topic_link row.
//  4. replies inherit their thread root's rows (same via/confidence).
//  5. business root – every epic member keeps its strongest row under the
//     root's key; every Slack message with a current 'eligible' gate decision
//     joins with via='eligible' and the gate score. The root node itself is
//     an epic_self member.
//
// Windows: every member row carries the node's own event time in
// first_at/last_at; the epic's own row (node_id = the epic node) carries the
// epic-wide min/max, so `epic_membership.first_at/last_at` on the epic row is
// the activity window the read side and round 2's temporal arm consume.
func rebuildEpicMembership(ctx context.Context, db *pgxpool.Pool, project string, epics []string) error {
	like := project + "-%"
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin membership rebuild: %w", err)
	}
	defer tx.Rollback(ctx)

	steps := []struct {
		name string
		sql  string
		args []any
	}{
		{"clear", `DELETE FROM graph.epic_membership`, nil},
		{"epic_self", `
INSERT INTO graph.epic_membership (node_id, epic_key, via, confidence)
SELECT n.id, em.epic_key, $3, 1.0
FROM graph.jira_epic_map em
JOIN graph.nodes n ON n.id = 'jira:' || em.issue_key AND n.deleted_at IS NULL
WHERE em.epic_key <> '' AND em.epic_key LIKE $1
UNION
SELECT n.id, k, $3, 1.0
FROM unnest($2::text[]) AS k
JOIN graph.nodes n ON n.id = 'jira:' || k AND n.deleted_at IS NULL
ON CONFLICT DO NOTHING`, []any{like, epics, viaEpicSelf}},
		{"key", fmt.Sprintf(`
INSERT INTO graph.epic_membership (node_id, epic_key, via, confidence)
SELECT DISTINCT COALESCE(r.id, n.id), m.epic_key, $2, 1.0
FROM graph.epic_membership m
JOIN graph.edges e ON e.kind = 'REFERENCES' AND e.to_node_id = m.node_id
JOIN graph.nodes n ON n.id = e.from_node_id AND n.deleted_at IS NULL AND n.type <> 'business'
LEFT JOIN graph.nodes r
  ON n.type IN ('slack', 'slack_thread')
 AND r.id = %s
 AND r.deleted_at IS NULL
WHERE m.via = $1
ON CONFLICT DO NOTHING`, fmt.Sprintf(slackRootExpr, "n")), []any{viaEpicSelf, viaKey}},
		{"topic_link", fmt.Sprintf(`
INSERT INTO graph.epic_membership (node_id, epic_key, via, confidence)
SELECT DISTINCT ON (x.nid, x.epic_key) x.nid, x.epic_key, $3, x.conf
FROM (
  SELECT COALESCE(r.id, n.id) AS nid, m.epic_key,
         COALESCE(NULLIF(e.metadata->>'confidence', '')::float8, 0) AS conf
  FROM graph.epic_membership m
  JOIN graph.edges e ON e.kind = 'SAME_TOPIC' AND m.node_id IN (e.from_node_id, e.to_node_id)
  JOIN graph.nodes n
    ON n.id = CASE WHEN e.from_node_id = m.node_id THEN e.to_node_id ELSE e.from_node_id END
   AND n.deleted_at IS NULL
  LEFT JOIN graph.nodes r
    ON n.type IN ('slack', 'slack_thread')
   AND r.id = %s
   AND r.deleted_at IS NULL
  WHERE m.via IN ($1, $2)
) x
WHERE x.conf >= $4
ORDER BY x.nid, x.epic_key, x.conf DESC
ON CONFLICT DO NOTHING`, fmt.Sprintf(slackRootExpr, "n")), []any{viaEpicSelf, viaKey, viaTopicLink, topicLinkMinConfidence}},
		{"replies_inherit", `
INSERT INTO graph.epic_membership (node_id, epic_key, via, confidence)
SELECT n.id, m.epic_key, m.via, m.confidence
FROM graph.nodes n
JOIN graph.epic_membership m
  ON m.node_id = 'slack:' || split_part(n.id, ':', 2) || ':' || NULLIF(n.metadata->>'thread_ts', '')
WHERE n.type IN ('slack', 'slack_thread') AND n.deleted_at IS NULL AND n.id <> m.node_id
ON CONFLICT DO NOTHING`, nil},
		{"business_root_self", `
INSERT INTO graph.epic_membership (node_id, epic_key, via, confidence)
SELECT $1, $1, $2, 1.0 FROM graph.nodes WHERE id = $1
ON CONFLICT DO NOTHING`, []any{businessRootID, viaEpicSelf}},
		{"business_epic_members", `
INSERT INTO graph.epic_membership (node_id, epic_key, via, confidence)
SELECT DISTINCT ON (m.node_id) m.node_id, $1, m.via, m.confidence
FROM graph.epic_membership m
WHERE m.epic_key <> $1
ORDER BY m.node_id, m.confidence DESC, m.via
ON CONFLICT DO NOTHING`, []any{businessRootID}},
		// Latest decision per message wins (same rule as the gate's own reads).
		// An inherited_root row has no score of its own; it carries the root's
		// scored decision, so take that score.
		{"business_eligible", `
INSERT INTO graph.epic_membership (node_id, epic_key, via, confidence)
SELECT n.id, $1, $2,
       COALESCE(d.score,
                (SELECT rd.score FROM graph.eligibility_decisions rd
                 WHERE rd.channel_id = d.channel_id
                   AND rd.message_ts = NULLIF(n.metadata->>'thread_ts', '')
                   AND rd.score IS NOT NULL
                 ORDER BY rd.decided_at DESC, rd.id DESC LIMIT 1),
                1.0)
FROM (
  SELECT DISTINCT ON (channel_id, message_ts) channel_id, message_ts, decision, score
  FROM graph.eligibility_decisions
  ORDER BY channel_id, message_ts, decided_at DESC, id DESC
) d
JOIN graph.nodes n ON n.id = 'slack:' || d.channel_id || ':' || d.message_ts AND n.deleted_at IS NULL
WHERE d.decision = 'eligible'
ON CONFLICT DO NOTHING`, []any{businessRootID, viaEligible}},
		{"row_times", `
UPDATE graph.epic_membership m
SET first_at = COALESCE(n.created_at, n.first_seen_at),
    last_at  = COALESCE(n.created_at, n.first_seen_at)
FROM graph.nodes n WHERE n.id = m.node_id`, nil},
		// The epic's own row (or the root's) takes MIN/MAX of the dated members'
		// real created_at, excluding the self row and the root. Placeholder
		// first_seen_at never leaks in; no qualifying member → NULL window.
		{"epic_windows", `
UPDATE graph.epic_membership m
SET first_at = w.first_at, last_at = w.last_at
FROM (
  SELECT s.epic_key, s.node_id,
         MIN(n.created_at) AS first_at,
         MAX(n.created_at) AS last_at
  FROM graph.epic_membership s
  LEFT JOIN graph.epic_membership mm
    ON mm.epic_key = s.epic_key AND mm.node_id <> s.node_id AND mm.node_id <> $1
  LEFT JOIN graph.nodes n ON n.id = mm.node_id
  WHERE s.node_id = CASE WHEN s.epic_key = $1 THEN $1 ELSE 'jira:' || s.epic_key END
  GROUP BY s.epic_key, s.node_id
) w
WHERE m.epic_key = w.epic_key AND m.node_id = w.node_id`, []any{businessRootID}},
	}
	for _, st := range steps {
		if _, err := tx.Exec(ctx, st.sql, st.args...); err != nil {
			return fmt.Errorf("membership %s: %w", st.name, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit membership rebuild: %w", err)
	}
	return nil
}

// businessRootProjectRe: a Jira project key (uppercase letters/digits, letter
// first), so a typo cannot turn the LIKE filter into a wildcard.
var businessRootProjectRe = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}$`)

type businessRootConfig struct {
	Project string `json:"project"`
}

// getBusinessRoot serves GET /api/graph/business-root: the Jira project key
// behind the Payments business root (Settings page).
func (h *Channels) getBusinessRoot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(businessRootConfig{Project: businessRootProject(r.Context(), h.db)})
}

// putBusinessRoot serves PUT /api/graph/business-root. Takes effect on the
// next refresh_jira_board run (6h); no cache to invalidate.
func (h *Channels) putBusinessRoot(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var cfg businessRootConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	cfg.Project = strings.ToUpper(strings.TrimSpace(cfg.Project))
	if !businessRootProjectRe.MatchString(cfg.Project) {
		writeError(w, http.StatusBadRequest, "project must be a Jira project key like PAY")
		return
	}
	if _, err := h.db.Exec(r.Context(), `
		INSERT INTO settings(key,value) VALUES($1,$2)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, businessRootProjectKey, cfg.Project); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(cfg)
}
