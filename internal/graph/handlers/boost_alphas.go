package handlers

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"

	"github.com/agent-mem/agent-mem/internal/graph/scoring"
)

// boostAlphasConfig is the Settings-page view of the search boosts: the
// editable graph.boost.alpha.* values plus the legacy graph.weights.* (still
// read by /api/graph/resolve) shown read-only.
type boostAlphasConfig struct {
	Alphas        scoring.BoostAlphas `json:"alphas"`
	ArmGraph      float64             `json:"arm_graph"`
	LegacyWeights map[string]float64  `json:"legacy_weights"`
}

// maxBoostAlpha keeps a boost from flipping the sign of a score: with
// components in [0,1], α ≤ 1 keeps every factor in [0.5, 1.5].
const maxBoostAlpha = 1.0

// getBoostAlphas serves GET /api/graph/boost-alphas.
func (h *Channels) getBoostAlphas(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := scoring.LoadBoostAlphas(ctx, h.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	lw, err := scoring.LoadWeights(ctx, h.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	armGraph, err := scoring.LoadGraphArmWeight(ctx, h.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(boostAlphasConfig{
		Alphas:   a,
		ArmGraph: armGraph,
		LegacyWeights: map[string]float64{
			"sem": lw.Sem, "rec": lw.Rec, "edge": lw.Edge, "team": lw.Team, "auth": lw.Auth,
		},
	})
}

// putBoostAlphas serves PUT /api/graph/boost-alphas with {"alphas": {...}, "arm_graph": ...}.
// Applies to the next search request (settings are read per request).
func (h *Channels) putBoostAlphas(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var cfg struct {
		Alphas   scoring.BoostAlphas `json:"alphas"`
		ArmGraph *float64            `json:"arm_graph"`
	}
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	vals := [4]float64{cfg.Alphas.Rec, cfg.Alphas.Team, cfg.Alphas.Temporal, cfg.Alphas.Auth}
	for i, v := range vals {
		if v < 0 || v > maxBoostAlpha || v != v {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("%s must be between 0 and %g", scoring.BoostAlphaKeys[i], maxBoostAlpha))
			return
		}
	}
	if cfg.ArmGraph != nil && (math.IsNaN(*cfg.ArmGraph) || math.IsInf(*cfg.ArmGraph, 0) || *cfg.ArmGraph < 0 || *cfg.ArmGraph > 1) {
		writeError(w, http.StatusBadRequest, "arm_graph must be between 0 and 1")
		return
	}
	tx, err := h.db.Begin(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback(r.Context())
	for i, v := range vals {
		if _, err := tx.Exec(r.Context(), `
INSERT INTO settings(key,value) VALUES($1,$2)
ON CONFLICT(key) DO UPDATE SET value=excluded.value`, scoring.BoostAlphaKeys[i], fmt.Sprintf("%g", v)); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if cfg.ArmGraph != nil {
		if _, err := tx.Exec(r.Context(), `
INSERT INTO settings(key,value) VALUES($1,$2)
ON CONFLICT(key) DO UPDATE SET value=excluded.value`, scoring.GraphArmWeightKey, fmt.Sprintf("%g", *cfg.ArmGraph)); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.getBoostAlphas(w, r)
}
