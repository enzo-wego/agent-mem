package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// defaultContinents is returned by GET /api/graph/continents when no config row
// exists yet. It is also lazily inserted so subsequent edits have a baseline.
const defaultContinents = `{
  "continents": [
    {"id":"partners","label":"Payment Partners","color":"#d29922","center":[0,-75],"match":["ext-wego-"]},
    {"id":"core","label":"Payments Core","color":"#3fb950","center":[30,20],"match":["payments"]},
    {"id":"other","label":"Other","color":"#8b949e","center":[25,110],"match":["*"]}
  ],
  "overrides": {},
  "ignore": ["C0B1BR522F5"],
  "names": {
    "C08S954G2LX":"payments-alerts","C05RNSE8TBR":"payments-team","CUV9EAYGY":"payments-dev",
    "C0597404MS6":"payments-pull-requests","C06Q3JHUAUV":"payments-releases","C01T60D80JV":"payments-alerts-staging",
    "C0B1BR522F5":"payments-staging","C02NA2MA5K5":"payments-x-hotels-devs","C048WV1BZTK":"payments-x-flights-devs",
    "C04L5JN6GKB":"payments-x-mobile-devs","C051NJMRLF8":"payments-x-shopcash-devs","C06SCE1LXAA":"payments-x-backoffice-devs",
    "C011RFSBLP3":"ext-wego-checkout","C03K79A2S20":"ext-wego-tabby","C0736FUE03W":"ext-wego-juspay","C091REMLCAX":"ext-wego-triplea-juspay",
    "CCY420A3D":"flights-analysis","C04M1R6NQNB":"flights-supply-help","C029TRHS5HU":"disputes-hotels-production",
    "C02AD7A21UH":"disputes-flights-production","C031TA3JUMT":"offline-bookings","C04U4KATYUV":"value-added-tax",
    "C08SVNFA30R":"taxes-core","C099FA175CY":"alerts-taxes-status","C09A46W6ZN1":"vat_data_ota_eg",
    "C09AHGY5WJV":"vat_data_ota_ksa","C09H1QMK882":"vat_data_ota_pk","CPP5EH3A8":"task-alerts-production",
    "C0A7D29E5ED":"alerts-itops-tech-and-ai-news","C012A121AQJ":"pm-design","C09USC3U9A9":"sandbox-enzo",
    "C0AJ3JPRA9L":"enzo-private","C0AV14LGPMG":"partner-saudi-rail"
  },
  "groups": {
    "S01TMG8Q65R":"payments-geeks"
  }
}`

// Channels serves the Globe feature endpoints: per-channel message volume and
// the channel→continent config stored in the public settings table.
type Channels struct {
	db *pgxpool.Pool
}

// NewChannels creates a new Channels handler.
func NewChannels(db *pgxpool.Pool) *Channels {
	return &Channels{db: db}
}

type channelCount struct {
	ChannelID string `json:"channel_id"`
	Count     int    `json:"count"`
	Name      string `json:"name"` // resolved Slack channel name, "" if unknown
}

// list handles GET /api/graph/channels. An optional ?days=N restricts the count
// to messages first seen in the last N days (0 or absent = all-time).
func (h *Channels) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	days := 0
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			days = n
		}
	}
	rows, err := h.db.Query(ctx, `
SELECT REPLACE(n.scope,'slack:','') AS channel_id, COUNT(*) AS count,
       COALESCE(sc.name,'') AS name
FROM graph.nodes n
LEFT JOIN graph.slack_channels sc ON sc.slack_channel_id = REPLACE(n.scope,'slack:','')
WHERE n.scope LIKE 'slack:%' AND n.scope NOT LIKE 'slack:D%' AND n.deleted_at IS NULL
  AND ($1 = 0 OR n.first_seen_at >= now() - make_interval(days => $1))
GROUP BY n.scope, sc.name
ORDER BY count DESC`, days)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []channelCount{}
	for rows.Next() {
		var c channelCount
		if err := rows.Scan(&c.ChannelID, &c.Count, &c.Name); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

type recentChannel struct {
	ChannelID string `json:"channel_id"`
	Name      string `json:"name"`
	Delta     int    `json:"delta"` // new messages in the window
	AtMs      int64  `json:"at_ms"` // most recent message, epoch millis (UTC)
}

// recent activity handles GET /api/graph/channels/recent?mins=N&limit=M — the
// top channels by new message volume in the last N minutes (default 30, top 5).
// Server-backed so the globe's activity ticker is identical for every viewer and
// populated on first load, replacing the old per-browser localStorage diff.
func (h *Channels) recentActivity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	mins := 30
	if v := r.URL.Query().Get("mins"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			mins = n
		}
	}
	limit := 5
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	rows, err := h.db.Query(ctx, `
SELECT REPLACE(n.scope,'slack:','') AS channel_id, COALESCE(sc.name,'') AS name,
       COUNT(*) AS delta,
       (EXTRACT(EPOCH FROM MAX(n.first_seen_at)) * 1000)::bigint AS at_ms
FROM graph.nodes n
LEFT JOIN graph.slack_channels sc ON sc.slack_channel_id = REPLACE(n.scope,'slack:','')
WHERE n.scope LIKE 'slack:%' AND n.scope NOT LIKE 'slack:D%' AND n.deleted_at IS NULL
  AND n.first_seen_at >= now() - make_interval(mins => $1)
GROUP BY n.scope, sc.name
ORDER BY delta DESC, at_ms DESC
LIMIT $2`, mins, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []recentChannel{}
	for rows.Next() {
		var c recentChannel
		if err := rows.Scan(&c.ChannelID, &c.Name, &c.Delta, &c.AtMs); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

type msgRef struct {
	Type string `json:"type"` // jira | gh_pr | cf | slack_file | ...
	Key  string `json:"key"`  // natural key, e.g. PAY-2204
	URL  string `json:"url"`
}

type channelMessage struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Body     string   `json:"body"`
	URL      string   `json:"url"`
	TSMs     int64    `json:"ts_ms"` // real Slack message time, epoch millis (UTC)
	ThreadTS string   `json:"thread_ts"`
	Author   string   `json:"author"`
	Refs     []msgRef `json:"refs"`
}

// recent handles GET /api/graph/channel?id=C...&days=N&limit=M — the most recent
// messages for a single channel, used by the map's click-to-see-data panel. Each
// message carries its thread_ts, author, and REFERENCES edges (linked artifacts).
func (h *Channels) recent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, `{"error":"id required"}`, http.StatusBadRequest)
		return
	}
	days := 0
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			days = n
		}
	}
	limit := 40
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	// Optional ?thread= restricts to one thread (root + replies) for lazy expand.
	thread := r.URL.Query().Get("thread")
	rows, err := h.db.Query(ctx, `
SELECT id, ''::text AS title, LEFT(COALESCE(body,''),400), COALESCE(url,''),
       (EXTRACT(EPOCH FROM COALESCE(to_timestamp(NULLIF(metadata->>'ts','')::float8), first_seen_at)) * 1000)::bigint AS ts_ms,
       COALESCE(metadata->>'thread_ts',''),
       COALESCE(metadata->'author'->>'display_name','')
FROM graph.nodes
WHERE scope = 'slack:' || $1 AND deleted_at IS NULL
  AND ($2 = 0 OR first_seen_at >= now() - make_interval(days => $2))
  AND ($3 = '' OR COALESCE(metadata->>'thread_ts','') = $3)
ORDER BY COALESCE(to_timestamp(NULLIF(metadata->>'ts','')::float8), first_seen_at) DESC
LIMIT $4`, id, days, thread, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []channelMessage{}
	byID := map[string]*channelMessage{}
	ids := []string{}
	for rows.Next() {
		var m channelMessage
		if err := rows.Scan(&m.ID, &m.Title, &m.Body, &m.URL, &m.TSMs, &m.ThreadTS, &m.Author); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		m.Refs = []msgRef{}
		out = append(out, m)
		ids = append(ids, m.ID)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for i := range out {
		byID[out[i].ID] = &out[i]
	}
	// Attach REFERENCES edges (linked Jira/PR/Confluence/Slack-file artifacts).
	if len(ids) > 0 {
		erows, err := h.db.Query(ctx, `
SELECT e.from_node_id, n.type, n.natural_key, COALESCE(n.url,'')
FROM graph.edges e
JOIN graph.nodes n ON n.id = e.to_node_id
WHERE e.kind = 'REFERENCES' AND e.from_node_id = ANY($1) AND n.deleted_at IS NULL`, ids)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer erows.Close()
		for erows.Next() {
			var from string
			var ref msgRef
			if err := erows.Scan(&from, &ref.Type, &ref.Key, &ref.URL); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if m := byID[from]; m != nil {
				m.Refs = append(m.Refs, ref)
			}
		}
		if err := erows.Err(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

type topicView struct {
	ThreadTS     string   `json:"thread_ts"`
	NodeID       string   `json:"node_id"` // graph node id of the root/standalone msg (for /neighbors)
	Summary      string   `json:"summary"` // one-line topic label
	Overview     string   `json:"overview"`    // 2-3 sentence deep summary (threads only)
	Highlights   []string `json:"highlights"`  // chronological key points (threads only)
	IsThread     bool     `json:"is_thread"`
	MsgCount     int      `json:"msg_count"`
	Participants []string `json:"participants"`
	FirstMs      int64    `json:"first_ms"`
	LastMs       int64    `json:"last_ms"`
	URL          string   `json:"url"`
	// Kind from the summarizer: "chatter" (leave notices, greetings, acks) is
	// hidden by the panel; ""/"substantive" shows.
	Kind string `json:"kind,omitempty"`
	// TopicGroup: views whose threads are SAME_TOPIC-linked share a group id,
	// so the panel can render one card per topic instead of one per thread.
	TopicGroup string `json:"topic_group,omitempty"`
}

// firstLine returns the first non-empty line of s, trimmed to n runes.
func firstLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// slackIDRe matches an unresolved raw Slack identifier: a user (U…), bot (B…), or
// workspace (W…) id — an uppercase-alnum run. Real display names carry lowercase
// or spaces, so this never rejects a genuine name.
var slackIDRe = regexp.MustCompile(`^[BUW][A-Z0-9]{6,}$`)

// looksLikeSlackID reports whether s is a raw Slack id rather than a resolved
// name. Such values leak into author chips when a bot name hasn't been resolved
// (refresh_slack_bots) or a user isn't in slack_users yet; hide them.
func looksLikeSlackID(s string) bool { return slackIDRe.MatchString(s) }

// withDept appends a person's department in parentheses ("Hazwan (Flights)") when
// known, so LLM transcripts and alert lines carry the team label. Returns the bare
// name when there's no department.
func withDept(author, dept string) string {
	if d := strings.TrimSpace(dept); d != "" {
		return author + " (" + d + ")"
	}
	return author
}

// topics handles GET /api/graph/channel/topics?id=&days=&limit= — thread-level
// rollups (one row per thread / standalone message) with a one-line topic
// summary. Multi-message threads get an LLM summary cached in
// graph.thread_summaries (keyed by msg-count+last-ts so it refreshes on growth).
func (h *Channels) topics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, `{"error":"id required"}`, http.StatusBadRequest)
		return
	}
	days := 0
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			days = n
		}
	}
	limit := 30
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}

	// Author: bot authors carry their raw bot_id in the metadata snapshot, so for
	// bots prefer the resolved person name (filled by refresh_slack_bots); for
	// humans keep the metadata snapshot, falling back to the person row.
	rows, err := h.db.Query(ctx, `
SELECT n.id, ''::text AS title, LEFT(COALESCE(n.body,''),600), COALESCE(n.url,''),
       (EXTRACT(EPOCH FROM COALESCE(to_timestamp(NULLIF(n.metadata->>'ts','')::float8), n.first_seen_at)) * 1000)::bigint AS ts_ms,
       COALESCE(n.metadata->>'thread_ts',''),
       CASE WHEN p.is_bot
            THEN COALESCE(NULLIF(p.display_name,''), '')
            ELSE COALESCE(NULLIF(n.metadata->'author'->>'display_name',''), p.display_name, '')
       END AS author
FROM graph.nodes n
LEFT JOIN graph.people p ON p.id = n.author_person_id
WHERE n.scope = 'slack:' || $1 AND n.deleted_at IS NULL
  AND ($2 = 0 OR n.first_seen_at >= now() - make_interval(days => $2))
ORDER BY COALESCE(to_timestamp(NULLIF(n.metadata->>'ts','')::float8), n.first_seen_at) ASC
LIMIT 3000`, id, days)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	groups := map[string]*threadGroup{}
	for rows.Next() {
		var m channelMessage
		if err := rows.Scan(&m.ID, &m.Title, &m.Body, &m.URL, &m.TSMs, &m.ThreadTS, &m.Author); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		key := m.ThreadTS
		if key == "" {
			key = m.ID
		}
		g := groups[key]
		if g == nil {
			g = &threadGroup{key: key, seen: map[string]bool{}}
			groups[key] = g
		}
		g.msgs = append(g.msgs, m)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Build views (msgs already ascending within a group).
	views := make([]*topicView, 0, len(groups))
	for _, g := range groups {
		v := &topicView{MsgCount: len(g.msgs)}
		for _, m := range g.msgs {
			if m.TSMs < v.FirstMs || v.FirstMs == 0 {
				v.FirstMs = m.TSMs
			}
			if m.TSMs > v.LastMs {
				v.LastMs = m.TSMs
			}
			if m.Author != "" && !looksLikeSlackID(m.Author) && !g.seen[m.Author] {
				g.seen[m.Author] = true
				v.Participants = append(v.Participants, m.Author)
			}
		}
		v.IsThread = len(g.msgs) > 1
		// thread_ts, node id + canonical url. Standalone groups are keyed by node
		// id; thread groups are keyed by thread_ts (root id = slack:<chan>:<ts>).
		if strings.HasPrefix(g.key, "slack:") {
			// Standalone message: its own ts is its effective thread id, so it
			// shares the summary cache/enqueue path (and the UI's expand key +
			// permalink fallback) with real threads.
			v.ThreadTS = g.key[strings.LastIndex(g.key, ":")+1:]
			v.NodeID = g.key
		} else {
			v.ThreadTS = g.key
			v.NodeID = "slack:" + id + ":" + g.key
		}
		v.URL = g.msgs[0].URL
		views = append(views, v)
	}
	// Most recent first; keep the top `limit`.
	sort.Slice(views, func(i, j int) bool { return views[i].LastMs > views[j].LastMs })
	if len(views) > limit {
		views = views[:limit]
	}

	// Summaries are READ-ONLY here so clicking a channel is instant. Every row —
	// threads AND standalone messages — uses the cached LLM topic if present; on
	// a miss we show the first line as a placeholder and enqueue a background
	// summarize_thread job. Standalone messages are only summarized on view (here),
	// never on ingest, so quiet channels don't burn LLM calls nobody reads.
	cacheKeys := []string{}
	rootText := map[int]string{}
	for i, v := range views {
		g := groups[groupKeyFor(v, groups)]
		if g == nil {
			continue
		}
		cacheKeys = append(cacheKeys, v.ThreadTS)
		rootText[i] = bodyOf(g.msgs[0])
	}
	cached := map[string]string{}            // thread_ts -> one-line topic
	cachedSig := map[string]string{}         // thread_ts -> signature it was generated for
	cachedOverview := map[string]string{}    // thread_ts -> deep overview
	cachedHl := map[string][]string{}        // thread_ts -> highlights
	cachedKind := map[string]string{}        // thread_ts -> substantive|chatter
	if len(cacheKeys) > 0 {
		crows, cerr := h.db.Query(ctx,
			`SELECT thread_ts, summary, COALESCE(signature,''), COALESCE(overview,''), COALESCE(highlights,'[]'::jsonb), COALESCE(kind,'')
			 FROM graph.thread_summaries WHERE channel_id=$1 AND thread_ts = ANY($2)`,
			id, cacheKeys)
		if cerr == nil {
			for crows.Next() {
				var tt, sum, sig, overview, kind string
				var hlRaw []byte
				if crows.Scan(&tt, &sum, &sig, &overview, &hlRaw, &kind) == nil {
					cached[tt] = sum
					cachedSig[tt] = sig
					cachedOverview[tt] = overview
					cachedKind[tt] = kind
					var hl []string
					if json.Unmarshal(hlRaw, &hl) == nil {
						cachedHl[tt] = hl
					}
				}
			}
			crows.Close()
		}
	}
	// Live signature per visible thread — the same count:lastMs that summarize_thread
	// keys on. A cached summary whose signature no longer matches is stale (a reply
	// arrived since it was generated), so we re-enqueue regeneration even though a
	// summary already exists. Without this, opening the channel never refreshes a
	// topic once it has any summary.
	liveSig := map[string]string{}
	if len(cacheKeys) > 0 {
		lrows, lerr := h.db.Query(ctx, `
SELECT COALESCE(NULLIF(metadata->>'thread_ts',''), split_part(id,':',3)), count(*),
       max((EXTRACT(EPOCH FROM updated_at) * 1000)::bigint)
FROM graph.nodes
WHERE scope = 'slack:' || $1 AND deleted_at IS NULL
  AND COALESCE(NULLIF(metadata->>'thread_ts',''), split_part(id,':',3)) = ANY($2)
GROUP BY 1`, id, cacheKeys)
		if lerr == nil {
			for lrows.Next() {
				var tt string
				var cnt int
				var last int64
				if lrows.Scan(&tt, &cnt, &last) == nil {
					// Must match summarize_thread's sig format ("v4:" prefix).
					liveSig[tt] = fmt.Sprintf("v7:%d:%d", cnt, last)
				}
			}
			lrows.Close()
		}
	}
	for i, v := range views {
		s, ok := cached[v.ThreadTS]
		if ok && s != "" {
			views[i].Summary = s // show the cached text (even if stale) rather than blank
		} else {
			views[i].Summary = firstLine(rootText[i], 90)
		}
		// Deep fields (overview + highlights) are shown when a thread is expanded.
		views[i].Overview = cachedOverview[v.ThreadTS]
		views[i].Highlights = cachedHl[v.ThreadTS]
		views[i].Kind = cachedKind[v.ThreadTS]
		// Refresh on a miss OR when the cached summary is stale vs the live thread.
		stale := !ok || s == "" || cachedSig[v.ThreadTS] != liveSig[v.ThreadTS]
		if stale {
			enqueueSummarizeThread(ctx, h.db, id, v.ThreadTS, false)
		}
	}

	// Topic grouping: threads confirmed SAME_TOPIC collapse into one card (e.g.
	// a bot-echoed RFC announcement, its epic, and the PRD-shared thread are one
	// initiative). Union-find over the SAME_TOPIC edges among the visible views;
	// the group id is the cluster's smallest node id, set only for real groups.
	if len(views) > 1 {
		nodeIDs := make([]string, len(views))
		parent := map[string]string{}
		var find func(string) string
		find = func(x string) string {
			if parent[x] == x {
				return x
			}
			parent[x] = find(parent[x])
			return parent[x]
		}
		for i, v := range views {
			nodeIDs[i] = v.NodeID
			parent[v.NodeID] = v.NodeID
		}
		erows, eerr := h.db.Query(ctx, `
SELECT from_node_id, to_node_id FROM graph.edges
WHERE kind='SAME_TOPIC' AND from_node_id = ANY($1) AND to_node_id = ANY($1)`, nodeIDs)
		if eerr == nil {
			for erows.Next() {
				var a, b string
				if erows.Scan(&a, &b) == nil {
					ra, rb := find(a), find(b)
					if ra != rb {
						if rb < ra {
							ra, rb = rb, ra
						}
						parent[rb] = ra
					}
				}
			}
			erows.Close()
			sizes := map[string]int{}
			for _, v := range views {
				sizes[find(v.NodeID)]++
			}
			for i, v := range views {
				if root := find(v.NodeID); sizes[root] > 1 {
					views[i].TopicGroup = root
				}
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(views)
}

// bodyOf returns the best display text for a message (title falls back to body).
func bodyOf(m channelMessage) string {
	if strings.TrimSpace(m.Title) != "" {
		return m.Title
	}
	return m.Body
}

// threadGroup accumulates the messages of one thread (or a standalone message).
type threadGroup struct {
	key  string
	msgs []channelMessage
	seen map[string]bool
}

// groupKeyFor recovers the groups-map key for a view: thread groups key by
// thread_ts, standalone groups by node id.
func groupKeyFor(v *topicView, groups map[string]*threadGroup) string {
	if v.ThreadTS != "" {
		if _, ok := groups[v.ThreadTS]; ok {
			return v.ThreadTS
		}
	}
	if _, ok := groups[v.NodeID]; ok {
		return v.NodeID
	}
	return ""
}

// getContinents handles GET /api/graph/continents. Returns the raw JSON stored
// under settings key graph_continents, lazily inserting the default if missing.
func (h *Channels) getContinents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var value string
	err := h.db.QueryRow(ctx, `SELECT value FROM settings WHERE key='graph_continents'`).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		// Lazily insert the default so future edits have a baseline.
		if _, ierr := h.db.Exec(ctx,
			`INSERT INTO settings(key,value) VALUES('graph_continents',$1) ON CONFLICT(key) DO NOTHING`,
			defaultContinents); ierr != nil {
			http.Error(w, ierr.Error(), http.StatusInternalServerError)
			return
		}
		value = defaultContinents
	} else if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, value)
}

// putContinents handles PUT /api/graph/continents. Validates the body parses as
// JSON, then upserts it under settings key graph_continents.
func (h *Channels) putContinents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// Limit request body to 64 KB.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, `{"error":"failed to read body"}`, http.StatusBadRequest)
		return
	}
	if !json.Valid(body) {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	if _, err := h.db.Exec(ctx,
		`INSERT INTO settings(key,value) VALUES('graph_continents',$1) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		string(body)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}
