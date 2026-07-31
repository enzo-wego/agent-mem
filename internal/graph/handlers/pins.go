package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

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
	Kind        string `json:"kind,omitempty"` // thread_summaries.kind; "chatter" is filtered from the board section
	MsgCount    int    `json:"msg_count"`
	LastMs      int64  `json:"last_ms"`
	LastAuthor  string `json:"last_author"`
	LastBody    string `json:"last_body"`
	URL         string `json:"url"`
	PinnedAtMs  int64  `json:"pinned_at_ms"`
}

// threadRef identifies one Slack thread (channel + root ts).
type threadRef struct {
	ChannelID string
	ThreadTS  string
}

// enrichThreads returns display data for each (channel, thread) pair: channel
// name, cached summary, live message count + latest message, root URL. Order of
// the result matches no particular order — callers sort. Stale/missing summaries
// re-enqueue summarize_thread, same as channels.topics.
func (h *Pins) enrichThreads(ctx context.Context, refs []threadRef) (map[threadRef]pinnedThread, error) {
	if len(refs) == 0 {
		return map[threadRef]pinnedThread{}, nil
	}
	chans := make([]string, len(refs))
	threads := make([]string, len(refs))
	for i, r := range refs {
		chans[i], threads[i] = r.ChannelID, r.ThreadTS
	}
	rows, err := h.db.Query(ctx, `
WITH p AS (SELECT DISTINCT unnest($1::text[]) AS channel_id, unnest($2::text[]) AS thread_ts)
SELECT p.channel_id, COALESCE(sc.name,''), p.thread_ts,
       COALESCE(ts.summary,''), COALESCE(ts.overview,''), COALESCE(ts.signature,''), COALESCE(ts.kind,''),
       COALESCE(st.cnt,0), COALESCE(st.last_ms,0), COALESCE(st.sig_ms,0),
       COALESCE(lm.author,''), COALESCE(lm.body,''), COALESCE(rt.url,'')
FROM p
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
              ELSE COALESCE(NULLIF(CASE WHEN pe.display_name ~ '^[BU][A-Z0-9]{6,}$' THEN '' ELSE pe.display_name END,''), NULLIF(n.metadata->'author'->>'display_name',''), '')
         END AS author,
         LEFT(COALESCE(n.body,''),200) AS body
  FROM graph.nodes n
  LEFT JOIN graph.people pe ON pe.id = n.author_person_id
  WHERE n.scope = 'slack:' || p.channel_id AND n.deleted_at IS NULL
    AND COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3)) = p.thread_ts
  ORDER BY COALESCE(to_timestamp(NULLIF(n.metadata->>'ts','')::float8), n.first_seen_at) DESC
  LIMIT 1
) lm ON true
LEFT JOIN graph.nodes rt ON rt.id = 'slack:' || p.channel_id || ':' || p.thread_ts`,
		chans, threads)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[threadRef]pinnedThread{}
	for rows.Next() {
		var pt pinnedThread
		var storedSig, kind string
		var sigMs int64
		if err := rows.Scan(&pt.ChannelID, &pt.ChannelName, &pt.ThreadTS,
			&pt.Summary, &pt.Overview, &storedSig, &kind,
			&pt.MsgCount, &pt.LastMs, &sigMs,
			&pt.LastAuthor, &pt.LastBody, &pt.URL); err != nil {
			return nil, err
		}
		pt.NodeID = "slack:" + pt.ChannelID + ":" + pt.ThreadTS
		pt.Kind = kind
		if looksLikeSlackID(pt.LastAuthor) {
			pt.LastAuthor = ""
		}
		liveSig := fmt.Sprintf("v7:%d:%d", pt.MsgCount, sigMs)
		if pt.MsgCount > 0 && (pt.Summary == "" || storedSig != liveSig) {
			enqueueSummarizeThread(ctx, h.db, pt.ChannelID, pt.ThreadTS)
		}
		out[threadRef{pt.ChannelID, pt.ThreadTS}] = pt
	}
	return out, rows.Err()
}

// list handles GET /api/graph/pins — every pin, newest-pinned first, enriched
// with live thread stats + latest message + cached summary. Stale or missing
// summaries re-enqueue summarize_thread (same signature check as channels.topics)
// so the panel's summary keeps up with thread growth.
func (h *Pins) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	prow, err := h.db.Query(ctx, `
SELECT channel_id, thread_ts, (EXTRACT(EPOCH FROM pinned_at)*1000)::bigint
FROM graph.pinned_threads ORDER BY pinned_at DESC`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type pinRow struct {
		ref      threadRef
		pinnedMs int64
	}
	var pins []pinRow
	for prow.Next() {
		var pr pinRow
		if prow.Scan(&pr.ref.ChannelID, &pr.ref.ThreadTS, &pr.pinnedMs) == nil {
			pins = append(pins, pr)
		}
	}
	prow.Close()

	refs := make([]threadRef, len(pins))
	for i, p := range pins {
		refs[i] = p.ref
	}
	enriched, err := h.enrichThreads(ctx, refs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := []pinnedThread{}
	for _, p := range pins {
		pt, ok := enriched[p.ref]
		if !ok {
			pt = pinnedThread{ChannelID: p.ref.ChannelID, ThreadTS: p.ref.ThreadTS,
				NodeID: "slack:" + p.ref.ChannelID + ":" + p.ref.ThreadTS}
		}
		pt.PinnedAtMs = p.pinnedMs
		out = append(out, pt)
	}
	writeJSON(w, http.StatusOK, out)
}

// boardIssue is one board ticket that pinned-board threads reference.
type boardIssue struct {
	Key     string `json:"key"`
	Summary string `json:"summary"`
	Status  string `json:"status"`
}

// boardEpicGroup is one epic swimlane of GET /api/graph/pins/board.
type boardEpicGroup struct {
	EpicKey     string         `json:"epic_key"` // "" = tickets with no epic
	EpicSummary string         `json:"epic_summary"`
	EpicStatus  string         `json:"epic_status"`
	EpicRank    int            `json:"epic_rank"`    // board rank; boardEpicNoRank = off-board / no epic
	OnBoard     bool           `json:"-"`            // epic has live board issues; off-board epics are hidden
	ActiveCount int            `json:"active_count"` // threads with a new message inside the active window
	Issues      []boardIssue   `json:"issues"`
	Threads     []pinnedThread `json:"threads"`
	LastMs      int64          `json:"last_ms"` // newest thread activity in the group
}

// boardMaxThreadsPerEpic caps each swimlane; boardWindowDays caps how far back
// thread activity counts. ponytail: constants; querystring overrides if needed.
const (
	boardMaxThreadsPerEpic = 8
	boardWindowDays        = 60
)

// boardActiveHoursDefault is the default "recent activity" window (hours) for
// the board section's per-epic active count.
const boardActiveHoursDefault = 24

// boardActiveHours returns the active-window in hours: AGENT_MEM_BOARD_ACTIVE_HOURS
// when set to a positive integer, otherwise boardActiveHoursDefault (24).
func boardActiveHours() int {
	if v, err := strconv.Atoi(os.Getenv("AGENT_MEM_BOARD_ACTIVE_HOURS")); err == nil && v > 0 {
		return v
	}
	return boardActiveHoursDefault
}

// countActiveThreads returns how many threads had their latest message at or
// after cutoffMs (epoch ms) — threads with new activity in the active window.
func countActiveThreads(threads []pinnedThread, cutoffMs int64) int {
	n := 0
	for _, t := range threads {
		if t.LastMs >= cutoffMs {
			n++
		}
	}
	return n
}

// sortBoardGroups orders epic swimlanes to mirror the PAY board: by board epic
// rank ascending, newest thread activity breaking ties, and the "no epic" group
// always last regardless of rank.
func sortBoardGroups(groups []boardEpicGroup) {
	sort.Slice(groups, func(i, j int) bool {
		a, b := groups[i], groups[j]
		if aNo, bNo := a.EpicKey == "", b.EpicKey == ""; aNo != bNo {
			return !aNo // real epic before the no-epic group
		}
		if a.EpicRank != b.EpicRank {
			return a.EpicRank < b.EpicRank
		}
		return a.LastMs > b.LastMs
	})
}

// board handles GET /api/graph/pins/board — every Slack thread that REFERENCES
// a ticket in graph.jira_epic_map, grouped by the ticket's epic (the board's
// swimlane view). Chatter threads are excluded; groups sort by newest activity.
func (h *Pins) board(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := h.db.Query(ctx, `
SELECT DISTINCT
  REPLACE(t.scope,'slack:','') AS channel_id,
  COALESCE(NULLIF(t.metadata->>'thread_ts',''), split_part(t.id,':',3)) AS thread_ts,
  em.epic_key, em.epic_summary, em.epic_status, em.epic_rank, em.on_board,
  em.issue_key, em.issue_summary, em.issue_status
FROM graph.edges e
JOIN graph.nodes j ON j.id = e.to_node_id AND j.type='jira' AND j.deleted_at IS NULL
JOIN graph.jira_epic_map em ON em.issue_key = j.natural_key
JOIN graph.nodes t ON t.id = e.from_node_id AND t.deleted_at IS NULL AND t.scope LIKE 'slack:%'
WHERE e.kind = 'REFERENCES'
  AND t.first_seen_at >= now() - make_interval(days => $1)`, boardWindowDays)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	groups := map[string]*boardEpicGroup{}
	threadEpic := map[threadRef]string{}
	issueSeen := map[string]bool{}
	var refs []threadRef
	for rows.Next() {
		var ch, tt, ek, es, est, ik, isum, ist string
		var er int
		var onb bool
		if rows.Scan(&ch, &tt, &ek, &es, &est, &er, &onb, &ik, &isum, &ist) != nil {
			continue
		}
		g := groups[ek]
		if g == nil {
			g = &boardEpicGroup{EpicKey: ek, EpicSummary: es, EpicStatus: est, EpicRank: er, OnBoard: onb}
			groups[ek] = g
		}
		if !issueSeen[ek+"|"+ik] {
			issueSeen[ek+"|"+ik] = true
			g.Issues = append(g.Issues, boardIssue{Key: ik, Summary: isum, Status: ist})
		}
		ref := threadRef{ch, tt}
		if _, dup := threadEpic[ref]; !dup {
			threadEpic[ref] = ek
			refs = append(refs, ref)
		}
	}
	rows.Close()

	enriched, err := h.enrichThreads(ctx, refs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for ref, ek := range threadEpic {
		pt, ok := enriched[ref]
		if !ok || pt.Kind == "chatter" || pt.MsgCount == 0 {
			continue // hidden: chatter, or the thread has no visible messages
		}
		g := groups[ek]
		g.Threads = append(g.Threads, pt)
		if pt.LastMs > g.LastMs {
			g.LastMs = pt.LastMs
		}
	}

	hours := boardActiveHours()
	cutoffMs := time.Now().Add(-time.Duration(hours) * time.Hour).UnixMilli()
	out := []boardEpicGroup{}
	for _, g := range groups {
		if len(g.Threads) == 0 {
			continue
		}
		if g.EpicKey != "" && !g.OnBoard {
			continue // epic not live on board 193 — mirror the board's Epics panel (no-epic kept)
		}
		// Count over all eligible threads BEFORE the display cap, so the number
		// reflects real recent activity even when >8 threads exist.
		g.ActiveCount = countActiveThreads(g.Threads, cutoffMs)
		sort.Slice(g.Threads, func(i, j int) bool { return g.Threads[i].LastMs > g.Threads[j].LastMs })
		if len(g.Threads) > boardMaxThreadsPerEpic {
			g.Threads = g.Threads[:boardMaxThreadsPerEpic]
		}
		sort.Slice(g.Issues, func(i, j int) bool { return g.Issues[i].Key < g.Issues[j].Key })
		out = append(out, *g)
	}
	sortBoardGroups(out)
	writeJSON(w, http.StatusOK, map[string]any{"groups": out, "active_hours": hours})
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
