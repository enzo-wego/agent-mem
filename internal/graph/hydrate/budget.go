// Package hydrate loads artifact bodies into the response up to a token budget.
package hydrate

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Candidate is a node + score being considered for hydration.
type Candidate struct {
	NodeID string
	Score  float64
}

// Hydrated is a Candidate with its body and metadata loaded.
type Hydrated struct {
	NodeID string
	Title  string
	Type   string
	URL    string
	Score  float64
	Body   string
	Tokens int
}

// Greedy hydrates candidates in score order, stopping when the next
// candidate would push past budgetTokens. Returns hydrated entries plus
// any node_ids whose body wasn't in cache (caller enqueues fetch_body).
//
// Token approximation: 1 token ≈ 4 chars (rough rule for English).
func Greedy(ctx context.Context, db *pgxpool.Pool, cands []Candidate, budgetTokens int) ([]Hydrated, []string, error) {
	var out []Hydrated
	var missed []string
	used := 0
	for _, c := range cands {
		// Slack thread titles live in graph.thread_summaries, not n.title (often
		// empty); fall back to the summary so the opened-node header/resolve shows
		// readable text, never a raw slack:CHANNEL:TS id. Same join the neighbor
		// rows use. Read-time only — thread_summaries stays the source of truth.
		row := db.QueryRow(ctx, `
SELECT COALESCE(
         NULLIF(n.title,''),
         CASE WHEN n.type IN ('slack','slack_thread') THEN NULLIF(ts.summary,'') END,
         ''),
       n.type, COALESCE(n.url,''), b.body_full
FROM graph.nodes n
LEFT JOIN graph.artifact_bodies b ON b.node_id = n.id
LEFT JOIN graph.thread_summaries ts
  ON ts.channel_id = REPLACE(n.scope,'slack:','')
  AND ts.thread_ts = COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3))
WHERE n.id = $1 AND n.deleted_at IS NULL
`, c.NodeID)
		var title, typ, url string
		var body *string
		err := row.Scan(&title, &typ, &url, &body)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if body == nil {
			missed = append(missed, c.NodeID)
			// ponytail: slack_file and jira_attachment have no body by nature —
			// they are a title + URL, not content awaiting fetch. Emit a
			// zero-token artifact so they surface in resolve (a title + URL costs
			// nothing worth budgeting), while STILL appending to missed above so
			// the fetch_body enqueue path stays byte-for-byte unchanged. Every
			// other bodyless type is genuinely waiting on a body, so it keeps
			// today's behaviour (missed, no artifact) — nothing leaks empty
			// artifacts.
			if typ == "slack_file" || typ == "jira_attachment" {
				out = append(out, Hydrated{
					NodeID: c.NodeID,
					Title:  title,
					Type:   typ,
					URL:    url,
					Score:  c.Score,
					Body:   "",
					Tokens: 0,
				})
			}
			continue
		}
		tokens := len(*body)/4 + 1
		if used+tokens > budgetTokens {
			// Skip, don't stop: candidates are score-ordered, so one oversized
			// body near the front used to break the loop and return NOTHING.
			// A 16KB Slack thread root (4032 tokens vs a 4000 budget) emptied
			// the whole /api/graph/resolve response.
			continue
		}
		out = append(out, Hydrated{
			NodeID: c.NodeID,
			Title:  title,
			Type:   typ,
			URL:    url,
			Score:  c.Score,
			Body:   *body,
			Tokens: tokens,
		})
		used += tokens
	}
	return out, missed, nil
}
