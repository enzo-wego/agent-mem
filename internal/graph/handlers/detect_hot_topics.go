package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// Hot-topic detection cadence and lookback. The detect job re-enqueues itself
// every detectInterval and considers threads active within detectLookback.
// Dedup (graph.topic_notifications) means a long-running hot thread fires once.
const (
	detectInterval = 5 * time.Minute
	detectLookback = 24 // hours
)

// subscription mirrors a graph.topic_subscriptions row.
type subscription struct {
	ID              int64    `json:"id"`
	SubscriberSlack string   `json:"subscriber_slack_id"`
	Topic           string   `json:"topic"`
	ChannelFilter   []string `json:"channel_filter"`
	MinParticipants int      `json:"min_participants"`
	MaxAuthorDepth  int      `json:"max_author_depth"`
	Active          bool     `json:"active"`
	CreatedAt       time.Time `json:"created_at"`
}

// hotThread is one candidate thread that crossed a hot trigger (seniority or
// volume). Topic relevance is decided later, semantically, in the handler.
type hotThread struct {
	RootNodeID   string
	Channel      string
	ChannelName  string
	MsgCount     int
	Participants int
	TopDepth     int
	TopAuthor    string
	FirstLine    string
	LastTS       time.Time
	Blob         string // concatenated message text, for semantic topic match
}

// NewDetectHotTopics returns the handler for the self-rescheduling
// 'detect_hot_topics' job. Each run scans every active subscription for hot
// threads, DMs new matches via enzobot, records them, then re-enqueues itself.
func NewDetectHotTopics(deps Deps) jobs.Handler {
	log := deps.Logger
	return func(ctx context.Context, _ []byte) error {
		// Always reschedule the next tick, even if this run hit per-sub errors, so
		// the chain never dies on a transient failure.
		defer func() {
			if _, err := jobs.Enqueue(ctx, deps.DB, "detect_hot_topics", map[string]any{},
				jobs.EnqueueOptions{
					AvailableAt:  time.Now().Add(detectInterval),
					TargetRunner: deps.Runner,
					MachineID:    deps.MachineID,
				}); err != nil {
				log.Warn().Err(err).Msg("detect_hot_topics: reschedule failed")
			}
		}()

		subs, err := listSubscriptions(ctx, deps.DB, true)
		if err != nil {
			return fmt.Errorf("list subscriptions: %w", err)
		}
		for _, s := range subs {
			hot, err := findHotThreads(ctx, deps.DB, s)
			if err != nil {
				log.Warn().Err(err).Int64("sub", s.ID).Msg("detect_hot_topics: query failed")
				continue
			}
			// Already-notified roots: skip re-evaluating (and re-embedding) them.
			notified := loadNotified(ctx, deps.DB, s.ID)
			// Embed the topic once for semantic matching. nil ⇒ keyword fallback.
			var topicVec []float32
			if deps.Gemini != nil {
				if v, e := deps.Gemini.Embed(ctx, s.Topic); e == nil {
					topicVec = v
				}
			}
			for _, h := range hot {
				if notified[h.RootNodeID] {
					continue
				}
				// Semantic topic gate: is this hot thread actually about the topic?
				if !topicMatches(ctx, deps, s, topicVec, h) {
					continue
				}
				// Dedup: claim the (sub, thread) pair; skip if already notified.
				ct, err := deps.DB.Exec(ctx,
					`INSERT INTO graph.topic_notifications(subscription_id, root_node_id)
					 VALUES ($1,$2) ON CONFLICT DO NOTHING`, s.ID, h.RootNodeID)
				if err != nil {
					log.Warn().Err(err).Msg("detect_hot_topics: dedup insert failed")
					continue
				}
				if ct.RowsAffected() == 0 {
					continue // already notified
				}
				to := s.SubscriberSlack
				if to == "" {
					to = deps.SlackDMUserID
				}
				if err := slackDM(ctx, deps.SlackBotToken, to, buildAlert(ctx, deps, s, h)); err != nil {
					log.Warn().Err(err).Str("to", to).Msg("detect_hot_topics: DM failed")
					// Roll back the dedup claim so a retry can re-send.
					_, _ = deps.DB.Exec(ctx,
						`DELETE FROM graph.topic_notifications WHERE subscription_id=$1 AND root_node_id=$2`,
						s.ID, h.RootNodeID)
					continue
				}
				log.Info().Int64("sub", s.ID).Str("node", h.RootNodeID).Msg("hot-topic alert sent")
			}
		}
		return nil
	}
}

// findHotThreads returns threads active in the lookback window with a real
// discussion — distinct participant count ≥ min_participants (so a lone message
// never alerts). The org-depth "seniority" trigger was dropped: depth_from_root
// is unreliable (most people default to 0), so it matched everyone and produced
// false positives. Topic relevance is NOT applied here — it is decided
// semantically in the handler, so a thread that never uses the topic word (e.g.
// "Juspay blocked pk" for topic "payments") can still match.
func findHotThreads(ctx context.Context, db *pgxpool.Pool, s subscription) ([]hotThread, error) {
	const q = `
WITH recent AS (
  SELECT n.id,
         replace(n.scope,'slack:','') AS channel,
         COALESCE(NULLIF(n.metadata->>'thread_ts',''), '') AS thread_ts,
         n.author_person_id,
         COALESCE(NULLIF(n.title,''), n.body, '') AS text,
         COALESCE(to_timestamp(NULLIF(n.metadata->>'ts','')::float8), n.first_seen_at) AS ts,
         COALESCE(p.depth_from_root, 99) AS depth,
         COALESCE(p.display_name, '') AS author
  FROM graph.nodes n
  LEFT JOIN graph.people p ON p.id = n.author_person_id
  WHERE n.type IN ('slack','slack_thread')
    AND n.deleted_at IS NULL
    AND n.scope LIKE 'slack:%'
    AND COALESCE(n.created_at, n.first_seen_at) >= NOW() - make_interval(hours => $1)
    AND ( $2::text[] = '{}'::text[] OR replace(n.scope,'slack:','') = ANY($2) )
),
grp AS (
  SELECT channel,
         CASE WHEN thread_ts <> '' THEN 'slack:'||channel||':'||thread_ts ELSE id END AS root_node_id,
         count(*)                                          AS msg_count,
         count(DISTINCT author_person_id)                  AS participants,
         min(depth)                                        AS top_depth,
         (array_agg(author ORDER BY depth ASC, ts ASC))[1] AS top_author,
         (array_agg(text   ORDER BY ts ASC))[1]            AS first_text,
         max(ts)                                           AS last_ts,
         string_agg(text, ' ')                             AS blob
  FROM recent
  GROUP BY 1, 2
)
SELECT g.root_node_id, g.channel, COALESCE(c.name,''),
       g.msg_count, g.participants, g.top_depth,
       COALESCE(g.top_author,''), COALESCE(g.first_text,''), g.last_ts,
       LEFT(COALESCE(g.blob,''), 2000)
FROM grp g
LEFT JOIN graph.slack_channels c ON c.slack_channel_id = g.channel
WHERE g.participants >= $3
ORDER BY g.last_ts DESC
LIMIT 50`
	filter := s.ChannelFilter
	if filter == nil {
		filter = []string{}
	}
	rows, err := db.Query(ctx, q, detectLookback, filter, s.MinParticipants)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []hotThread
	for rows.Next() {
		var h hotThread
		if err := rows.Scan(&h.RootNodeID, &h.Channel, &h.ChannelName,
			&h.MsgCount, &h.Participants, &h.TopDepth, &h.TopAuthor, &h.FirstLine, &h.LastTS, &h.Blob); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// topicSimThreshold is the minimum cosine similarity between the subscription
// topic and a thread for a semantic match. 0.50 cleanly separates real matches
// (a Juspay/PK-403 payments thread scored 0.512) from weak ones (off-topic FPs
// scored 0.47-0.48). Similarity is logged at Info so this can be re-tuned.
const topicSimThreshold = 0.50

// loadNotified returns the set of root_node_ids already notified for a sub.
func loadNotified(ctx context.Context, db *pgxpool.Pool, subID int64) map[string]bool {
	out := map[string]bool{}
	rows, err := db.Query(ctx,
		`SELECT root_node_id FROM graph.topic_notifications WHERE subscription_id=$1`, subID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}

// topicMatches decides whether a hot thread is about the subscription's topic.
// Primary path is semantic: cosine(topic, thread) ≥ threshold, so a thread that
// never uses the topic word still matches by meaning. Falls back to a keyword
// substring match when no embedder is available.
func topicMatches(ctx context.Context, deps Deps, s subscription, topicVec []float32, h hotThread) bool {
	if topicVec != nil && deps.Gemini != nil {
		bv, err := deps.Gemini.Embed(ctx, h.Blob)
		if err == nil && len(bv) > 0 {
			sim := cosine(topicVec, bv)
			// Logged at Info while the threshold is being tuned in prod; dial back to
			// Debug once topicSimThreshold is settled.
			deps.Logger.Info().Str("node", h.RootNodeID).Str("topic", s.Topic).
				Int("participants", h.Participants).Int("top_depth", h.TopDepth).
				Float64("sim", sim).Bool("match", sim >= topicSimThreshold).
				Msg("detect_hot_topics: topic similarity")
			return sim >= topicSimThreshold
		}
	}
	// Fallback: literal keyword over the thread text + channel name.
	hay := strings.ToLower(h.Blob + " " + h.ChannelName)
	return strings.Contains(hay, strings.ToLower(s.Topic))
}

// cosine returns the cosine similarity of two equal-length vectors (0 if either
// is empty or zero-norm).
func cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// alertMsg is one transcript line (who said what), with the author's org-depth
// so the senior speaker can be named in plain language.
type alertMsg struct {
	author string
	text   string
	depth  int
}

// buildAlert composes the DM for a hot-topic match: a plain-language briefing —
// what it's about (deep summary), the key points, a "who said what" transcript,
// and a jargon-free reason it was flagged. Falls back gracefully when the deep
// summary or names are missing.
func buildAlert(ctx context.Context, deps Deps, s subscription, h hotThread) string {
	channel := h.ChannelName
	if channel == "" {
		channel = h.Channel
	}
	threadTS := ""
	if parts := strings.SplitN(h.RootNodeID, ":", 3); len(parts) == 3 {
		threadTS = parts[2]
	}

	// Transcript: every message in the thread, oldest first, with author + depth.
	var msgs []alertMsg
	if rows, err := deps.DB.Query(ctx, `
SELECT COALESCE(n.metadata->'author'->>'display_name',''),
       COALESCE(NULLIF(n.title,''), n.body, ''),
       COALESCE(p.depth_from_root, 99)
FROM graph.nodes n
LEFT JOIN graph.people p ON p.id = n.author_person_id
WHERE n.scope = 'slack:' || $1 AND n.deleted_at IS NULL
  AND ( n.id = $2 OR COALESCE(n.metadata->>'thread_ts','') = $3 )
ORDER BY COALESCE(to_timestamp(NULLIF(n.metadata->>'ts','')::float8), n.first_seen_at) ASC`,
		h.Channel, h.RootNodeID, threadTS); err == nil {
		for rows.Next() {
			var m alertMsg
			if rows.Scan(&m.author, &m.text, &m.depth) == nil {
				msgs = append(msgs, m)
			}
		}
		rows.Close()
	}

	// Deep summary: prefer the cached one; generate on the fly if it's not ready.
	var overview string
	var highlights []string
	var hlRaw []byte
	_ = deps.DB.QueryRow(ctx,
		`SELECT COALESCE(overview,''), COALESCE(highlights,'[]'::jsonb)
		 FROM graph.thread_summaries WHERE channel_id=$1 AND thread_ts=$2`,
		h.Channel, threadTS).Scan(&overview, &hlRaw)
	_ = json.Unmarshal(hlRaw, &highlights)
	if overview == "" && deps.Gemini != nil && len(msgs) > 0 {
		var tb strings.Builder
		for _, m := range msgs {
			a := m.author
			if a == "" {
				a = "someone"
			}
			tb.WriteString(a + ": " + firstLine(m.text, 280) + "\n")
		}
		_, overview, highlights = genThreadDeepSummary(ctx, deps.Gemini, tb.String())
	}

	var b strings.Builder
	fmt.Fprintf(&b, "🔥 *%s* · #%s\n", s.Topic, channel)
	if overview != "" {
		fmt.Fprintf(&b, "\n%s\n", overview)
	}
	if len(highlights) > 0 {
		b.WriteString("\n")
		for _, hl := range highlights {
			fmt.Fprintf(&b, "• %s\n", hl)
		}
	}
	if len(msgs) > 0 {
		b.WriteString("\n💬 *Conversation*\n")
		shown := 0
		for _, m := range msgs {
			t := firstLine(m.text, 160)
			if t == "" {
				continue
			}
			a := m.author
			if a == "" {
				a = "someone"
			}
			line := fmt.Sprintf("• *%s:* %s\n", a, t)
			if b.Len()+len(line) > 2600 {
				break
			}
			b.WriteString(line)
			shown++
			if shown >= 8 {
				if rest := len(msgs) - shown; rest > 0 {
					fmt.Fprintf(&b, "_…and %d more_\n", rest)
				}
				break
			}
		}
	}
	fmt.Fprintf(&b, "\n_Flagged because %s._\n", whyFlagged(h))
	if link := slackPermalink(h.RootNodeID); link != "" {
		b.WriteString(link)
	}
	return b.String()
}

// whyFlagged explains in plain language why the thread was surfaced. The thread
// matched the topic semantically and crossed the discussion threshold.
func whyFlagged(h hotThread) string {
	return fmt.Sprintf("%d people are discussing it", h.Participants)
}

// slackPermalink builds an archive link from a root node id slack:<chan>:<ts>.
func slackPermalink(rootNodeID string) string {
	parts := strings.SplitN(rootNodeID, ":", 3)
	if len(parts) != 3 || parts[0] != "slack" {
		return ""
	}
	digits := strings.ReplaceAll(parts[2], ".", "")
	if digits == "" {
		return ""
	}
	return fmt.Sprintf("https://wego.slack.com/archives/%s/p%s", parts[1], digits)
}

// ── Subscription CRUD over HTTP ──────────────────────────────────────────────

// Subscriptions serves the topic-subscription REST endpoints.
type Subscriptions struct {
	db          *pgxpool.Pool
	defaultUser string
	log         zerolog.Logger
}

// NewSubscriptions builds the subscription HTTP handler.
func NewSubscriptions(deps Deps) *Subscriptions {
	return &Subscriptions{db: deps.DB, defaultUser: deps.SlackDMUserID, log: deps.Logger}
}

// listSubscriptions loads subscriptions; activeOnly restricts to active rows.
func listSubscriptions(ctx context.Context, db *pgxpool.Pool, activeOnly bool) ([]subscription, error) {
	q := `SELECT id, subscriber_slack_id, topic, channel_filter,
	             min_participants, max_author_depth, active, created_at
	      FROM graph.topic_subscriptions`
	if activeOnly {
		q += ` WHERE active`
	}
	q += ` ORDER BY created_at DESC`
	rows, err := db.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []subscription
	for rows.Next() {
		var s subscription
		if err := rows.Scan(&s.ID, &s.SubscriberSlack, &s.Topic, &s.ChannelFilter,
			&s.MinParticipants, &s.MaxAuthorDepth, &s.Active, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (h *Subscriptions) list(w http.ResponseWriter, r *http.Request) {
	subs, err := listSubscriptions(r.Context(), h.db, false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if subs == nil {
		subs = []subscription{}
	}
	writeJSON(w, http.StatusOK, subs)
}

func (h *Subscriptions) create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Topic           string   `json:"topic"`
		SubscriberSlack string   `json:"subscriber_slack_id"`
		ChannelFilter   []string `json:"channel_filter"`
		MinParticipants *int     `json:"min_participants"`
		MaxAuthorDepth  *int     `json:"max_author_depth"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	req.Topic = strings.TrimSpace(req.Topic)
	if req.Topic == "" {
		http.Error(w, "topic required", http.StatusBadRequest)
		return
	}
	subscriber := strings.TrimSpace(req.SubscriberSlack)
	if subscriber == "" {
		subscriber = h.defaultUser
	}
	if subscriber == "" {
		http.Error(w, "no subscriber_slack_id and no default recipient configured", http.StatusBadRequest)
		return
	}
	minP := 2 // a real exchange (≥2 people); a lone message never alerts
	if req.MinParticipants != nil {
		minP = *req.MinParticipants
	}
	maxD := 2
	if req.MaxAuthorDepth != nil {
		maxD = *req.MaxAuthorDepth
	}
	filter := req.ChannelFilter
	if filter == nil {
		filter = []string{}
	}
	var s subscription
	err := h.db.QueryRow(r.Context(), `
		INSERT INTO graph.topic_subscriptions
		  (subscriber_slack_id, topic, channel_filter, min_participants, max_author_depth)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING id, subscriber_slack_id, topic, channel_filter,
		          min_participants, max_author_depth, active, created_at`,
		subscriber, req.Topic, filter, minP, maxD).
		Scan(&s.ID, &s.SubscriberSlack, &s.Topic, &s.ChannelFilter,
			&s.MinParticipants, &s.MaxAuthorDepth, &s.Active, &s.CreatedAt)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (h *Subscriptions) delete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if _, err := h.db.Exec(r.Context(), `DELETE FROM graph.topic_subscriptions WHERE id=$1`, id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
