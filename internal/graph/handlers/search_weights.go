package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/scoring"
)

// NewSearchWeightsHandler serves GET and PUT /api/graph/search-weights.
func NewSearchWeightsHandler(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
		case http.MethodPut:
			var req struct {
				Rec   *float64 `json:"hybrid_rec"`
				KW    *float64 `json:"kw"`
				Title *float64 `json:"title"`
			}
			r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeError(w, http.StatusBadRequest, "invalid JSON")
				return
			}
			if req.Rec == nil || req.KW == nil || req.Title == nil {
				writeError(w, http.StatusBadRequest, "hybrid_rec, kw and title are required")
				return
			}
			if *req.Rec < 0 || *req.Rec > 1 || *req.KW < 0 || *req.KW > 1 || *req.Title < 0 || *req.Title > 1 {
				writeError(w, http.StatusBadRequest, "weights must be between 0 and 1")
				return
			}
			if err := saveSearchWeights(r.Context(), deps.DB, scoring.HybridWeights{Rec: *req.Rec, KW: *req.KW, Title: *req.Title}); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		hw, err := scoring.LoadHybridWeights(r.Context(), deps.DB)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, hw)
	})
}

func saveSearchWeights(ctx context.Context, db *pgxpool.Pool, w scoring.HybridWeights) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, setting := range []struct {
		key   string
		value float64
	}{
		{scoring.KeyHybridRec, w.Rec},
		{scoring.KeyHybridKW, w.KW},
		{scoring.KeyHybridTitle, w.Title},
	} {
		if err := putSetting(ctx, tx, setting.key, strconv.FormatFloat(setting.value, 'f', -1, 64)); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
