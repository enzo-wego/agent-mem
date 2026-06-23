package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/bfs"
)

// NewNeighbors returns a chi.Router that owns /node/{id}/neighbors.
func NewNeighbors(db *pgxpool.Pool) chi.Router {
	r := chi.NewRouter()
	h := &neighborsHandler{db: db, exp: bfs.NewExpander(db)}
	r.Get("/node/{id}/neighbors", h.serve)
	return r
}

type neighborsHandler struct {
	db  *pgxpool.Pool
	exp *bfs.Expander
}

type neighborItem struct {
	Node struct {
		NodeID string `json:"node_id"`
		Type   string `json:"type"`
		URL    string `json:"url"`
		Title  string `json:"title"`
	} `json:"node"`
	Edge struct {
		Kind string `json:"kind"`
	} `json:"edge"`
	Hop int `json:"hop"`
}

func (h *neighborsHandler) serve(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	depth, _ := strconv.Atoi(r.URL.Query().Get("depth"))
	if depth < 1 || depth > 3 {
		depth = 1
	}
	kindFilter := r.URL.Query()["kind"]

	seen := map[string]bool{id: true}
	frontier := []struct {
		id  string
		hop int
	}{{id, 0}}
	var out []neighborItem
	for len(frontier) > 0 {
		next := frontier[0]
		frontier = frontier[1:]
		if next.hop >= depth {
			continue
		}

		nbrs, err := h.exp.Expand(ctx, next.id, kindFilter)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, n := range nbrs {
			if seen[n.NodeID] {
				continue
			}
			seen[n.NodeID] = true
			frontier = append(frontier, struct {
				id  string
				hop int
			}{n.NodeID, next.hop + 1})

			var item neighborItem
			item.Hop = next.hop + 1
			item.Edge.Kind = n.EdgeKind
			row := h.db.QueryRow(ctx,
				`SELECT id, type, COALESCE(url,''), COALESCE(title,'') FROM graph.nodes WHERE id=$1`, n.NodeID)
			if err := row.Scan(&item.Node.NodeID, &item.Node.Type,
				&item.Node.URL, &item.Node.Title); err != nil {
				continue
			}
			out = append(out, item)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"neighbors": out})
}
