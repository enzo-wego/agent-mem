package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

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
	db *pgxpool.Pool
}

// NewChannels creates a new Channels handler.
func NewChannels(db *pgxpool.Pool) *Channels {
	return &Channels{db: db}
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
	TS       string   `json:"ts"`
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
	rows, err := h.db.Query(ctx, `
SELECT id, COALESCE(title,''), LEFT(COALESCE(body,''),400), COALESCE(url,''),
       COALESCE(first_seen_at::text,''),
       COALESCE(metadata->>'thread_ts',''),
       COALESCE(metadata->'author'->>'display_name','')
FROM graph.nodes
WHERE scope = 'slack:' || $1 AND deleted_at IS NULL
  AND ($2 = 0 OR first_seen_at >= now() - make_interval(days => $2))
ORDER BY first_seen_at DESC
LIMIT $3`, id, days, limit)
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
		if err := rows.Scan(&m.ID, &m.Title, &m.Body, &m.URL, &m.TS, &m.ThreadTS, &m.Author); err != nil {
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
