package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pins serves the pinned-thread REST endpoints backing the /live 📌 PINS
// panel. A pin bookmarks one Slack thread; list() enriches each pin with the
// thread's live latest message (graph.nodes) and its cached topic summary
// (graph.thread_summaries), so a pinned discussion is checkable at a glance.
type Pins struct {
	db *pgxpool.Pool
}

// NewPins builds the pins HTTP handler.
func NewPins(db *pgxpool.Pool) *Pins { return &Pins{db: db} }

// pinnedThread is one row of GET /api/graph/pins.
type pinnedThread struct {
	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	ThreadTS    string `json:"thread_ts"`
	NodeID      string `json:"node_id"`
	Summary     string `json:"summary"`
	Overview    string `json:"overview"`
	MsgCount    int    `json:"msg_count"`
	LastMs      int64  `json:"last_ms"`
	LastAuthor  string `json:"last_author"`
	LastBody    string `json:"last_body"`
	URL         string `json:"url"`
	PinnedAtMs  int64  `json:"pinned_at_ms"`
}

// list handles GET /api/graph/pins — every pin, newest-pinned first, enriched
// with live thread stats + latest message + cached summary. Stale or missing
// summaries re-enqueue summarize_thread (same signature check as channels.topics)
// so the panel's summary keeps up with thread growth.
func (h *Pins) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := h.db.Query(ctx, `
SELECT p.channel_id, COALESCE(sc.name,''), p.thread_ts,
       (EXTRACT(EPOCH FROM p.pinned_at)*1000)::bigint,
       COALESCE(ts.summary,''), COALESCE(ts.overview,''), COALESCE(ts.signature,''),
       COALESCE(st.cnt,0), COALESCE(st.last_ms,0), COALESCE(st.sig_ms,0),
       COALESCE(lm.author,''), COALESCE(lm.body,''), COALESCE(rt.url,'')
FROM graph.pinned_threads p
LEFT JOIN graph.slack_channels sc ON sc.slack_channel_id = p.channel_id
LEFT JOIN graph.thread_summaries ts
       ON ts.channel_id = p.channel_id AND ts.thread_ts = p.thread_ts
LEFT JOIN LATERAL (
  SELECT count(*) AS cnt,
         max((EXTRACT(EPOCH FROM COALESCE(to_timestamp(NULLIF(n.metadata->>'ts','')::float8), n.first_seen_at))*1000)::bigint) AS last_ms,
         max((EXTRACT(EPOCH FROM n.updated_at)*1000)::bigint) AS sig_ms
  FROM graph.nodes n
  WHERE n.scope = 'slack:' || p.channel_id AND n.deleted_at IS NULL
    AND COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3)) = p.thread_ts
) st ON true
LEFT JOIN LATERAL (
  SELECT CASE WHEN pe.is_bot
              THEN COALESCE(NULLIF(pe.display_name,''), '')
              ELSE COALESCE(NULLIF(n.metadata->'author'->>'display_name',''), pe.display_name, '')
         END AS author,
         LEFT(COALESCE(n.body,''),200) AS body
  FROM graph.nodes n
  LEFT JOIN graph.people pe ON pe.id = n.author_person_id
  WHERE n.scope = 'slack:' || p.channel_id AND n.deleted_at IS NULL
    AND COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3)) = p.thread_ts
  ORDER BY COALESCE(to_timestamp(NULLIF(n.metadata->>'ts','')::float8), n.first_seen_at) DESC
  LIMIT 1
) lm ON true
LEFT JOIN graph.nodes rt ON rt.id = 'slack:' || p.channel_id || ':' || p.thread_ts
ORDER BY p.pinned_at DESC`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	out := []pinnedThread{}
	for rows.Next() {
		var pt pinnedThread
		var storedSig string
		var sigMs int64
		if err := rows.Scan(&pt.ChannelID, &pt.ChannelName, &pt.ThreadTS, &pt.PinnedAtMs,
			&pt.Summary, &pt.Overview, &storedSig,
			&pt.MsgCount, &pt.LastMs, &sigMs,
			&pt.LastAuthor, &pt.LastBody, &pt.URL); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		pt.NodeID = "slack:" + pt.ChannelID + ":" + pt.ThreadTS
		if looksLikeSlackID(pt.LastAuthor) {
			pt.LastAuthor = ""
		}
		// Must match summarize_thread's sig format ("v7:" prefix, count:lastUpdatedMs).
		liveSig := fmt.Sprintf("v7:%d:%d", pt.MsgCount, sigMs)
		if pt.MsgCount > 0 && (pt.Summary == "" || storedSig != liveSig) {
			enqueueSummarizeThread(ctx, h.db, pt.ChannelID, pt.ThreadTS, false)
		}
		out = append(out, pt)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// create handles POST /api/graph/pins {channel_id, thread_ts}. Idempotent —
// re-pinning an already-pinned thread is a no-op.
func (h *Pins) create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ChannelID string `json:"channel_id"`
		ThreadTS  string `json:"thread_ts"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	req.ChannelID = strings.TrimSpace(req.ChannelID)
	req.ThreadTS = strings.TrimSpace(req.ThreadTS)
	if req.ChannelID == "" || req.ThreadTS == "" {
		http.Error(w, "channel_id and thread_ts required", http.StatusBadRequest)
		return
	}
	if _, err := h.db.Exec(r.Context(),
		`INSERT INTO graph.pinned_threads(channel_id, thread_ts) VALUES ($1,$2)
		 ON CONFLICT DO NOTHING`, req.ChannelID, req.ThreadTS); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// delete handles DELETE /api/graph/pins?channel=&thread=. Query params (not a
// path segment) because thread_ts contains a dot.
func (h *Pins) delete(w http.ResponseWriter, r *http.Request) {
	ch := strings.TrimSpace(r.URL.Query().Get("channel"))
	tt := strings.TrimSpace(r.URL.Query().Get("thread"))
	if ch == "" || tt == "" {
		http.Error(w, "channel and thread required", http.StatusBadRequest)
		return
	}
	if _, err := h.db.Exec(r.Context(),
		`DELETE FROM graph.pinned_threads WHERE channel_id=$1 AND thread_ts=$2`, ch, tt); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
