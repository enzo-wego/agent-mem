package handlers

import (
	"context"
	"encoding/json"
	"fmt"
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
	case "gws_doc", "gws":
		return "Google Docs"
	default:
		return nodeType
	}
}

type clusterResource struct {
	Source string `json:"source"`
	Count  int    `json:"count"`
}

type clusterGraphNode struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title"`
	URL   string `json:"url"`
	Root  bool   `json:"root,omitempty"`
}

type clusterGraphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}

type clusterSummaryResponse struct {
	Overview   string             `json:"overview"`
	Highlights []string           `json:"highlights"`
	Resources  []clusterResource  `json:"resources"`
	Nodes      []clusterGraphNode `json:"nodes"`
	Edges      []clusterGraphEdge `json:"edges"`
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
			// Skip Slack DMs (slack:D…): private 1:1 conversations don't belong in a
			// shared cluster and have no channel name.
			if strings.HasPrefix(n.NodeID, "slack:D") {
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
SELECT id, type, COALESCE(title,''), COALESCE(url,''), COALESCE(body,''),
       COALESCE(metadata->'author'->>'display_name',''),
       COALESCE(to_timestamp(NULLIF(metadata->>'ts','')::float8), first_seen_at) AS ts,
       (EXTRACT(EPOCH FROM updated_at) * 1000)::bigint AS upd
FROM graph.nodes
WHERE id = ANY($1) AND deleted_at IS NULL`, ordered)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type clusterNode struct {
		id, typ, title, body, author, dept string
		ts                                 time.Time
		depth                              int // author org-depth (0=CEO); -1 unknown
	}
	counts := map[string]int{}
	var slackMsgs []clusterNode
	var otherTitles []string
	var gnodes []clusterGraphNode
	present := map[string]bool{}
	total := 0
	var maxUpdated int64 // newest updated_at across all cluster nodes (signature input)
	var rootType, rootTitle string
	for rows.Next() {
		var n clusterNode
		var urlCol string
		var upd int64
		if err := rows.Scan(&n.id, &n.typ, &n.title, &urlCol, &n.body, &n.author, &n.ts, &upd); err != nil {
			rows.Close()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if upd > maxUpdated {
			maxUpdated = upd
		}
		idCol := n.id
		total++
		present[idCol] = true
		// Node label: real title, else the first line of the body (Slack messages).
		label := strings.TrimSpace(n.title)
		if label == "" {
			label = firstLine(n.body, 80)
		}
		if idCol == id {
			rootType = n.typ
			rootTitle = label
		}
		gnodes = append(gnodes, clusterGraphNode{
			ID: idCol, Type: n.typ, Title: label, URL: urlCol, Root: idCol == id,
		})
		src := friendlySource(n.typ)
		counts[src]++
		// Slack messages are gathered later from full threads (with seniority); here
		// we only collect non-slack resource titles.
		if n.typ != "slack" && n.typ != "slack_thread" {
			if t := strings.TrimSpace(n.title); t != "" {
				otherTitles = append(otherTitles, src+": "+firstLine(t, 120))
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Induced edges: only those whose both endpoints survived in the cluster.
	var gedges []clusterGraphEdge
	erows, err := h.db.Query(ctx,
		`SELECT from_node_id, to_node_id, kind FROM graph.edges
		 WHERE from_node_id = ANY($1) AND to_node_id = ANY($1)`, ordered)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for erows.Next() {
		var e clusterGraphEdge
		if err := erows.Scan(&e.From, &e.To, &e.Kind); err != nil {
			erows.Close()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if present[e.From] && present[e.To] {
			gedges = append(gedges, e)
		}
	}
	erows.Close()
	if err := erows.Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := clusterSummaryResponse{NodeCount: total, Nodes: gnodes, Edges: gedges}
	for src, c := range counts {
		resp.Resources = append(resp.Resources, clusterResource{Source: src, Count: c})
	}
	sort.Slice(resp.Resources, func(i, j int) bool {
		if resp.Resources[i].Count != resp.Resources[j].Count {
			return resp.Resources[i].Count > resp.Resources[j].Count
		}
		return resp.Resources[i].Source < resp.Resources[j].Source
	})

	// Ground the summary in the FULL Slack discussion. Graph edges don't connect a
	// thread's replies to its root, and a cluster spans several threads (e.g. the
	// originating report and the later fix), so we pull every distinct thread in the
	// cluster — not just the opened one — and merge all their messages (deduped by
	// id). Each message carries its author's org-depth (BambooHR; 0=CEO) so the LLM
	// can foreground the originating reporter and senior voices.
	seenMsg := map[string]bool{}
	for _, m := range slackMsgs {
		seenMsg[m.id] = true
	}
	slackIDs := []string{}
	for _, gn := range gnodes {
		if gn.Type == "slack" || gn.Type == "slack_thread" {
			slackIDs = append(slackIDs, gn.ID)
		}
	}
	type threadKey struct{ channel, ts string }
	threads := map[threadKey]bool{}
	if rest, ok := strings.CutPrefix(id, "slack:"); ok { // always include the opened thread
		if parts := strings.SplitN(rest, ":", 2); len(parts) == 2 {
			var tt string
			_ = h.db.QueryRow(ctx, `SELECT COALESCE(NULLIF(metadata->>'thread_ts',''), $2) FROM graph.nodes WHERE id=$1`, id, parts[1]).Scan(&tt)
			if tt != "" {
				threads[threadKey{parts[0], tt}] = true
			}
		}
	}
	if len(slackIDs) > 0 {
		if trows, terr := h.db.Query(ctx, `
SELECT DISTINCT REPLACE(scope,'slack:',''), COALESCE(NULLIF(metadata->>'thread_ts',''), split_part(id,':',3))
FROM graph.nodes WHERE id = ANY($1) AND scope LIKE 'slack:%'`, slackIDs); terr == nil {
			for trows.Next() {
				var ch, tt string
				if trows.Scan(&ch, &tt) == nil && ch != "" && tt != "" {
					threads[threadKey{ch, tt}] = true
				}
			}
			trows.Close()
		}
	}
	for tk := range threads {
		trows, terr := h.db.Query(ctx, `
SELECT n.id, COALESCE(n.body,''), COALESCE(n.metadata->'author'->>'display_name',''),
       COALESCE(p.depth_from_root, -1), COALESCE(p.department,''),
       COALESCE(to_timestamp(NULLIF(n.metadata->>'ts','')::float8), n.first_seen_at) AS ts
FROM graph.nodes n
LEFT JOIN graph.people p ON p.id = n.author_person_id
WHERE n.scope = 'slack:' || $1 AND n.deleted_at IS NULL AND COALESCE(n.metadata->>'thread_ts','') = $2
ORDER BY ts ASC`, tk.channel, tk.ts)
		if terr != nil {
			continue
		}
		for trows.Next() {
			var m clusterNode
			m.typ = "slack"
			var depth int
			if trows.Scan(&m.id, &m.body, &m.author, &depth, &m.dept, &m.ts) == nil && m.body != "" && !seenMsg[m.id] {
				m.depth = depth
				slackMsgs = append(slackMsgs, m)
				seenMsg[m.id] = true
			}
		}
		trows.Close()
	}

	// Standalone messages: a cluster's Slack nodes are often non-threaded (e.g. bot
	// notifications like "@x created Task PAY-…"), which the thread pass above misses
	// because their thread_ts is empty. Pull every cluster Slack node's own body so
	// these still ground the summary instead of leaving it blank.
	if len(slackIDs) > 0 {
		srows, serr := h.db.Query(ctx, `
SELECT n.id, COALESCE(n.body,''), COALESCE(n.metadata->'author'->>'display_name',''),
       COALESCE(p.depth_from_root, -1), COALESCE(p.department,''),
       COALESCE(to_timestamp(NULLIF(n.metadata->>'ts','')::float8), n.first_seen_at) AS ts
FROM graph.nodes n
LEFT JOIN graph.people p ON p.id = n.author_person_id
WHERE n.id = ANY($1) AND n.deleted_at IS NULL AND COALESCE(n.body,'') <> ''`, slackIDs)
		if serr == nil {
			for srows.Next() {
				var m clusterNode
				m.typ = "slack"
				var depth int
				if srows.Scan(&m.id, &m.body, &m.author, &depth, &m.dept, &m.ts) == nil && m.body != "" && !seenMsg[m.id] {
					m.depth = depth
					slackMsgs = append(slackMsgs, m)
					seenMsg[m.id] = true
				}
			}
			srows.Close()
		}
	}

	sort.Slice(slackMsgs, func(i, j int) bool { return slackMsgs[i].ts.Before(slackMsgs[j].ts) })

	// Cache key: the summary text is reused verbatim until the cluster's content
	// changes (so it stays consistent across clicks/sessions instead of being
	// re-generated — and re-worded — on every open). signature = node count +
	// message count + latest message ts.
	var lastMs int64
	if n := len(slackMsgs); n > 0 {
		lastMs = slackMsgs[n-1].ts.UnixMilli()
	}
	// v3: after the identity merge, author org-depth now resolves for merged people
	// (e.g. Ross = 0), so bump the version to regenerate summaries with leadership
	// weighting. The prefix invalidates anything cached under older logic.
	// v5: signature now reflects the cluster's full content state — node count plus
	// the newest updated_at across ALL member nodes (any type). So an add (count),
	// delete (count), or update to any Slack/Jira/PR/doc node (updated_at bumps)
	// invalidates the cache and the summary regenerates on the next open. lastMs is
	// kept so a new Slack reply also triggers it even if updated_at lags.
	// v6: author labels now carry the person's department ("Hazwan (Flights)"), so
	// bump the version to regenerate summaries with the team label.
	sig := fmt.Sprintf("v6:%d:%d:%d:%d", total, maxUpdated, len(slackMsgs), lastMs)

	var cachedSig, cachedOverview string
	var cachedHl []byte
	if err := h.db.QueryRow(ctx,
		`SELECT signature, overview, highlights FROM graph.cluster_summaries WHERE node_id=$1`, id).
		Scan(&cachedSig, &cachedOverview, &cachedHl); err == nil && cachedSig == sig {
		resp.Overview = cachedOverview
		_ = json.Unmarshal(cachedHl, &resp.Highlights)
	}

	// The opened ("root") node anchors the summary. When it's a Slack thread the
	// oldest message IS the originating report; when it's a Jira/PR/doc, the cluster
	// may also include unrelated threads pulled in only by a co-mention, so we anchor
	// on the root resource itself and do NOT crown an arbitrary thread's first message.
	rootIsSlack := rootType == "slack" || rootType == "slack_thread"

	if resp.Overview == "" && h.gemini != nil && len(slackMsgs) > 0 {
		var b strings.Builder
		if !rootIsSlack && rootTitle != "" {
			b.WriteString("PRIMARY RESOURCE (the summary is about this): " +
				friendlySource(rootType) + " — " + rootTitle + "\n\n")
		}
		if len(otherTitles) > 0 {
			b.WriteString("Linked resources:\n")
			for _, t := range otherTitles {
				b.WriteString("- " + t + "\n")
			}
			b.WriteString("\nSlack discussion (oldest first):\n")
		}
		for i, m := range slackMsgs {
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
			author = withDept(author, m.dept) // "Hazwan (Flights)"
			// Tag seniority (lower org-depth = more senior) and the originating msg so
			// the LLM can foreground who raised it and weight leadership input.
			if m.depth >= 0 && m.depth <= 2 {
				author += " [leadership]"
			}
			if i == 0 && rootIsSlack {
				author += " [originator]"
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
			hlJSON, e := json.Marshal(hl)
			if e != nil || hlJSON == nil {
				hlJSON = []byte("[]")
			}
			_, _ = h.db.Exec(ctx,
				`INSERT INTO graph.cluster_summaries(node_id,signature,overview,highlights,updated_at)
				 VALUES($1,$2,$3,$4,NOW())
				 ON CONFLICT (node_id) DO UPDATE SET
				   signature=excluded.signature, overview=excluded.overview,
				   highlights=excluded.highlights, updated_at=NOW()`,
				id, sig, ov, hlJSON)
		}
	}

	// Never emit nil slices: Go marshals them to JSON null, and the dashboard does
	// s.highlights.length / .map on these — null.length throws, unmounting the whole
	// page (the "click summary → white screen after the LLM returns" bug).
	if resp.Highlights == nil {
		resp.Highlights = []string{}
	}
	if resp.Resources == nil {
		resp.Resources = []clusterResource{}
	}
	if resp.Nodes == nil {
		resp.Nodes = []clusterGraphNode{}
	}
	if resp.Edges == nil {
		resp.Edges = []clusterGraphEdge{}
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

EMPHASIS:
- If a "PRIMARY RESOURCE" line is given, the summary is ABOUT that resource. Center the
  overview on it. The Slack discussion is supporting context — some messages may be only
  loosely related (pulled in because they mention a ticket id in passing), so do NOT
  force a single narrative tying every ticket together, and do NOT open with an unrelated
  ticket just because its message is oldest.
- If a message is tagged [originator], START the overview with what it raised (who, and
  the actual problem/request). This is the most important context; never omit it.
- Give extra weight to messages tagged [leadership] (senior people); surface their
  asks and decisions explicitly and attribute them by name.
- Strip the [originator]/[leadership] tags from your output — they are hints, not text.

STRICT GROUNDING RULES — follow exactly:
- Use ONLY facts, names, and ticket ids that literally appear in the provided text.
- NEVER invent ticket ids (e.g. JIRA-123), people, dates, fixes, or outcomes. If it
  is not in the text, do not state it.
- Do NOT assert that two tickets are related unless the text explicitly says so; a
  shared channel or thread is not a relationship.
- Do not assume the issue was resolved/deployed unless the text says so.
- If the text is thin or inconclusive, write a short overview and return fewer (or
  zero) highlights rather than filling gaps.
No markdown, no quotes around the whole thing.`
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
