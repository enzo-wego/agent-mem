package scoring

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RRFK is the standard reciprocal-rank-fusion constant: rank 1 contributes
// 1/61, rank 60 contributes 1/120, so a node near the top of one list beats a
// node in the middle of two.
const RRFK = 60

// Fused is one node's fused score and where each arm ranked it (1-based;
// arms that did not return the node are absent).
type Fused struct {
	Score float64        `json:"rrf"`
	Ranks map[string]int `json:"ranks"`
}

// Fuse combines per-arm ranked id lists by reciprocal rank: score(id) =
// Σ_arms 1/(k + rank). A duplicate id inside one arm's list keeps its first
// (best) rank. k <= 0 falls back to RRFK.
func Fuse(lists map[string][]string, k int) map[string]Fused {
	if k <= 0 {
		k = RRFK
	}
	out := make(map[string]Fused)
	for arm, ids := range lists {
		for i, id := range ids {
			f, ok := out[id]
			if !ok {
				f = Fused{Ranks: make(map[string]int, len(lists))}
			}
			if _, seen := f.Ranks[arm]; seen {
				continue
			}
			rank := i + 1
			f.Ranks[arm] = rank
			f.Score += 1 / float64(k+rank)
			out[id] = f
		}
	}
	return out
}

// BoostAlphas are the multiplicative boost strengths applied on top of the
// fused rank score: final = rrf × Π (1 + α·(component − 0.5)). 0 disables a
// boost; 0.2 lets a component move the score by ±10%.
type BoostAlphas struct {
	Rec      float64 `json:"rec"`
	Team     float64 `json:"team"`
	Temporal float64 `json:"temporal"`
	Auth     float64 `json:"auth"`
}

// DefaultBoostAlphas are the design values from the four-arm plan.
var DefaultBoostAlphas = BoostAlphas{Rec: 0.2, Team: 0.2, Temporal: 0.2, Auth: 0.1}

// Boost applies the alphas to a fused score. Every component is expected in
// [0,1]; 0.5 is neutral.
func Boost(rrf float64, a BoostAlphas, rec, team, temporal, auth float64) float64 {
	return rrf *
		(1 + a.Rec*(rec-0.5)) *
		(1 + a.Team*(team-0.5)) *
		(1 + a.Temporal*(temporal-0.5)) *
		(1 + a.Auth*(auth-0.5))
}

// BoostAlphaKeys are the public.settings keys behind BoostAlphas, in
// rec/team/temporal/auth order.
var BoostAlphaKeys = [4]string{
	"graph.boost.alpha.rec", "graph.boost.alpha.team",
	"graph.boost.alpha.temporal", "graph.boost.alpha.auth",
}

// LoadBoostAlphas reads `graph.boost.alpha.*` from public.settings; missing
// or unparsable keys keep DefaultBoostAlphas. Cheap enough to run per search
// request so a dashboard edit applies without a restart.
func LoadBoostAlphas(ctx context.Context, db *pgxpool.Pool) (BoostAlphas, error) {
	a := DefaultBoostAlphas
	rows, err := db.Query(ctx, `SELECT key, value FROM public.settings WHERE key = ANY($1)`, BoostAlphaKeys[:])
	if err != nil {
		return a, fmt.Errorf("load boost alphas: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return a, err
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			continue
		}
		switch k {
		case BoostAlphaKeys[0]:
			a.Rec = f
		case BoostAlphaKeys[1]:
			a.Team = f
		case BoostAlphaKeys[2]:
			a.Temporal = f
		case BoostAlphaKeys[3]:
			a.Auth = f
		}
	}
	return a, rows.Err()
}
