package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"github.com/agent-mem/agent-mem/internal/graph/acl"
	"github.com/agent-mem/agent-mem/internal/graph/scoring"
)

// Embedder is the minimal interface Search needs to embed a query string.
// *gemini.Client satisfies it via its Embed method.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// Search handles GET /api/graph/search.
type Search struct {
	db      *pgxpool.Pool
	embed   Embedder
	aclBld  *acl.Builder
	weights scoring.Weights
}

// NewSearch creates a Search handler. embed may be nil (semantic scoring
// will be skipped and only recency/team/authority used).
func NewSearch(db *pgxpool.Pool) (*Search, error) {
	w, err := scoring.LoadWeights(context.Background(), db)
	if err != nil {
		return nil, err
	}
	return &Search{
		db:     db,
		embed:  nil, // wired at server level via NewSearchWithEmbedder
		aclBld: acl.NewBuilder(db, 5*time.Minute),
		weights: w,
	}, nil
}

// NewSearchWithEmbedder is used when a real Embedder is available.
func NewSearchWithEmbedder(db *pgxpool.Pool, embed Embedder) (*Search, error) {
	s, err := NewSearch(db)
	if err != nil {
		return nil, err
	}
	s.embed = embed
	return s, nil
}

type searchResult struct {
	NodeID         string             `json:"node_id"`
	Type           string             `json:"type"`
	Title          string             `json:"title"`
	URL            string             `json:"url"`
	Summary        string             `json:"summary"`
	Score          float64            `json:"score"`
	ScoreBreakdown scoring.Components `json:"score_breakdown"`
	Author         map[string]string  `json:"author,omitempty"`
}

func (s *Search) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query().Get("q")
	if q == "" {
		http.Error(w, "q required", http.StatusBadRequest)
		return
	}
	typesFilter := splitCSV(r.URL.Query().Get("types"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 50 {
		limit = 10
	}

	askerEEID := lookupAskerEEID(ctx, s.db, r.Header.Get("X-Asker-User"))
	scopes, _ := s.aclBld.For(ctx, askerEEID)

	// Embed the query if an embedder is available.
	var queryVec []float32
	if s.embed != nil {
		v, err := s.embed.Embed(ctx, q)
		if err != nil {
			http.Error(w, "embed failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		queryVec = v
	}

	var rows interface{ Next() bool; Scan(...any) error; Close(); Err() error }
	var err error

	if queryVec != nil {
		// Semantic search: order by vector cosine distance.
		const query = `
SELECT n.id, n.type, COALESCE(n.title,''), COALESCE(n.url,''),
       COALESCE(ai.summary,''),
       COALESCE(p.display_name,''),
       1.0 - (ai.embedding <=> $1) AS cosine,
       n.updated_at,
       COALESCE(p.depth_from_root, 0),
       COALESCE(p.is_bot, false),
       COALESCE(p.eeid, 0)
FROM graph.artifact_index ai
JOIN graph.nodes n ON n.id = ai.node_id
LEFT JOIN graph.people p ON p.id = n.author_person_id
WHERE n.deleted_at IS NULL
  AND ($2::text[] IS NULL OR n.type = ANY($2))
  AND ($3::text[] IS NULL OR n.scope = ANY($3))
ORDER BY ai.embedding <=> $1
LIMIT $4
`
		var scopeArg any
		if len(scopes) > 0 {
			scopeArg = scopes
		}
		var typesArg any
		if len(typesFilter) > 0 {
			typesArg = typesFilter
		}
		rows, err = s.db.Query(ctx, query,
			pgvector.NewVector(queryVec),
			typesArg, scopeArg, limit*3)
	} else {
		// Keyword/title search fallback when no embedder.
		const query = `
SELECT n.id, n.type, COALESCE(n.title,''), COALESCE(n.url,''),
       COALESCE(ai.summary,''),
       COALESCE(p.display_name,''),
       0.5 AS cosine,
       n.updated_at,
       COALESCE(p.depth_from_root, 0),
       COALESCE(p.is_bot, false),
       COALESCE(p.eeid, 0)
FROM graph.nodes n
LEFT JOIN graph.artifact_index ai ON ai.node_id = n.id
LEFT JOIN graph.people p ON p.id = n.author_person_id
WHERE n.deleted_at IS NULL
  AND ($1::text[] IS NULL OR n.type = ANY($1))
  AND ($2::text[] IS NULL OR n.scope = ANY($2))
  AND (n.title ILIKE '%' || $3 || '%' OR n.body ILIKE '%' || $3 || '%')
ORDER BY n.updated_at DESC
LIMIT $4
`
		var scopeArg any
		if len(scopes) > 0 {
			scopeArg = scopes
		}
		var typesArg any
		if len(typesFilter) > 0 {
			typesArg = typesFilter
		}
		rows, err = s.db.Query(ctx, query, typesArg, scopeArg, q, limit*3)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	now := time.Now()
	var results []searchResult
	for rows.Next() {
		var (
			id, typ, title, url, summary, authorName string
			cosine                                    float64
			updatedAt                                 time.Time
			depth                                     int16
			isBot                                     bool
			authorEEID                                int
		)
		if err := rows.Scan(&id, &typ, &title, &url, &summary, &authorName,
			&cosine, &updatedAt, &depth, &isBot, &authorEEID); err != nil {
			continue
		}
		c := scoring.Components{
			Sem:  scoring.Semantic(cosine),
			Rec:  scoring.Recency(updatedAt, now, 30*24*time.Hour),
			Edge: 0, // /search has no graph context — leave 0
			Team: personScoreForSearch(ctx, s.db, askerEEID, authorEEID),
			Auth: scoring.Authority(depth, 6),
		}
		score := scoring.Combine(s.weights, c)
		results = append(results, searchResult{
			NodeID: id, Type: typ, Title: title, URL: url,
			Summary: summary, Score: score, ScoreBreakdown: c,
			Author: map[string]string{"name": authorName},
		})
	}
	if rows.Err() != nil {
		http.Error(w, rows.Err().Error(), http.StatusInternalServerError)
		return
	}

	// Re-rank by combined score, return top `limit`.
	sortByScore(results)
	if len(results) > limit {
		results = results[:limit]
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"results": results,
		"total":   len(results),
	})
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// personScoreForSearch is the simplified person scoring used in /search
// where there's no asker thread to anchor against.
func personScoreForSearch(ctx context.Context, db *pgxpool.Pool, asker, author int) float64 {
	if asker == 0 || author == 0 {
		return 0.1
	}
	if asker == author {
		return 1.0
	}
	if shareTeamGroup(ctx, db, asker, author) {
		return 0.9
	}
	if shareDeptGroup(ctx, db, asker, author) {
		return 0.7
	}
	d, _ := scoring.LookupDistance(ctx, db, asker, author)
	switch {
	case d <= 2:
		return 0.4
	case d <= 4:
		return 0.25
	default:
		return 0.1
	}
}

// shareTeamGroup returns true if both eeids are in at least one common
// team-level group (team_group_ids from user_affinity_config).
func shareTeamGroup(ctx context.Context, db *pgxpool.Pool, asker, author int) bool {
	row := db.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM graph.user_affinity_config a
  JOIN graph.user_affinity_config b ON b.eeid = $2
  WHERE a.eeid = $1
    AND a.team_group_ids && b.team_group_ids
)`, asker, author)
	var ok bool
	row.Scan(&ok)
	return ok
}

// shareDeptGroup returns true if both eeids are in at least one common
// dept-level group (dept_group_ids from user_affinity_config).
func shareDeptGroup(ctx context.Context, db *pgxpool.Pool, asker, author int) bool {
	row := db.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM graph.user_affinity_config a
  JOIN graph.user_affinity_config b ON b.eeid = $2
  WHERE a.eeid = $1
    AND a.dept_group_ids && b.dept_group_ids
)`, asker, author)
	var ok bool
	row.Scan(&ok)
	return ok
}

func sortByScore(rs []searchResult) {
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0 && rs[j].Score > rs[j-1].Score; j-- {
			rs[j], rs[j-1] = rs[j-1], rs[j]
		}
	}
}

// lookupAskerEEID resolves the X-Asker-User header (slack uid or email)
// to an eeid by joining graph.people. Returns 0 if not found.
func lookupAskerEEID(ctx context.Context, db *pgxpool.Pool, ref string) int {
	if ref == "" {
		return 0
	}
	row := db.QueryRow(ctx, `
SELECT COALESCE(eeid, 0) FROM graph.people
WHERE slack_user_id = $1 OR email = $1 OR github_login = $1 OR jira_account_id = $1
LIMIT 1`, ref)
	var eeid int
	row.Scan(&eeid)
	return eeid
}
