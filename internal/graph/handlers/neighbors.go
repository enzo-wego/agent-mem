package handlers

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/acl"
	"github.com/agent-mem/agent-mem/internal/graph/bfs"
)

// NewNeighbors returns a chi.Router that owns /node/{id}/neighbors.
func NewNeighbors(db *pgxpool.Pool) chi.Router {
	r := chi.NewRouter()
	h := &neighborsHandler{db: db, exp: bfs.NewExpander(db), aclBld: acl.NewBuilder(db, 5*time.Minute)}
	r.Get("/node/{id}/neighbors", h.serve)
	return r
}

type neighborsHandler struct {
	db     *pgxpool.Pool
	exp    *bfs.Expander
	aclBld *acl.Builder
}

type neighborItem struct {
	Node struct {
		NodeID   string `json:"node_id"`
		Type     string `json:"type"`
		URL      string `json:"url"`
		Title    string `json:"title"`
		Overview string `json:"overview,omitempty"` // slack threads: 2-3 sentence summary, for the expanded row
		Channel  string `json:"channel"`   // slack only: human channel name (e.g. payments-dev), for display
		ThreadTS string `json:"thread_ts"` // slack only; lets the UI collapse a thread's messages into one row
		TSMs     int64  `json:"ts_ms"`     // node time (slack message ts, else first_seen_at), epoch millis
		// Slack threads only: first/last message time across the whole thread,
		// computed server-side because SIMILAR rows are leaves (one node in the
		// payload) so the client can't derive the span itself. 0 when unknown.
		FirstTSMs int64 `json:"first_ts_ms,omitempty"`
		LastTSMs  int64 `json:"last_ts_ms,omitempty"`
		// PendingSummary marks a row whose summarize job was just enqueued by
		// this request — the client can re-poll shortly to swap in the summary.
		PendingSummary bool `json:"pending_summary,omitempty"`
	} `json:"node"`
	Edge struct {
		Kind       string  `json:"kind"`
		Topic      string  `json:"topic,omitempty"`
		Why        string  `json:"why,omitempty"`
		Confidence float64 `json:"confidence,omitempty"`
		// Score is the embedding cosine for SIMILAR edges (omitted otherwise),
		// so the UI can explain why a semantic match was surfaced.
		Score float64 `json:"score,omitempty"`
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

	// ACL: a real asker (eeid != 0) only sees neighbors in scope; eeid 0 is the
	// trusted unfiltered view. Hidden nodes are neither surfaced nor traversed
	// through, so the walk can't leak private structure or content.
	eeid, scopeSet := askerScopeSet(ctx, h.db, h.aclBld, r.Header.Get("X-Asker-User"))
	noFilter := eeid == 0

	// ponytail: flat per-request cap on lazy summarize enqueues; per-thread dedup
	// lives in enqueueSummarizeThread.
	const maxLazySummarize = 12
	lazySummarized := 0

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
		// At the opened node, also surface semantically-related Slack threads that
		// share no explicit edge (e.g. the same incident discussed in other channels).
		// Root-only and unfiltered; these are leaves — we don't expand through them.
		if next.hop == 0 && len(kindFilter) == 0 && strings.HasPrefix(next.id, "slack:") {
			if sim, serr := h.exp.SimilarThreads(ctx, next.id); serr == nil {
				nbrs = append(nbrs, sim...) // failure is non-fatal: just no related threads
			}
		}
		for _, n := range nbrs {
			if seen[n.NodeID] {
				continue
			}
			seen[n.NodeID] = true

			var item neighborItem
			item.Hop = next.hop + 1
			item.Edge.Kind = n.EdgeKind
			item.Edge.Topic = n.Topic
			item.Edge.Why = n.Why
			item.Edge.Confidence = n.Confidence
			item.Edge.Score = n.Score
			// For Slack nodes, prefer the thread summary, then the first line of the
			// body — so a row shows readable text (and a whole thread one label),
			// never a raw slack:CHANNEL:TS id.
			var title, body, threadSummary string
			var scope *string
			row := h.db.QueryRow(ctx, `
SELECT n.id, n.type, COALESCE(n.url,''), COALESCE(n.title,''),
       LEFT(COALESCE(n.body,''),200),
       COALESCE(n.metadata->>'thread_ts',''),
       COALESCE(ts.summary,''),
       COALESCE(ts.overview,''),
       COALESCE(sc.name,''),
       (EXTRACT(EPOCH FROM COALESCE(n.created_at, to_timestamp(NULLIF(n.metadata->>'ts','')::float8), n.first_seen_at)) * 1000)::bigint,
       n.scope,
       COALESCE(tspan.first_ms, 0), COALESCE(tspan.last_ms, 0)
FROM graph.nodes n
LEFT JOIN graph.thread_summaries ts
  ON ts.channel_id = REPLACE(n.scope,'slack:','')
  AND ts.thread_ts = COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3))
LEFT JOIN graph.slack_channels sc
  ON sc.slack_channel_id = REPLACE(n.scope,'slack:','')
LEFT JOIN LATERAL (
  SELECT (EXTRACT(EPOCH FROM MIN(COALESCE(to_timestamp(NULLIF(m.metadata->>'ts','')::float8), m.created_at, m.first_seen_at))) * 1000)::bigint AS first_ms,
         (EXTRACT(EPOCH FROM MAX(COALESCE(to_timestamp(NULLIF(m.metadata->>'ts','')::float8), m.created_at, m.first_seen_at))) * 1000)::bigint AS last_ms
  FROM graph.nodes m
  WHERE n.type IN ('slack','slack_thread')
    AND m.scope = n.scope AND m.deleted_at IS NULL
    AND COALESCE(NULLIF(m.metadata->>'thread_ts',''), split_part(m.id,':',3))
      = COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3))
) tspan ON TRUE
WHERE n.id=$1`, n.NodeID)
			if err := row.Scan(&item.Node.NodeID, &item.Node.Type, &item.Node.URL,
				&title, &body, &item.Node.ThreadTS, &threadSummary, &item.Node.Overview, &item.Node.Channel, &item.Node.TSMs, &scope,
				&item.Node.FirstTSMs, &item.Node.LastTSMs); err != nil {
				continue
			}
			// Hidden from this asker: don't surface it and don't expand through it.
			if !scopeVisible(scope, scopeSet, noFilter) {
				continue
			}
			// Don't traverse through a SIMILAR link — surface related threads as leaves,
			// not as a launch point for further semantic drift.
			if n.EdgeKind != "SIMILAR" {
				frontier = append(frontier, struct {
					id  string
					hop int
				}{n.NodeID, next.hop + 1})
			}
			if item.Node.Type == "slack" || item.Node.Type == "slack_thread" {
				switch {
				case threadSummary != "":
					title = threadSummary
				case strings.TrimSpace(title) == "":
					title = firstLine(body, 120)
				}
				// Rows collapse to one-per-thread, so link the thread ROOT: stored
				// urls are unreliable here (backfill wrote bare slack.com links, and
				// a reply's permalink drops the reader mid-thread with no context).
				if scope != nil && strings.HasPrefix(*scope, "slack:") {
					rootTs := item.Node.ThreadTS
					if rootTs == "" {
						if parts := strings.Split(item.Node.NodeID, ":"); len(parts) == 3 {
							rootTs = parts[2]
						}
					}
					if u := slackPermalink("slack:" + strings.TrimPrefix(*scope, "slack:") + ":" + rootTs); u != "" {
						item.Node.URL = u
					}
				}
				// Lazily summarize threads the panel surfaced raw: only hot threads
				// get summarized proactively, so SIMILAR/THREAD rows often show the
				// first message verbatim. Enqueue (deduped, best-effort) so the next
				// open shows a summary. Bounded per request to protect LLM quota.
				if threadSummary == "" && lazySummarized < maxLazySummarize &&
					scope != nil && strings.HasPrefix(*scope, "slack:C") {
					tt := item.Node.ThreadTS
					if tt == "" {
						if parts := strings.Split(item.Node.NodeID, ":"); len(parts) == 3 {
							tt = parts[2]
						}
					}
					enqueueSummarizeThread(ctx, h.db, strings.TrimPrefix(*scope, "slack:"), tt, false)
					lazySummarized++
					item.Node.PendingSummary = true
				}
				// Never surface a raw slack:CHANNEL:TS id: when there's no summary or
				// body, fall back to a readable channel-scoped label.
				if strings.TrimSpace(title) == "" {
					if item.Node.Channel != "" {
						title = "Slack thread in #" + item.Node.Channel
					} else {
						title = "Slack thread"
					}
				}
			} else if strings.TrimSpace(title) == "" {
				title = firstLine(body, 120)
			}
			item.Node.Title = title
			// Drop un-enriched reference stubs: a node we linked to but never fetched
			// has no title and no url, so the panel would render a raw id like
			// "jira:RFC-53" or "feature:card_scan" that can't be opened. These are
			// noise, not resources. Slack nodes always get a synthesized label above,
			// so this only trims the never-fetched stubs.
			if strings.TrimSpace(item.Node.Title) == "" && item.Node.URL == "" {
				continue
			}
			out = append(out, item)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"neighbors": out})
}
