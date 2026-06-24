package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

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
	db     *pgxpool.Pool
	gemini GeminiClient
}

// NewChannels creates a new Channels handler.
func NewChannels(db *pgxpool.Pool, gemini GeminiClient) *Channels {
	return &Channels{db: db, gemini: gemini}
}

type channelCount struct {
	ChannelID string `json:"channel_id"`
	Count     int    `json:"count"`
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
SELECT REPLACE(scope,'slack:','') AS channel_id, COUNT(*) AS count
FROM graph.nodes
WHERE scope LIKE 'slack:%' AND deleted_at IS NULL
  AND ($1 = 0 OR first_seen_at >= now() - make_interval(days => $1))
GROUP BY scope
ORDER BY count DESC`, days)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []channelCount{}
	for rows.Next() {
		var c channelCount
		if err := rows.Scan(&c.ChannelID, &c.Count); err != nil {
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
SELECT id, COALESCE(title,''), LEFT(COALESCE(body,''),400), COALESCE(url,''),
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
	Summary      string   `json:"summary"`
	IsThread     bool     `json:"is_thread"`
	MsgCount     int      `json:"msg_count"`
	Participants []string `json:"participants"`
	FirstMs      int64    `json:"first_ms"`
	LastMs       int64    `json:"last_ms"`
	URL          string   `json:"url"`
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

	rows, err := h.db.Query(ctx, `
SELECT id, COALESCE(title,''), LEFT(COALESCE(body,''),600), COALESCE(url,''),
       (EXTRACT(EPOCH FROM COALESCE(to_timestamp(NULLIF(metadata->>'ts','')::float8), first_seen_at)) * 1000)::bigint AS ts_ms,
       COALESCE(metadata->>'thread_ts',''),
       COALESCE(metadata->'author'->>'display_name','')
FROM graph.nodes
WHERE scope = 'slack:' || $1 AND deleted_at IS NULL
  AND ($2 = 0 OR first_seen_at >= now() - make_interval(days => $2))
ORDER BY COALESCE(to_timestamp(NULLIF(metadata->>'ts','')::float8), first_seen_at) ASC
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
			if m.Author != "" && !g.seen[m.Author] {
				g.seen[m.Author] = true
				v.Participants = append(v.Participants, m.Author)
			}
		}
		v.IsThread = len(g.msgs) > 1
		// thread_ts + canonical url: prefer the thread root (id ends with the key).
		if strings.HasPrefix(g.key, "slack:") {
			v.ThreadTS = "" // standalone message group keyed by node id
		} else {
			v.ThreadTS = g.key
		}
		v.URL = g.msgs[0].URL
		views = append(views, v)
	}
	// Most recent first; keep the top `limit`.
	sort.Slice(views, func(i, j int) bool { return views[i].LastMs > views[j].LastMs })
	if len(views) > limit {
		views = views[:limit]
	}

	// Resolve summaries: single message -> first line (no LLM); thread -> cache or LLM.
	type pending struct {
		idx  int
		sig  string
		txt  string
		root string
	}
	var todo []pending
	cacheKeys := []string{}
	for i, v := range views {
		g := groups[groupKeyFor(v, groups)]
		if g == nil || !v.IsThread {
			if g != nil {
				views[i].Summary = firstLine(bodyOf(g.msgs[0]), 90)
			}
			continue
		}
		sig := fmt.Sprintf("%d:%d", v.MsgCount, v.LastMs)
		cacheKeys = append(cacheKeys, v.ThreadTS)
		var b strings.Builder
		for _, m := range g.msgs {
			line := m.Author + ": " + firstLine(bodyOf(m), 200) + "\n"
			if b.Len()+len(line) > 4000 {
				break
			}
			b.WriteString(line)
		}
		todo = append(todo, pending{idx: i, sig: sig, txt: b.String(), root: bodyOf(g.msgs[0])})
	}

	// Load cached summaries (matching signature) in one query.
	cached := map[string]string{} // thread_ts -> summary (only if signature matches)
	if len(cacheKeys) > 0 {
		crows, cerr := h.db.Query(ctx,
			`SELECT thread_ts, signature, summary FROM graph.thread_summaries WHERE channel_id=$1 AND thread_ts = ANY($2)`,
			id, cacheKeys)
		if cerr == nil {
			defer crows.Close()
			for crows.Next() {
				var tt, sig, sum string
				if crows.Scan(&tt, &sig, &sum) == nil {
					cached[tt+"|"+sig] = sum
				}
			}
		}
	}

	// Summarize cache-misses with bounded concurrency.
	sem := make(chan struct{}, 6)
	var wg sync.WaitGroup
	for _, p := range todo {
		v := views[p.idx]
		if s, ok := cached[v.ThreadTS+"|"+p.sig]; ok {
			views[p.idx].Summary = s
			continue
		}
		if h.gemini == nil {
			views[p.idx].Summary = firstLine(p.root, 90)
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(p pending, threadTS string) {
			defer wg.Done()
			defer func() { <-sem }()
			sum := h.summarizeThread(ctx, p.txt)
			if sum == "" {
				sum = firstLine(p.root, 90)
			}
			views[p.idx].Summary = sum
			_, _ = h.db.Exec(ctx,
				`INSERT INTO graph.thread_summaries(channel_id,thread_ts,signature,summary,updated_at)
				 VALUES($1,$2,$3,$4,NOW())
				 ON CONFLICT (channel_id,thread_ts) DO UPDATE SET signature=excluded.signature, summary=excluded.summary, updated_at=NOW()`,
				id, threadTS, p.sig, sum)
		}(p, v.ThreadTS)
	}
	wg.Wait()

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

// groupKeyFor recovers the groups-map key for a view.
func groupKeyFor(v *topicView, groups map[string]*threadGroup) string {
	if v.ThreadTS != "" {
		return v.ThreadTS
	}
	// standalone: the only group whose single message url matches
	for k, g := range groups {
		if len(g.msgs) == 1 && g.msgs[0].URL == v.URL {
			return k
		}
	}
	return ""
}

// summarizeThread asks Gemini for a one-line topic label. Returns "" on any error.
func (h *Channels) summarizeThread(ctx context.Context, transcript string) string {
	const sys = `You label a Slack conversation with a short, factual topic (max 10 words). No quotes, no trailing period. Respond as JSON: {"topic":"..."}`
	out, err := h.gemini.Generate(ctx, sys, transcript)
	if err != nil || out == "" {
		return ""
	}
	var parsed struct {
		Topic string `json:"topic"`
	}
	if json.Unmarshal([]byte(out), &parsed) != nil {
		return ""
	}
	return firstLine(parsed.Topic, 90)
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
