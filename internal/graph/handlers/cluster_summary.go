package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/bfs"
)

// clusterSummaryHandler answers GET /api/graph/cluster/summary?node=<id>&depth=N.
// It gathers the BFS cluster around a node, counts resources by source, and asks
// the LLM for a plain-language synthesis ("what is this, what happened on Slack")
// so the open-in-Graph overlay can show an answer instead of a wall of rows.
type clusterSummaryHandler struct {
	db     *pgxpool.Pool
	exp    *bfs.Expander
	gemini GeminiClient
}

func NewClusterSummary(deps Deps) http.HandlerFunc {
	h := &clusterSummaryHandler{db: deps.DB, exp: bfs.NewExpander(deps.DB), gemini: deps.Gemini}
	return h.serve
}

// friendlySource maps a node type to the same coarse buckets the dashboard uses.
func friendlySource(nodeType string) string {
	switch nodeType {
	case "jira":
		return "Jira"
	case "gh_pr":
		return "Pull Requests"
	case "cf", "cf_page":
		return "Confluence"
	case "slack", "slack_thread":
		return "Slack"
	case "slack_file":
		return "Files"
	case "feature":
		return "Features"
	case "person":
		return "People"
	default:
		return nodeType
	}
}

type clusterResource struct {
	Source string `json:"source"`
	Count  int    `json:"count"`
}

type clusterSummaryResponse struct {
	Overview   string            `json:"overview"`
	Highlights []string          `json:"highlights"`
	Resources  []clusterResource `json:"resources"`
	NodeCount  int               `json:"node_count"`
}

func (h *clusterSummaryHandler) serve(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.URL.Query().Get("node")
	if dec, err := url.QueryUnescape(id); err == nil {
		id = dec
	}
	if id == "" {
		http.Error(w, "node required", http.StatusBadRequest)
		return
	}
	depth, _ := strconv.Atoi(r.URL.Query().Get("depth"))
	if depth < 1 || depth > 3 {
		depth = 2
	}

	// BFS the cluster (bounded), collecting unique node ids.
	const maxNodes = 120
	seen := map[string]bool{id: true}
	ordered := []string{id}
	frontier := []struct {
		id  string
		hop int
	}{{id, 0}}
	for len(frontier) > 0 && len(ordered) < maxNodes {
		cur := frontier[0]
		frontier = frontier[1:]
		if cur.hop >= depth {
			continue
		}
		nbrs, err := h.exp.Expand(ctx, cur.id, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, n := range nbrs {
			if seen[n.NodeID] {
				continue
			}
			seen[n.NodeID] = true
			ordered = append(ordered, n.NodeID)
			frontier = append(frontier, struct {
				id  string
				hop int
			}{n.NodeID, cur.hop + 1})
			if len(ordered) >= maxNodes {
				break
			}
		}
	}

	// Load the cluster's nodes with the fields we need for counts + transcript.
	rows, err := h.db.Query(ctx, `
SELECT id, type, COALESCE(title,''), COALESCE(body,''),
       COALESCE(metadata->'author'->>'display_name',''),
       COALESCE(to_timestamp(NULLIF(metadata->>'ts','')::float8), first_seen_at) AS ts
FROM graph.nodes
WHERE id = ANY($1) AND deleted_at IS NULL`, ordered)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type clusterNode struct {
		typ, title, body, author string
		ts                       time.Time
	}
	counts := map[string]int{}
	var slackMsgs []clusterNode
	var otherTitles []string
	total := 0
	for rows.Next() {
		var n clusterNode
		var idCol string
		if err := rows.Scan(&idCol, &n.typ, &n.title, &n.body, &n.author, &n.ts); err != nil {
			rows.Close()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		total++
		src := friendlySource(n.typ)
		counts[src]++
		if n.typ == "slack" || n.typ == "slack_thread" {
			slackMsgs = append(slackMsgs, n)
		} else if t := strings.TrimSpace(n.title); t != "" {
			otherTitles = append(otherTitles, src+": "+firstLine(t, 120))
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := clusterSummaryResponse{NodeCount: total}
	for src, c := range counts {
		resp.Resources = append(resp.Resources, clusterResource{Source: src, Count: c})
	}
	sort.Slice(resp.Resources, func(i, j int) bool {
		if resp.Resources[i].Count != resp.Resources[j].Count {
			return resp.Resources[i].Count > resp.Resources[j].Count
		}
		return resp.Resources[i].Source < resp.Resources[j].Source
	})

	if h.gemini != nil && len(slackMsgs) > 0 {
		sort.Slice(slackMsgs, func(i, j int) bool { return slackMsgs[i].ts.Before(slackMsgs[j].ts) })
		var b strings.Builder
		if len(otherTitles) > 0 {
			b.WriteString("Linked resources:\n")
			for _, t := range otherTitles {
				b.WriteString("- " + t + "\n")
			}
			b.WriteString("\nSlack discussion (oldest first):\n")
		}
		for _, m := range slackMsgs {
			text := firstLine(m.body, 280)
			if text == "" {
				text = firstLine(m.title, 280)
			}
			if text == "" {
				continue
			}
			author := m.author
			if author == "" {
				author = "someone"
			}
			line := author + ": " + text + "\n"
			if b.Len()+len(line) > 7000 {
				break
			}
			b.WriteString(line)
		}
		if ov, hl := genClusterSummary(ctx, h.gemini, b.String()); ov != "" {
			resp.Overview = ov
			resp.Highlights = hl
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// genClusterSummary asks the LLM to synthesize the cluster. Returns ("", nil) on error.
func genClusterSummary(ctx context.Context, g GeminiClient, transcript string) (string, []string) {
	const sys = `You are given linked resources and a Slack discussion about one topic.
Write a factual synthesis for a teammate skimming this. Respond as JSON:
{"overview":"2-3 sentences: what this is about and the current state",
 "highlights":["chronological key events / decisions, each one short line, max 6 items"]}
Use names and ticket ids that appear in the text. No markdown, no quotes around the whole thing.`
	out, err := g.Generate(ctx, sys, transcript)
	if err != nil || out == "" {
		return "", nil
	}
	var parsed struct {
		Overview   string   `json:"overview"`
		Highlights []string `json:"highlights"`
	}
	if json.Unmarshal([]byte(out), &parsed) != nil {
		return "", nil
	}
	return strings.TrimSpace(parsed.Overview), parsed.Highlights
}
