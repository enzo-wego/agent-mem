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
		row := db.QueryRow(ctx, `
SELECT n.title, n.type, COALESCE(n.url,''), b.body_full
FROM graph.nodes n
LEFT JOIN graph.artifact_bodies b ON b.node_id = n.id
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
			continue
		}
		tokens := len(*body)/4 + 1
		if used+tokens > budgetTokens {
			break
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
