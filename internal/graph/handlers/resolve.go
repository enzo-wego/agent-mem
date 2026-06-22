package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/acl"
	"github.com/agent-mem/agent-mem/internal/graph/bfs"
	"github.com/agent-mem/agent-mem/internal/graph/hydrate"
	"github.com/agent-mem/agent-mem/internal/graph/scoring"
)

type resolveRequest struct {
	Seeds         []string `json:"seeds"`
	Query         string   `json:"query"`
	AskerEEID     int      `json:"asker_eeid"`
	Depth         int      `json:"depth"`
	BudgetTokens  int      `json:"budget_tokens"`
	IncludeBodies bool     `json:"include_bodies"`
}

type resolveResponse struct {
	ContextTokens int               `json:"context_tokens"`
	Artifacts     []resolveArtifact `json:"artifacts"`
	GraphTrace    resolveTrace      `json:"graph_trace"`
	CacheMisses   []string          `json:"cache_misses,omitempty"`
}

type resolveArtifact struct {
	NodeID         string             `json:"node_id"`
	URL            string             `json:"url"`
	Type           string             `json:"type"`
	Title          string             `json:"title"`
	Author         string             `json:"author,omitempty"`
	Score          float64            `json:"score"`
	ScoreBreakdown scoring.Components `json:"score_breakdown"`
	Summary        string             `json:"summary,omitempty"`
	Body           string             `json:"body,omitempty"`
	Hop            int                `json:"hop"`
	Via            []string           `json:"via,omitempty"`
}

type resolveTrace struct {
	Seeds               []string `json:"seeds"`
	ExpandedNodes       int      `json:"expanded_nodes"`
	AfterACL            int      `json:"after_acl"`
	AfterScoreThreshold int      `json:"after_score_threshold"`
	TookMs              int64    `json:"took_ms"`
}

// Resolve handles POST /api/graph/resolve.
type Resolve struct {
	db      *pgxpool.Pool
	aclBld  *acl.Builder
	exp     *bfs.Expander
	weights scoring.Weights
}

// NewResolve creates a Resolve handler.
func NewResolve(db *pgxpool.Pool) (*Resolve, error) {
	w, err := scoring.LoadWeights(context.Background(), db)
	if err != nil {
		return nil, err
	}
	return &Resolve{
		db:     db,
		aclBld: acl.NewBuilder(db, 5*time.Minute),
		exp:    bfs.NewExpander(db),
		weights: w,
	}, nil
}

func (h *Resolve) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	ctx := r.Context()
	var req resolveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(req.Seeds) == 0 {
		http.Error(w, "seeds required", http.StatusBadRequest)
		return
	}
	if req.Depth <= 0 {
		req.Depth = 2
	}
	if req.BudgetTokens <= 0 {
		req.BudgetTokens = 4000
	}

	scopes, _ := h.aclBld.For(ctx, req.AskerEEID)
	scopeSet := make(map[string]bool, len(scopes))
	for _, s := range scopes {
		scopeSet[s] = true
	}

	// BFS expansion from seeds.
	frontier := bfs.NewFrontier(200)
	for _, s := range req.Seeds {
		frontier.Push(bfs.Candidate{NodeID: s, Hop: 0, Score: 1.0})
	}
	visited := make(map[string]bfs.Candidate)
	for frontier.Len() > 0 {
		c := frontier.Pop()
		if _, seen := visited[c.NodeID]; seen {
			continue
		}
		visited[c.NodeID] = c
		if c.Hop >= req.Depth {
			continue
		}
		nbrs, err := h.exp.Expand(ctx, c.NodeID, nil)
		if err != nil {
			continue
		}
		for _, n := range nbrs {
			frontier.Push(bfs.Candidate{
				NodeID:  n.NodeID,
				Hop:     c.Hop + 1,
				Score:   c.Score * 0.5, // edge attenuation
				ViaEdge: n.EdgeKind,
			})
		}
	}

	// ACL filter. noFilter is keyed on whether a *principal was asserted at all*,
	// NOT on whether the membership set is empty: a real asker (eeid != 0) with
	// zero memberships must still be filtered (sees only unscoped + "public"),
	// never the whole graph. eeid == 0 means no asker was asserted — the trusted
	// dashboard/integration calling behind the API key — which gets the unfiltered
	// admin view (same contract as /search). See checkScope for "public".
	// (The API key is the privilege boundary here; asker_eeid is advisory until
	// the asker identity is authenticated — see Mount's auth note.)
	var filtered []bfs.Candidate
	noFilter := req.AskerEEID == 0
	for _, c := range visited {
		if noFilter {
			filtered = append(filtered, c)
			continue
		}
		ok, _ := h.checkScope(ctx, c.NodeID, scopeSet)
		if ok {
			filtered = append(filtered, c)
		}
	}

	// Score each surviving candidate.
	scored := h.scoreAll(ctx, req, filtered)

	// Sort descending by score.
	for i := 1; i < len(scored); i++ {
		for j := i; j > 0 && scored[j].Score > scored[j-1].Score; j-- {
			scored[j], scored[j-1] = scored[j-1], scored[j]
		}
	}

	// Hydrate top-K to fit budget.
	cands := make([]hydrate.Candidate, 0, len(scored))
	for _, s := range scored {
		cands = append(cands, hydrate.Candidate{NodeID: s.NodeID, Score: s.Score})
	}
	hydrated, missed, _ := hydrate.Greedy(ctx, h.db, cands, req.BudgetTokens)

	// Build response.
	resp := resolveResponse{
		Artifacts: make([]resolveArtifact, 0, len(hydrated)),
		GraphTrace: resolveTrace{
			Seeds:               req.Seeds,
			ExpandedNodes:       len(visited),
			AfterACL:            len(filtered),
			AfterScoreThreshold: len(scored),
			TookMs:              time.Since(t0).Milliseconds(),
		},
		CacheMisses: missed,
	}
	for _, hyd := range hydrated {
		// Find matching scored entry for breakdown.
		var bd scoring.Components
		var score float64
		var hop int
		for _, s := range scored {
			if s.NodeID == hyd.NodeID {
				bd = s.Breakdown
				score = s.Score
				hop = s.Hop
				break
			}
		}
		body := hyd.Body
		if !req.IncludeBodies {
			body = ""
		}
		resp.Artifacts = append(resp.Artifacts, resolveArtifact{
			NodeID: hyd.NodeID, URL: hyd.URL, Type: hyd.Type, Title: hyd.Title,
			Score: score, ScoreBreakdown: bd,
			Body: body, Hop: hop,
		})
		resp.ContextTokens += hyd.Tokens
	}
	// Enqueue fetch_body jobs for misses (so subsequent calls are fast).
	for _, m := range missed {
		h.enqueueFetchBody(ctx, m)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

type scoredCand struct {
	NodeID    string
	Score     float64
	Breakdown scoring.Components
	Hop       int
}

func (h *Resolve) scoreAll(ctx context.Context, req resolveRequest, cands []bfs.Candidate) []scoredCand {
	now := time.Now()
	out := make([]scoredCand, 0, len(cands))
	for _, c := range cands {
		row := h.db.QueryRow(ctx, `
SELECT n.updated_at,
       COALESCE(p.depth_from_root, 0),
       COALESCE(p.eeid, 0),
       COALESCE(p.is_bot, false)
FROM graph.nodes n
LEFT JOIN graph.people p ON p.id = n.author_person_id
WHERE n.id = $1
`, c.NodeID)
		var ts time.Time
		var depth int16
		var authorEEID int
		var isBot bool
		if err := row.Scan(&ts, &depth, &authorEEID, &isBot); err != nil {
			continue
		}
		bd := scoring.Components{
			Sem:  0, // no query vector in resolve (BFS is seed-driven, not semantic)
			Rec:  scoring.Recency(ts, now, 30*24*time.Hour),
			Edge: scoring.Edge(c.Hop),
			Team: personScoreForSearch(ctx, h.db, req.AskerEEID, authorEEID),
			Auth: scoring.Authority(depth, 6),
		}
		out = append(out, scoredCand{
			NodeID: c.NodeID, Score: scoring.Combine(h.weights, bd),
			Breakdown: bd, Hop: c.Hop,
		})
	}
	return out
}

func (h *Resolve) checkScope(ctx context.Context, nodeID string, scopeSet map[string]bool) (bool, error) {
	row := h.db.QueryRow(ctx, `SELECT scope FROM graph.nodes WHERE id=$1`, nodeID)
	var scope *string
	if err := row.Scan(&scope); err != nil {
		return false, err
	}
	if scope == nil || *scope == "" {
		return true, nil // unscoped = open
	}
	if *scope == "public" {
		return true, nil // internal-public, visible to everyone
	}
	return scopeSet[*scope], nil
}

func (h *Resolve) enqueueFetchBody(ctx context.Context, nodeID string) {
	_, _ = h.db.Exec(ctx, `
INSERT INTO graph.jobs (type, payload, priority, machine_id)
VALUES ('fetch_body', jsonb_build_object('node_id', $1::text), 0, 'resolver')
ON CONFLICT DO NOTHING
`, nodeID)
}
