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
	"github.com/rs/zerolog/log"

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
	case "wegohub":
		return "Wego Hub"
	case "claude_artifact":
		return "Claude Artifact"
	default:
		return nodeType
	}
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
	Nodes      []clusterGraphNode `json:"nodes"`
	Edges      []clusterGraphEdge `json:"edges"`
	NodeCount  int                `json:"node_count"`
	// Sources maps the [T1]/[R1] markers cited inside Overview/Highlights to
	// the thread/resource each sentence came from, so the UI renders per-
	// sentence provenance as links.
	Sources map[string]clusterSource `json:"sources,omitempty"`
}

// clusterSource identifies where a cited transcript marker came from.
type clusterSource struct {
	NodeID string `json:"node_id"`
	Label  string `json:"label"`
	URL    string `json:"url,omitempty"`
}

// slackSourceRef labels a thread source as "#channel — topic" with a permalink.
func (h *clusterSummaryHandler) slackSourceRef(ctx context.Context, nodeID string) clusterSource {
	ref := clusterSource{NodeID: nodeID, Label: "Slack thread", URL: slackPermalink(nodeID)}
	parts := strings.SplitN(nodeID, ":", 3)
	if len(parts) != 3 {
		return ref
	}
	var chName, topic string
	_ = h.db.QueryRow(ctx, `
SELECT COALESCE(sc.name,''), COALESCE(ts.summary,'')
FROM (SELECT $1::text AS ch, $2::text AS tt) q
LEFT JOIN graph.slack_channels sc ON sc.slack_channel_id = q.ch
LEFT JOIN graph.thread_summaries ts ON ts.channel_id = q.ch AND ts.thread_ts = q.tt`,
		parts[1], parts[2]).Scan(&chName, &topic)
	switch {
	case chName != "" && topic != "":
		ref.Label = "#" + chName + " — " + firstLine(topic, 80)
	case chName != "":
		ref.Label = "#" + chName
	case topic != "":
		ref.Label = firstLine(topic, 80)
	}
	return ref
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
			// Entity tags, people and popular resources stay leaves here too —
			// otherwise the summary synthesizes "everything that ever said
			// apple pay" instead of this thread's cluster.
			if expandableThrough(ctx, h.db, n.NodeID) {
				frontier = append(frontier, struct {
					id  string
					hop int
				}{n.NodeID, cur.hop + 1})
			}
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
		id, typ, title, body, author, dept, jobTitle string
		src                                string // thread-root node id this message belongs to (provenance)
		ts                                 time.Time
		depth                              int // author org-depth (0=CEO); -1 unknown
	}
	type resRef struct{ label, id, url string }
	var slackMsgs []clusterNode
	var resRefs []resRef
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
		// Slack messages are gathered later from full threads (with seniority); here
		// we only collect non-slack resource titles.
		if n.typ != "slack" && n.typ != "slack_thread" {
			if t := strings.TrimSpace(n.title); t != "" {
				resRefs = append(resRefs, resRef{friendlySource(n.typ) + ": " + firstLine(t, 120), idCol, urlCol})
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
	// A structurally-reachable thread the rules judge REFUSED against the
	// opened thread must not ground the summary — its content is, verified,
	// a different topic (the "why is crypto in my pxx6xgkdtl summary" bug).
	// Unjudged threads stay: the popup queues their verdicts, and the excluded
	// count below is part of the cache signature so a new refusal regenerates.
	excludedRefused := 0
	for tk := range threads {
		rootID := "slack:" + tk.channel + ":" + tk.ts
		if rootID == id {
			continue
		}
		from, to := id, rootID
		if to < from {
			from, to = to, from
		}
		var same bool
		if err := h.db.QueryRow(ctx, `
SELECT same_topic FROM graph.topic_link_judgments
WHERE source_node_id=$1 AND target_node_id=$2`, from, to).Scan(&same); err == nil && !same {
			delete(threads, tk)
			excludedRefused++
		}
	}

	// Confirmed same-topic threads: the neighbor graph links the opened thread to
	// others with a SAME_TOPIC edge (rules-verified same topic), and the popup shows
	// a "Confirmed same topic" chip for them — but the LLM, told to center on the
	// primary and not force a narrative, otherwise drops their content, so the chip
	// has no matching prose. Collect those roots; below we flag them in the transcript
	// and require the LLM to cover each. Only meaningful when the root is a thread.
	sameTopicRoots := map[string]bool{}
	if strings.HasPrefix(id, "slack:") {
		if strows, serr := h.db.Query(ctx, `
SELECT CASE WHEN from_node_id=$1 THEN to_node_id ELSE from_node_id END
FROM graph.edges
WHERE kind='SAME_TOPIC' AND (from_node_id=$1 OR to_node_id=$1)`, id); serr == nil {
			for strows.Next() {
				var other string
				if strows.Scan(&other) == nil && strings.HasPrefix(other, "slack:") {
					sameTopicRoots[other] = true
				}
			}
			strows.Close()
		}
	}

	for tk := range threads {
		trows, terr := h.db.Query(ctx, `
SELECT n.id, COALESCE(n.body,''), COALESCE(NULLIF(CASE WHEN p.display_name ~ '^[BU][A-Z0-9]{6,}$' THEN '' ELSE p.display_name END,''), NULLIF(n.metadata->'author'->>'display_name',''), ''),
       COALESCE(p.depth_from_root, -1), COALESCE(p.department,''), COALESCE(p.job_title,''),
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
			m.src = "slack:" + tk.channel + ":" + tk.ts
			var depth int
			if trows.Scan(&m.id, &m.body, &m.author, &depth, &m.dept, &m.jobTitle, &m.ts) == nil && m.body != "" && !seenMsg[m.id] {
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
SELECT n.id, COALESCE(n.body,''), COALESCE(NULLIF(CASE WHEN p.display_name ~ '^[BU][A-Z0-9]{6,}$' THEN '' ELSE p.display_name END,''), NULLIF(n.metadata->'author'->>'display_name',''), ''),
       COALESCE(p.depth_from_root, -1), COALESCE(p.department,''), COALESCE(p.job_title,''),
       COALESCE(to_timestamp(NULLIF(n.metadata->>'ts','')::float8), n.first_seen_at) AS ts
FROM graph.nodes n
LEFT JOIN graph.people p ON p.id = n.author_person_id
WHERE n.id = ANY($1) AND n.deleted_at IS NULL AND COALESCE(n.body,'') <> ''`, slackIDs)
		if serr == nil {
			for srows.Next() {
				var m clusterNode
				m.typ = "slack"
				var depth int
				if srows.Scan(&m.id, &m.body, &m.author, &depth, &m.dept, &m.jobTitle, &m.ts) == nil && m.body != "" && !seenMsg[m.id] {
					m.depth = depth
					m.src = m.id // standalone message: it is its own source
					slackMsgs = append(slackMsgs, m)
					seenMsg[m.id] = true
				}
			}
			srows.Close()
		}
	}

	sort.Slice(slackMsgs, func(i, j int) bool { return slackMsgs[i].ts.Before(slackMsgs[j].ts) })

	// Source markers: one [T#] per thread (chronological first appearance) and
	// one [R#] per linked resource. Transcript lines carry them, the LLM cites
	// them per sentence, and the sources map below lets the UI render each
	// citation as a link. Built even on cache hits — cached text keeps markers.
	srcMarker := map[string]string{}
	sources := map[string]clusterSource{}
	for _, m := range slackMsgs {
		if m.src == "" || srcMarker[m.src] != "" {
			continue
		}
		marker := fmt.Sprintf("T%d", len(srcMarker)+1)
		srcMarker[m.src] = marker
		sources[marker] = h.slackSourceRef(ctx, m.src)
	}
	for i, rr := range resRefs {
		sources[fmt.Sprintf("R%d", i+1)] = clusterSource{NodeID: rr.id, Label: rr.label, URL: rr.url}
	}
	if len(sources) > 0 {
		resp.Sources = sources
	}

	// Markers of grounded threads that are confirmed same-topic neighbors of the
	// opened thread — the transcript flags these so the LLM can't drop them (sorted
	// for deterministic output).
	var sameTopicMarkers []string
	for src, mk := range srcMarker {
		if sameTopicRoots[src] {
			sameTopicMarkers = append(sameTopicMarkers, "["+mk+"]")
		}
	}
	sort.Strings(sameTopicMarkers)

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
	// v7: author now falls back to the resolved person's display_name (so bot
	// notifications attribute to the real actor instead of "someone"), and the
	// [leadership] hint is only tagged on known authors. Bump to regenerate
	// summaries that were cached with anonymous "Someone (leadership)" authors.
	// v9: transcript lines now carry [T#]/[R#] source markers and the LLM must
	// cite them per sentence. Bump so cached un-cited summaries regenerate.
	// v10: refused threads are excluded from the transcript and the originator
	// is the OPENED thread's root (not the cluster's oldest message) — both
	// change what the LLM sees, and a new refusal must regenerate the summary.
	// v11: confirmed same-topic threads are flagged in the transcript and the LLM
	// must cover each, so a newly-added SAME_TOPIC edge (which doesn't bump any
	// member node's updated_at) must invalidate the cached summary too.
	// v12: author labels now carry the BambooHR job title alongside the department
	// ("Lei Zheng (Engineering · Staff Software Engineer)"), so the LLM can weight
	// seniority from the transcript. Bump to regenerate title-less cached summaries.
	sig := fmt.Sprintf("v12:%d:%d:%d:%d:%d:%d", total, maxUpdated, len(slackMsgs), lastMs, excludedRefused, len(sameTopicRoots))

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

	if resp.Overview == "" && (h.gemini == nil || len(slackMsgs) == 0) {
		log.Info().Str("node", id).Bool("llm_nil", h.gemini == nil).
			Int("slack_msgs", len(slackMsgs)).Int("cluster_nodes", total).
			Msg("cluster summary: skipping LLM synthesis (no generator or no slack messages)")
	}
	if resp.Overview == "" && h.gemini != nil && len(slackMsgs) > 0 {
		var b strings.Builder
		if !rootIsSlack && rootTitle != "" {
			b.WriteString("PRIMARY RESOURCE (the summary is about this): " +
				friendlySource(rootType) + " — " + rootTitle + "\n\n")
		}
		if len(sameTopicMarkers) > 0 {
			b.WriteString("CONFIRMED SAME-TOPIC threads (rules-verified to be about the same topic as the primary — you MUST devote at least one highlight to the events in each, citing its marker; never drop them as loosely related): " +
				strings.Join(sameTopicMarkers, " ") + "\n\n")
		}
		if len(resRefs) > 0 {
			b.WriteString("Linked resources:\n")
			for i, rr := range resRefs {
				fmt.Fprintf(&b, "- [R%d] %s\n", i+1, rr.label)
			}
			b.WriteString("\nSlack discussion (oldest first):\n")
		}
		taggedOriginator := false
		for _, m := range slackMsgs {
			text := firstLine(m.body, 280)
			if text == "" {
				text = firstLine(m.title, 280)
			}
			if text == "" {
				continue
			}
			named := m.author != ""
			author := m.author
			if author == "" {
				author = "someone"
			}
			author = withDept(author, m.dept, m.jobTitle) // "Hazwan (Flights)"
			// Tag seniority (lower org-depth = more senior) and the originating msg so
			// the LLM can foreground who raised it and weight leadership input. Only tag
			// a known author — a "[leadership]" hint on an anonymous "someone" is useless
			// and leaks into the output as "Someone (leadership)".
			if named && m.depth >= 0 && m.depth <= 2 {
				author += " [leadership]"
			}
			// The originator is the OPENED thread's first message. Tagging the
			// cluster's chronologically-first message instead made the summary
			// open with whatever linked thread happened to be oldest.
			if rootIsSlack && !taggedOriginator && m.src == id {
				author += " [originator]"
				taggedOriginator = true
			}
			prefix := ""
			if mk := srcMarker[m.src]; mk != "" {
				prefix = "[" + mk + "] "
			}
			line := prefix + author + ": " + text + "\n"
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
  the actual problem/request). This is the most important context; never omit it. The
  [originator] belongs to the thread this summary is ABOUT; messages with other source
  markers are related context — never open the overview with them, and do not claim they
  are the origin of this topic.
- Give extra weight to messages tagged [leadership] (senior people); surface their
  asks and decisions explicitly and attribute them by name.
- If the input lists "CONFIRMED SAME-TOPIC threads" with markers, those threads are
  rules-verified to share the primary's topic. You MUST devote at least one highlight to
  the events in each such thread and cite its marker; never omit them as loosely related.
- An author may be written as "Name (Department)". When you name that person, keep
  their team label exactly as given on first mention (e.g. "Hazwan (Flights · Senior
  Engineer) reported…"). Never invent a department or job title that isn't given.
- Strip the [originator]/[leadership] tags from your output — they are hints, not text.

SOURCE CITATIONS — required:
- Every Slack line starts with a source marker like [T1]; linked resources are listed as [R1].
- End EVERY overview sentence and EVERY highlight with the marker(s) it is based on,
  e.g. "…was proposed. [T2]" or "…confirmed the fix. [T1][R2]".
- Cite only markers that appear in the input; never invent markers. Do NOT strip these —
  they are rendered as links to the source thread.

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
		log.Warn().Err(err).Bool("empty", out == "").Msg("cluster summary: LLM generate failed")
		return "", nil
	}
	var parsed struct {
		Overview   string   `json:"overview"`
		Highlights []string `json:"highlights"`
	}
	if uerr := json.Unmarshal([]byte(out), &parsed); uerr != nil {
		// Claude (unlike Gemini's responseMimeType) sometimes ignores "respond as
		// JSON" and answers in plain prose — a valid summary the old code threw
		// away, leaving the popup blank and re-calling the LLM on every open. When
		// the output isn't JSON-shaped, use the prose itself as the overview so it
		// renders and caches. See prose() for why we don't salvage broken JSON.
		if prose := prose(out); prose != "" {
			return prose, nil
		}
		log.Warn().Err(uerr).Str("raw", firstLine(out, 300)).Msg("cluster summary: LLM output not valid JSON")
		return "", nil
	}
	return strings.TrimSpace(parsed.Overview), parsed.Highlights
}

// prose salvages a non-JSON LLM reply as an overview. It returns the trimmed text
// only when it doesn't look like JSON — a reply starting with "{" is malformed
// JSON (e.g. truncated), not prose, and showing its raw braces to the user is
// worse than falling back to blank. Returns "" for JSON-shaped or empty input.
func prose(out string) string {
	out = strings.TrimSpace(out)
	if out == "" || strings.HasPrefix(out, "{") {
		return ""
	}
	return out
}
