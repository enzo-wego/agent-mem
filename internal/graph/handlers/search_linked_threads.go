package handlers

import (
	"context"
	"sort"
	"strings"

	"github.com/agent-mem/agent-mem/internal/graph/scoring"
	"github.com/agent-mem/agent-mem/internal/graph/temporal"
)

const (
	linkedPerSource = 3
	linkedTotal     = 10
)

type linkedCandidate struct {
	row  searchResult
	last int64
}

// addLinkedThreads inserts, right after each kept Jira (`jira`) or Confluence
// (`cf`) result, the Slack threads that REFERENCES it: at most linkedPerSource
// per source (newest activity first) and linkedTotal overall. Each thread is
// owned by the highest-ranked source linking it. Threads already ranked, or
// rejected by the request filter / hard window, are dropped before the caps.
func (s *Search) addLinkedThreads(ctx context.Context, results []searchResult, f searchFilter,
	hardWindow bool, win temporal.Window) ([]searchResult, error) {
	if !slackTypesAllowed(f.types) {
		return results, nil
	}
	var srcIDs []string
	srcByID := map[string]searchResult{}
	for _, r := range results {
		if r.Type == "jira" || r.Type == "cf" {
			srcIDs = append(srcIDs, r.NodeID)
			srcByID[r.NodeID] = r
		}
	}
	if len(srcIDs) == 0 {
		return results, nil
	}

	// Referencing Slack messages folded to their thread key.
	rows, err := s.db.Query(ctx, `
SELECT e.to_node_id, `+threadKeySQL+`
FROM graph.edges e
JOIN graph.nodes n ON n.id = e.from_node_id
WHERE e.kind = 'REFERENCES' AND e.to_node_id = ANY($1)
  AND n.type IN ('slack','slack_thread') AND n.deleted_at IS NULL`, srcIDs)
	if err != nil {
		return nil, err
	}
	linked := map[string]map[string]bool{} // source -> thread keys
	var keys []string
	seenKey := map[string]bool{}
	for rows.Next() {
		var src, key string
		if err := rows.Scan(&src, &key); err != nil {
			rows.Close()
			return nil, err
		}
		if linked[src] == nil {
			linked[src] = map[string]bool{}
		}
		linked[src][key] = true
		if !seenKey[key] {
			seenKey[key] = true
			keys = append(keys, key)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return results, nil
	}

	// Thread roots that exist and pass the request filter.
	rrows, err := s.db.Query(ctx, `
SELECT n.id, n.type, COALESCE(n.title,''), COALESCE(n.url,''), COALESCE(ai.summary,''),
       COALESCE(p.display_name,''), COALESCE(n.created_at, n.first_seen_at)
FROM graph.nodes n
LEFT JOIN graph.artifact_index ai ON ai.node_id = n.id
LEFT JOIN graph.people p ON p.id = n.author_person_id
WHERE n.id = ANY($1) AND n.type IN ('slack','slack_thread')
  AND `+f.sql(2, 3, 4), append([]any{keys}, f.args()...)...)
	if err != nil {
		return nil, err
	}
	roots := map[string]searchResult{}
	for rrows.Next() {
		var r searchResult
		if err := rrows.Scan(&r.NodeID, &r.Type, &r.Title, &r.URL, &r.Summary, &r.Author, &r.CreatedAt); err != nil {
			rrows.Close()
			return nil, err
		}
		r.ID = r.NodeID
		roots[r.NodeID] = r
	}
	rrows.Close()
	if err := rrows.Err(); err != nil {
		return nil, err
	}

	if hardWindow && len(roots) > 0 {
		ids := make([]string, 0, len(roots))
		for id := range roots {
			ids = append(ids, id)
		}
		eligible, err := nodesEligibleInWindow(ctx, s.db, ids, win)
		if err != nil {
			return nil, err
		}
		for id := range roots {
			if !eligible[id] {
				delete(roots, id)
			}
		}
	}

	// Drop threads already among the ranked results.
	if len(roots) > 0 {
		rankedIDs := make([]string, len(results))
		for i, r := range results {
			rankedIDs[i] = r.NodeID
		}
		drows, err := s.db.Query(ctx, `
SELECT `+slackThreadRootSQL+` FROM graph.nodes n WHERE n.id = ANY($1)`, rankedIDs)
		if err != nil {
			return nil, err
		}
		for drows.Next() {
			var root *string
			if err := drows.Scan(&root); err != nil {
				drows.Close()
				return nil, err
			}
			if root != nil {
				delete(roots, *root)
			}
		}
		drows.Close()
		if err := drows.Err(); err != nil {
			return nil, err
		}
		for _, r := range results {
			delete(roots, r.NodeID)
		}
	}
	if len(roots) == 0 {
		return results, nil
	}

	// Ownership: the highest-ranked source linking a thread.
	owned := map[string][]string{}
	taken := map[string]bool{}
	for _, src := range srcIDs {
		for key := range linked[src] {
			if _, ok := roots[key]; ok && !taken[key] {
				taken[key] = true
				owned[src] = append(owned[src], key)
			}
		}
	}
	var ownedRoots []string
	for key := range taken {
		ownedRoots = append(ownedRoots, key)
	}
	cards, err := threadCards(ctx, s.db, ownedRoots)
	if err != nil {
		return nil, err
	}

	out := make([]searchResult, 0, len(results)+linkedTotal)
	added := 0
	expanded := map[string]bool{}
	for _, r := range results {
		out = append(out, r)
		if (r.Type != "jira" && r.Type != "cf") || expanded[r.NodeID] {
			continue
		}
		expanded[r.NodeID] = true
		var cands []linkedCandidate
		for _, key := range owned[r.NodeID] {
			cands = append(cands, linkedCandidate{row: roots[key], last: cards[key].LastTSMs})
		}
		sort.Slice(cands, func(i, j int) bool {
			if cands[i].last != cands[j].last {
				return cands[i].last > cands[j].last
			}
			return cands[i].row.NodeID < cands[j].row.NodeID
		})
		title := strings.TrimPrefix(r.NodeID, "jira:")
		if r.Type == "cf" {
			title = r.Title
		}
		for i, c := range cands {
			if i >= linkedPerSource || added >= linkedTotal {
				break
			}
			row := c.row
			row.Score = r.Score
			row.ScoreBreakdown = scoring.Components{}
			row.LinkedVia, row.LinkedViaTitle = r.NodeID, title
			out = append(out, row)
			added++
		}
	}
	return out, nil
}

// slackTypesAllowed reports whether a types filter (nil = no filter) admits
// Slack roots.
func slackTypesAllowed(types any) bool {
	t, ok := types.([]string)
	if !ok || t == nil {
		return true
	}
	for _, x := range t {
		if x == "slack" || x == "slack_thread" {
			return true
		}
	}
	return false
}
