package handlers

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

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
		NodeID   string `json:"node_id"`
		Type     string `json:"type"`
		URL      string `json:"url"`
		Title    string `json:"title"`
		ThreadTS string `json:"thread_ts"` // slack only; lets the UI collapse a thread's messages into one row
	} `json:"node"`
	Edge struct {
		Kind string `json:"kind"`
	} `json:"edge"`
	Hop int `json:"hop"`
}

func (h *neighborsHandler) serve(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	// Node ids contain ':' which the client URL-encodes (%3A); chi does not decode
	// path params, so unescape here or the node lookup misses.
	if dec, err := url.PathUnescape(id); err == nil {
		id = dec
	}
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
			// For Slack nodes, prefer the thread summary, then the first line of the
			// body — so a row shows readable text (and a whole thread one label),
			// never a raw slack:CHANNEL:TS id.
			var title, body, threadSummary string
			row := h.db.QueryRow(ctx, `
SELECT n.id, n.type, COALESCE(n.url,''), COALESCE(n.title,''),
       LEFT(COALESCE(n.body,''),200),
       COALESCE(n.metadata->>'thread_ts',''),
       COALESCE(ts.summary,'')
FROM graph.nodes n
LEFT JOIN graph.thread_summaries ts
  ON ts.channel_id = REPLACE(n.scope,'slack:','')
  AND ts.thread_ts = COALESCE(n.metadata->>'thread_ts','')
WHERE n.id=$1`, n.NodeID)
			if err := row.Scan(&item.Node.NodeID, &item.Node.Type, &item.Node.URL,
				&title, &body, &item.Node.ThreadTS, &threadSummary); err != nil {
				continue
			}
			if item.Node.Type == "slack" || item.Node.Type == "slack_thread" {
				switch {
				case threadSummary != "":
					title = threadSummary
				case strings.TrimSpace(title) == "":
					title = firstLine(body, 120)
				}
			} else if strings.TrimSpace(title) == "" {
				title = firstLine(body, 120)
			}
			item.Node.Title = title
			out = append(out, item)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"neighbors": out})
}
