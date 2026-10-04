package handlers

import (
	"context"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

// prRef is one PR linked to a Jira ticket.
type prRef struct {
	NodeID    string `json:"node_id"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	Author    string `json:"author"`
	CreatedMs int64  `json:"created_ms"`
}

// jiraPRList is a ticket's visible linked PRs: the full count and the first 20.
type jiraPRList struct {
	Count int     `json:"pr_count"`
	PRs   []prRef `json:"prs"`
}

// jiraPRs returns, per Jira id, the gh_pr nodes joined to it by a direct
// REFERENCES edge in either direction. PRs whose scope is not visible are
// dropped before counting and capping. Jira ids with no PRs are absent.
func jiraPRs(ctx context.Context, db *pgxpool.Pool, jiraIDs []string, visible func(scope *string) bool) (map[string]jiraPRList, error) {
	out := map[string]jiraPRList{}
	if len(jiraIDs) == 0 {
		return out, nil
	}
	rows, err := db.Query(ctx, `
SELECT l.jira_id, n.id, COALESCE(n.title,''), COALESCE(n.url,''), n.scope,
       COALESCE(NULLIF(p.display_name,''), ''),
       (EXTRACT(EPOCH FROM COALESCE(n.created_at, n.first_seen_at))*1000)::bigint
FROM (
  SELECT from_node_id AS jira_id, to_node_id AS pr_id FROM graph.edges
    WHERE kind = 'REFERENCES' AND from_node_id = ANY($1)
  UNION
  SELECT to_node_id AS jira_id, from_node_id AS pr_id FROM graph.edges
    WHERE kind = 'REFERENCES' AND to_node_id = ANY($1)
) l
JOIN graph.nodes n ON n.id = l.pr_id AND n.type = 'gh_pr' AND n.deleted_at IS NULL
LEFT JOIN graph.people p ON p.id = n.author_person_id`, jiraIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all := map[string][]prRef{}
	for rows.Next() {
		var jid string
		var pr prRef
		var scope *string
		var ms *int64
		if err := rows.Scan(&jid, &pr.NodeID, &pr.Title, &pr.URL, &scope, &pr.Author, &ms); err != nil {
			return nil, err
		}
		if !visible(scope) {
			continue
		}
		if looksLikeSlackID(pr.Author) {
			pr.Author = ""
		}
		if ms != nil {
			pr.CreatedMs = *ms
		}
		all[jid] = append(all[jid], pr)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for jid, prs := range all {
		sort.Slice(prs, func(i, j int) bool {
			if prs[i].CreatedMs != prs[j].CreatedMs {
				return prs[i].CreatedMs > prs[j].CreatedMs
			}
			return prs[i].NodeID < prs[j].NodeID
		})
		n := len(prs)
		// ponytail: cap the listed PRs at 20; tickets with up to 35 PRs exist in prod.
		if len(prs) > 20 {
			prs = prs[:20]
		}
		out[jid] = jiraPRList{Count: n, PRs: prs}
	}
	return out, nil
}
