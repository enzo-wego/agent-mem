package scoring

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Weights are the per-component coefficients in the weighted sum.
type Weights struct {
	Sem  float64
	Rec  float64
	Edge float64
	Team float64
	Auth float64
}

// Components are the per-component normalised inputs.
type Components struct {
	Sem  float64 `json:"sem"`
	Rec  float64 `json:"rec"`
	Edge float64 `json:"edge"`
	Team float64 `json:"team"`
	Auth float64 `json:"auth"`
}

// Combine returns the weighted sum. No normalisation of weights — the
// caller is responsible for keeping them in [0,1] and (ideally) summing
// to ~1.0 so scores stay comparable across queries.
func Combine(w Weights, c Components) float64 {
	return w.Sem*c.Sem + w.Rec*c.Rec + w.Edge*c.Edge + w.Team*c.Team + w.Auth*c.Auth
}

// LoadWeights reads `graph.weights.{sem,rec,edge,team,auth}` from
// app_settings. Missing keys fall back to the design defaults.
func LoadWeights(ctx context.Context, db *pgxpool.Pool) (Weights, error) {
	w := Weights{Sem: 0.50, Rec: 0.15, Edge: 0.15, Team: 0.15, Auth: 0.05}
	rows, err := db.Query(ctx, `
SELECT key, value FROM public.settings
WHERE key IN ('graph.weights.sem','graph.weights.rec','graph.weights.edge',
              'graph.weights.team','graph.weights.auth')
`)
	if err != nil {
		return w, fmt.Errorf("load weights: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return w, err
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			continue
		}
		switch k {
		case "graph.weights.sem":
			w.Sem = f
		case "graph.weights.rec":
			w.Rec = f
		case "graph.weights.edge":
			w.Edge = f
		case "graph.weights.team":
			w.Team = f
		case "graph.weights.auth":
			w.Auth = f
		}
	}
	return w, nil
}
