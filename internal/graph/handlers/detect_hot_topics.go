package handlers

import (
	"context"
	"encoding/json"
	"fmt"
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

// hotThread is one candidate thread that matched a subscription's topic and
// crossed a trigger (seniority or volume).
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
			for _, h := range hot {
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
				if err := slackDM(ctx, deps.SlackBotToken, to, formatAlert(s, h)); err != nil {
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

// findHotThreads returns threads active in the lookback window that match the
// subscription's topic (in any message body or the channel name) AND crossed a
// trigger: either a senior author (org-depth ≤ max_author_depth, 0=CEO) posted,
// or the distinct participant count ≥ min_participants.
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
       COALESCE(g.top_author,''), COALESCE(g.first_text,''), g.last_ts
FROM grp g
LEFT JOIN graph.slack_channels c ON c.slack_channel_id = g.channel
WHERE ( g.blob ILIKE '%'||$3||'%' OR COALESCE(c.name,'') ILIKE '%'||$3||'%' )
  AND ( g.participants >= $4 OR g.top_depth <= $5 )
ORDER BY g.last_ts DESC
LIMIT 50`
	filter := s.ChannelFilter
	if filter == nil {
		filter = []string{}
	}
	rows, err := db.Query(ctx, q, detectLookback, filter, s.Topic, s.MinParticipants, s.MaxAuthorDepth)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []hotThread
	for rows.Next() {
		var h hotThread
		if err := rows.Scan(&h.RootNodeID, &h.Channel, &h.ChannelName,
			&h.MsgCount, &h.Participants, &h.TopDepth, &h.TopAuthor, &h.FirstLine, &h.LastTS); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// formatAlert builds the DM body for a hot-topic match.
func formatAlert(s subscription, h hotThread) string {
	channel := h.ChannelName
	if channel == "" {
		channel = h.Channel
	}
	var reason string
	switch {
	case h.TopDepth <= s.MaxAuthorDepth && h.Participants >= s.MinParticipants:
		reason = fmt.Sprintf("senior voice (org-depth %d) + %d people discussing", h.TopDepth, h.Participants)
	case h.TopDepth <= s.MaxAuthorDepth:
		reason = fmt.Sprintf("raised by a senior voice (org-depth %d)", h.TopDepth)
	default:
		reason = fmt.Sprintf("%d people discussing", h.Participants)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🔥 *%s* — #%s\n", s.Topic, channel)
	if h.FirstLine != "" {
		fmt.Fprintf(&b, "%s\n", firstLine(h.FirstLine, 220))
	}
	by := h.TopAuthor
	if by == "" {
		by = "someone"
	}
	fmt.Fprintf(&b, "_%s · %d msgs · led by %s_\n", reason, h.MsgCount, by)
	if link := slackPermalink(h.RootNodeID); link != "" {
		b.WriteString(link)
	}
	return b.String()
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
	minP := 4
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
