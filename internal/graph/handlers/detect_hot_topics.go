package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
	"github.com/agent-mem/agent-mem/internal/llmjson"
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
	ID               int64         `json:"id"`
	SubscriberSlack  string        `json:"subscriber_slack_id"`
	Topic            string        `json:"topic"`
	ChannelFilter    []string      `json:"channel_filter"`
	MinParticipants  int           `json:"min_participants"`
	MaxAuthorDepth   int           `json:"max_author_depth"`
	Active           bool          `json:"active"`
	CreatedAt        time.Time     `json:"created_at"`
	Sources          []topicSource `json:"sources"`
	ScopeDefinition  string        `json:"scope_definition"` // judge guidance; GUI-editable
	ScopeSummary     string        `json:"scope_summary"`    // human-readable, shown in UI
	ScopeStatus      string        `json:"scope_status"`
	ScopeError       string        `json:"scope_error"`
	ScopeRefreshedAt *time.Time    `json:"scope_refreshed_at,omitempty"`
}

// judgeTopicText returns the text the LLM judge should match against: the
// distilled scope definition when available, else the bare topic label.
func (s subscription) judgeTopicText() string {
	if strings.TrimSpace(s.ScopeDefinition) != "" {
		return s.ScopeDefinition
	}
	return s.Topic
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

	HasImportant    bool   // an author is in the subscriber's reporting line / near org circle
	ImportantAuthor string // name of that important author (for the DM)
	HasAlwaysAlert  bool   // an author explicitly bypasses topic relevance for this subscriber
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
		ignored := ignoredChannelIDs(ctx, deps.DB)
		for _, s := range subs {
			// Resolve the subscriber once, then derive both their wider important
			// circle and the explicit always-alert subset.
			owner := subscriberEEID(ctx, deps.DB, s.SubscriberSlack)
			important := ownerImportantEeids(ctx, deps.DB, owner)
			alwaysAlert := alwaysAlertEeids(ctx, deps.DB, owner)
			// Whose threads to stay out of: the person this sub DMs.
			subscriber := s.SubscriberSlack
			if subscriber == "" {
				subscriber = deps.SlackDMUserID
			}
			hot, err := findHotThreads(ctx, deps.DB, s, important, alwaysAlert, subscriber)
			if err != nil {
				log.Warn().Err(err).Int64("sub", s.ID).Msg("detect_hot_topics: query failed")
				continue
			}
			// Already-notified roots: skip re-evaluating them.
			notified := loadNotified(ctx, deps.DB, s.ID)
			// Cached judge verdicts: judge a thread once per size, don't re-roll
			// the nondeterministic LLM every tick — re-rolled ~288x/day it
			// eventually flips a borderline "false" to "true" hours later.
			judged := loadJudgments(ctx, deps.DB, s.ID)
			for _, h := range hot {
				if notified[h.RootNodeID] || ignored[h.Channel] {
					continue
				}
				if !h.HasAlwaysAlert {
					// Topic gate: is this hot thread genuinely ABOUT the topic? Decided
					// by an LLM judgment — cosine on a bare topic word couldn't tell a
					// deployment thread (0.52) from a real payments incident (0.512).
					// A cached verdict is reused until the thread's message count
					// changes; only LLM verdicts are cached (keyword fallback is free).
					if j, ok := judged[h.RootNodeID]; ok && j.msgCount == h.MsgCount {
						if !j.relevant {
							continue
						}
					} else {
						relevant, fromLLM := topicMatches(ctx, deps, s, h)
						if fromLLM {
							saveJudgment(ctx, deps.DB, s.ID, h.RootNodeID, h.MsgCount, relevant)
						}
						if !relevant {
							continue
						}
					}
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
				if to == "" || deps.SlackBotToken == "" {
					// Release the dedup claim either way, so the alert can still
					// fire on a later run once the config problem is fixed.
					_, _ = deps.DB.Exec(ctx,
						`DELETE FROM graph.topic_notifications WHERE subscription_id=$1 AND root_node_id=$2`,
						s.ID, h.RootNodeID)
					if deps.SlackBotToken == "" {
						// Misconfiguration, not a data condition — a hot topic
						// matched and was then silently dropped for 10 days
						// because the token went missing with the 2026-08-12
						// migration. Fail loudly (agent-mem-egsf).
						return fmt.Errorf("%w: detect_hot_topics: SLACK_BOT_TOKEN not set", jobs.ErrFatal)
					}
					// No recipient resolved anywhere (sub + default):
					// data-dependent no-op, not an error.
					continue
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

// findHotThreads returns threads active in the lookback window that crossed a
// bar: either a real discussion (distinct participants ≥ min_participants) OR an
// "important" person is involved — important = the subscriber's reporting line
// (ancestors via person_distance.lca) + anyone within ~2 org hops (the `important`
// eeid list). So a lone message from your manager/CEO/teammate surfaces, while a
// stranger's lone message does not. (The old org-DEPTH seniority trigger was
// dropped — depth_from_root is unreliable; this uses anchored org-distance.)
// Topic relevance is NOT applied here — it is decided semantically in the handler,
// so a thread that never uses the topic word ("Juspay blocked pk" for "payments")
// can still match.
//
// Threads the subscriber has posted in are dropped: Slack already notifies you
// about replies to your own threads, so a DM is pure echo. Pass "" to disable.
// ponytail: the check is windowed like everything else here — it sees only the
// messages inside detectLookback, so a thread you last spoke in >24h ago can
// still alert. Widen to a per-root EXISTS over graph.nodes if that shows up.
func findHotThreads(ctx context.Context, db *pgxpool.Pool, s subscription, important, alwaysAlert []int32, subscriber string) ([]hotThread, error) {
	const q = `
WITH recent AS (
  SELECT n.id,
         replace(n.scope,'slack:','') AS channel,
         COALESCE(NULLIF(n.metadata->>'thread_ts',''), '') AS thread_ts,
         n.author_person_id,
         COALESCE(NULLIF(n.body,''), n.title, '') AS text,
         COALESCE(a.att, '') AS att,
         COALESCE(to_timestamp(NULLIF(n.metadata->>'ts','')::float8), n.first_seen_at) AS ts,
         COALESCE(p.depth_from_root, 99) AS depth,
         COALESCE(p.display_name, '') AS author,
         (p.eeid = ANY($4::int[])) AS is_important,
         (p.eeid = ANY($6::int[])) AS is_always_alert,
         -- The subscriber authored this message. Duplicate identities merged into
         -- them (a GitHub/Jira row carrying no slack_user_id) count too, else the
         -- 60-odd messages attributed to those rows leave the thread unsuppressed.
         ($5 <> '' AND (p.slack_user_id = $5
                        OR p.merged_into IN (SELECT id FROM graph.people WHERE slack_user_id = $5))) AS is_subscriber,
         COALESCE(p.is_bot, false) AS is_bot
  FROM graph.nodes n
  LEFT JOIN graph.people p ON p.id = n.author_person_id
  -- Attachment text (Gemini description + OCR) referenced by this message, so a
  -- screenshot-only bug report is judgeable on its content. describe_attachment
  -- writes that into artifact_bodies.body_full (= description + "\n\nOCR:\n" +
  -- ocr); the description/ocr_text columns are populated only on rows synced
  -- from another machine, so read all three. Capped at 500 chars per attachment
  -- so a full-page form OCR can't dominate the 6000-char aggregate budget
  -- below. Scoped to attachment node types so referenced URLs/tickets don't leak
  -- their fetched bodies into the judge blob. The LATERAL aggregates → exactly
  -- one row, so it never fans out (msg_count/participants unaffected).
  LEFT JOIN LATERAL (
    SELECT string_agg(
             left(trim(
               COALESCE(NULLIF(ab.description,''),'') || ' ' ||
               COALESCE(NULLIF(ab.ocr_text,''),'')    || ' ' ||
               COALESCE(ab.body_full,'')
             ), 500), ' ') AS att
    FROM graph.edges e
    JOIN graph.artifact_bodies ab ON ab.node_id = e.to_node_id
    WHERE e.from_node_id = n.id AND e.kind = 'REFERENCES'
      AND (e.to_node_id LIKE 'slack_file:%' OR e.to_node_id LIKE 'jira_attachment:%')
  ) a ON true
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
         count(DISTINCT author_person_id) FILTER (WHERE NOT is_bot) AS participants,
         min(depth)                                        AS top_depth,
         (array_agg(author ORDER BY depth ASC, ts ASC))[1] AS top_author,
         (array_agg(text   ORDER BY ts ASC))[1]            AS first_text,
         max(ts)                                           AS last_ts,
         string_agg(left(text, 2000) || COALESCE(' ' || NULLIF(att,''), ''), ' ') AS blob,
         bool_or(COALESCE(is_important,false))             AS has_important,
         bool_or(COALESCE(is_always_alert,false))          AS has_always_alert,
         bool_or(COALESCE(is_subscriber,false))            AS has_subscriber,
         (array_agg(author) FILTER (WHERE is_important))[1] AS important_author
  FROM recent
  GROUP BY 1, 2
)
SELECT g.root_node_id, g.channel, COALESCE(c.name,''),
       g.msg_count, g.participants, g.top_depth,
       COALESCE(g.top_author,''), COALESCE(g.first_text,''), g.last_ts,
       LEFT(COALESCE(g.blob,''), 6000),
       COALESCE(g.has_important,false), COALESCE(g.important_author,''),
       COALESCE(g.has_always_alert,false)
FROM grp g
LEFT JOIN graph.slack_channels c ON c.slack_channel_id = g.channel
WHERE (g.participants >= $3 OR g.has_important)
  AND NOT COALESCE(g.has_subscriber, false)
ORDER BY g.last_ts DESC
LIMIT 50`
	filter := s.ChannelFilter
	if filter == nil {
		filter = []string{}
	}
	if important == nil {
		important = []int32{}
	}
	if alwaysAlert == nil {
		alwaysAlert = []int32{}
	}
	rows, err := db.Query(ctx, q, detectLookback, filter, s.MinParticipants, important, subscriber, alwaysAlert)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []hotThread
	for rows.Next() {
		var h hotThread
		if err := rows.Scan(&h.RootNodeID, &h.Channel, &h.ChannelName,
			&h.MsgCount, &h.Participants, &h.TopDepth, &h.TopAuthor, &h.FirstLine, &h.LastTS, &h.Blob,
			&h.HasImportant, &h.ImportantAuthor, &h.HasAlwaysAlert); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// subscriberEEID resolves an active Slack identity to its org owner.
func subscriberEEID(ctx context.Context, db *pgxpool.Pool, subscriberSlack string) int32 {
	if subscriberSlack == "" {
		return 0
	}
	var owner int32
	if err := db.QueryRow(ctx,
		`SELECT COALESCE(eeid,0) FROM graph.people WHERE slack_user_id=$1 AND merged_into IS NULL`,
		subscriberSlack).Scan(&owner); err != nil {
		return 0
	}
	return owner
}

// ownerImportantEeids returns the eeids the owner treats as "important":
// their reporting line (ancestors — where the LCA of (owner, other) IS the
// other, i.e. their manager up to the CEO) plus anyone within ~2 org hops. Empty
// when the subscriber isn't org-anchored (no eeid) or has no distances yet.
func ownerImportantEeids(ctx context.Context, db *pgxpool.Pool, owner int32) []int32 {
	if owner == 0 {
		return nil
	}
	rows, err := db.Query(ctx, `
SELECT DISTINCT CASE WHEN a_eeid=$1 THEN b_eeid ELSE a_eeid END AS other
FROM graph.person_distance
WHERE $1 IN (a_eeid, b_eeid)
  AND ( hops <= 2
        OR lca_eeid = CASE WHEN a_eeid=$1 THEN b_eeid ELSE a_eeid END )`, owner)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int32
	for rows.Next() {
		var e int32
		if rows.Scan(&e) == nil {
			out = append(out, e)
		}
	}
	// Merge config pins (people the org tree can't see — e.g. a business owner or a
	// daily collaborator on a far branch). These count as important on their own.
	out = append(out, overrideImportantEeids(ctx, db, owner)...)
	return out
}

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

// topicMatches decides whether a hot thread is genuinely about the
// subscription's topic, using an LLM yes/no judgment (cosine on a bare topic
// word can't separate, e.g., a deployment thread from a payments incident — both
// scored ~0.52). Falls back to a literal keyword match when no LLM is available.
// fromLLM is true when the verdict came from the LLM judge — only those are
// worth caching; a keyword-fallback verdict leaves the judge another chance.
func topicMatches(ctx context.Context, deps Deps, s subscription, h hotThread) (relevant, fromLLM bool) {
	if deps.Gemini != nil {
		if relevant, ok := judgeTopic(ctx, deps, s.judgeTopicText(), h); ok {
			return relevant, true
		}
	}
	// Fallback: literal keyword over the thread text + channel name.
	hay := strings.ToLower(h.Blob + " " + h.ChannelName)
	return strings.Contains(hay, strings.ToLower(s.Topic)), false
}

// judgment is a cached LLM verdict for one (subscription, thread) pair, valid
// while the thread still has msgCount messages in the lookback window.
type judgment struct {
	msgCount int
	relevant bool
}

// loadJudgments returns the cached judge verdicts for a subscription.
func loadJudgments(ctx context.Context, db *pgxpool.Pool, subID int64) map[string]judgment {
	out := map[string]judgment{}
	rows, err := db.Query(ctx,
		`SELECT root_node_id, msg_count, relevant FROM graph.topic_judgments WHERE subscription_id=$1`, subID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var j judgment
		if rows.Scan(&id, &j.msgCount, &j.relevant) == nil {
			out[id] = j
		}
	}
	return out
}

// saveJudgment upserts a verdict so the thread isn't re-judged until it grows.
func saveJudgment(ctx context.Context, db *pgxpool.Pool, subID int64, root string, msgCount int, relevant bool) {
	_, _ = db.Exec(ctx, `
		INSERT INTO graph.topic_judgments(subscription_id, root_node_id, msg_count, relevant)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (subscription_id, root_node_id)
		DO UPDATE SET msg_count=EXCLUDED.msg_count, relevant=EXCLUDED.relevant, judged_at=NOW()`,
		subID, root, msgCount, relevant)
}

// judgeTopic asks the LLM whether the thread is substantively about the topic.
// Returns (relevant, ok); ok=false on LLM/parse error so the caller can fall back.
func judgeTopic(ctx context.Context, deps Deps, topic string, h hotThread) (bool, bool) {
	const sys = `You decide whether a Slack thread is substantively ABOUT a given topic.
Respond with JSON only: {"relevant": true|false}.
Be strict: the thread must be genuinely about the topic. If it is mainly about a
different subject (deployments, infra, secrets, CI, unrelated ops) and only
mentions a related word in passing, answer false. Judge the actual subject of
the discussion, not isolated keywords.`
	user := "TOPIC: " + topic + "\n\nTHREAD:\n" + h.Blob
	out, err := deps.Gemini.Generate(ctx, sys, user)
	if err != nil || out == "" {
		return false, false
	}
	var parsed struct {
		Relevant bool `json:"relevant"`
	}
	if json.Unmarshal(llmjson.ExtractJSON(out), &parsed) != nil {
		return false, false
	}
	deps.Logger.Info().Str("node", h.RootNodeID).Str("topic", topic).
		Int("participants", h.Participants).Bool("relevant", parsed.Relevant).
		Msg("detect_hot_topics: topic relevance")
	return parsed.Relevant, true
}

// alertMsg is one transcript line (who said what), with the author's org-depth
// so the senior speaker can be named in plain language.
type alertMsg struct {
	author string
	text   string
	dept   string
	title  string
	domain string
	role   string
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
SELECT COALESCE(NULLIF(CASE WHEN p.display_name ~ '^[BU][A-Z0-9]{6,}$' THEN '' ELSE p.display_name END,''), NULLIF(n.metadata->'author'->>'display_name',''), ''),
       COALESCE(NULLIF(n.body,''), n.title, ''),
       COALESCE(p.department,''),
       COALESCE(p.job_title,''),
       COALESCE(dr.domain,''),
       COALESCE(dr.role_label,''),
       COALESCE(p.depth_from_root, 99)
FROM graph.nodes n
LEFT JOIN graph.people p ON p.id = n.author_person_id
LEFT JOIN graph.person_derived_roles dr ON dr.eeid = p.eeid
WHERE n.scope = 'slack:' || $1 AND n.deleted_at IS NULL
  AND ( n.id = $2 OR COALESCE(n.metadata->>'thread_ts','') = $3 )
ORDER BY COALESCE(to_timestamp(NULLIF(n.metadata->>'ts','')::float8), n.first_seen_at) ASC`,
		h.Channel, h.RootNodeID, threadTS); err == nil {
		for rows.Next() {
			var m alertMsg
			if rows.Scan(&m.author, &m.text, &m.dept, &m.title, &m.domain, &m.role, &m.depth) == nil {
				msgs = append(msgs, m)
			}
		}
		rows.Close()
	}

	// Attachments referenced by the thread (screenshots/docs whose OCR/description
	// fed the judge). Named in one line so the reader knows the alert leaned on
	// image content. One extra query — msgs above don't carry their attachments.
	var attachments []string
	if rows, err := deps.DB.Query(ctx, `
SELECT DISTINCT COALESCE(NULLIF(f.title,''), NULLIF(f.body,''), f.natural_key)
FROM graph.nodes m
JOIN graph.edges e ON e.from_node_id = m.id AND e.kind = 'REFERENCES'
  AND (e.to_node_id LIKE 'slack_file:%' OR e.to_node_id LIKE 'jira_attachment:%')
JOIN graph.nodes f ON f.id = e.to_node_id
JOIN graph.artifact_bodies ab ON ab.node_id = f.id
WHERE m.scope = 'slack:' || $1 AND m.deleted_at IS NULL
  AND ( m.id = $2 OR COALESCE(m.metadata->>'thread_ts','') = $3 )
ORDER BY 1 LIMIT 5`, h.Channel, h.RootNodeID, threadTS); err == nil {
		for rows.Next() {
			var name string
			if rows.Scan(&name) == nil && strings.TrimSpace(name) != "" {
				attachments = append(attachments, firstLine(name, 80))
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
		hasDiscussion := false
		for _, m := range msgs {
			if !linkOnly(m.text) {
				hasDiscussion = true
				break
			}
		}
		var tb strings.Builder
		// Same linked-resources block the cached summarizer uses — without it a
		// bare shared link summarizes to "no context provided".
		tb.WriteString(linkedResourceBlock(ctx, deps, h.Channel, threadTS, !hasDiscussion))
		for _, m := range msgs {
			a := m.author
			if a == "" {
				a = "someone"
			}
			tb.WriteString(withDept(a, m.dept, m.title, m.domain, m.role) + ": " + flattenLines(m.text, 400) + "\n")
		}
		_, overview, highlights, _ = genThreadDeepSummary(ctx, deps.Gemini, tb.String())
	}

	// Resolve Slack mention codes (<@U…>, <#C…>, <url|text>) to readable names so
	// the DM shows "@Lei Zheng", not a raw user id.
	allTexts := append([]string{overview}, highlights...)
	for _, m := range msgs {
		allTexts = append(allTexts, m.text)
	}
	names := loadSlackNames(ctx, deps.DB, allTexts...)
	overview = humanizeSlack(overview, names)
	for i := range highlights {
		highlights[i] = humanizeSlack(highlights[i], names)
	}
	for i := range msgs {
		msgs[i].text = humanizeSlack(msgs[i].text, names)
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
	for _, att := range attachments {
		fmt.Fprintf(&b, "📎 _%s_\n", att)
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
			line := fmt.Sprintf("• *%s:* %s\n", withDept(a, m.dept, m.title, m.domain, m.role), t)
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
	if base := strings.TrimSuffix(deps.PublicBaseURL, "/"); base != "" {
		fmt.Fprintf(&b, "\n📊 <%s/live/graph?node=%s|Open graph view>", base, url.QueryEscape(h.RootNodeID))
	}
	return b.String()
}

// whyFlagged explains in plain language why the thread was surfaced. It always
// matched the topic; here we say what crossed the bar — an important person
// (your org circle) and/or a real discussion.
func whyFlagged(h hotThread) string {
	if h.HasImportant {
		who := h.ImportantAuthor
		if who == "" {
			who = "someone important to you"
		} else {
			who += " (important to you)"
		}
		if h.Participants >= 2 {
			return fmt.Sprintf("%s is involved and %d people are discussing it", who, h.Participants)
		}
		return fmt.Sprintf("%s raised it", who)
	}
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

// slackNodeIDFromURL is the inverse of slackPermalink: it turns a Slack archive
// permalink into a canonical node id slack:<chan>:<ts>, or "" when the URL is
// not a Slack archive link. It reads only the URL *path*, so the
// ?thread_ts=…&cid=… query and any #fragment that Slack's "Copy link" appends
// are irrelevant by construction. Path shape: /archives/<CHANNEL>/p<digits>;
// the ts is the digits with a "." inserted 6 from the end
// (1782118242921599 -> 1782118242.921599). Returns "" on any shape mismatch, a
// non-numeric p<...> segment, or fewer than 7 digits.
func slackNodeIDFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "archives" {
		return ""
	}
	channel, seg := parts[1], parts[2]
	if channel == "" || !strings.HasPrefix(seg, "p") {
		return ""
	}
	digits := seg[1:]
	if len(digits) < 7 { // need >=1 digit before the "." inserted 6 from the end
		return ""
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return ""
		}
	}
	ts := digits[:len(digits)-6] + "." + digits[len(digits)-6:]
	return "slack:" + channel + ":" + ts
}

// stripQueryFragment returns rawURL without its ?query or #fragment, so a link
// carrying tracking params (?utm_source=…) still matches the bare url stored on
// the node. Returns the input unchanged when it isn't a URL or doesn't parse.
func stripQueryFragment(rawURL string) string {
	if !strings.Contains(rawURL, "://") {
		return rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// Slack stores mentions/links in an encoded form (<@U…>, <#C…|name>, <!here>,
// <url|text>). These regexes turn them back into readable text for the DM.
var (
	reSlackUser    = regexp.MustCompile(`<@(U[A-Z0-9]+)(?:\|[^>]*)?>`)
	reSlackChan    = regexp.MustCompile(`<#C[A-Z0-9]+\|([^>]*)>`)
	reSlackChanNo  = regexp.MustCompile(`<#C[A-Z0-9]+>`)
	reSlackSubteam = regexp.MustCompile(`<!subteam\^[A-Z0-9]+\|(@?[^>]+)>`)
	reSlackSpecial = regexp.MustCompile(`<!(here|channel|everyone)>`)
	reSlackLinkT   = regexp.MustCompile(`<(https?://[^>|]+)\|([^>]+)>`)
	reSlackLink    = regexp.MustCompile(`<(https?://[^>]+)>`)
)

// humanizeSlack replaces Slack mention/link codes with readable text. User ids
// resolve to "@<name>" via the names map (falling back to "@<id>" if unknown).
func humanizeSlack(text string, names map[string]string) string {
	if text == "" {
		return text
	}
	text = reSlackUser.ReplaceAllStringFunc(text, func(m string) string {
		id := reSlackUser.FindStringSubmatch(m)[1]
		if n := names[id]; n != "" {
			return "@" + n
		}
		return "@" + id
	})
	text = reSlackChan.ReplaceAllString(text, "#$1")
	text = reSlackChanNo.ReplaceAllString(text, "#channel")
	text = reSlackSubteam.ReplaceAllString(text, "$1")
	text = reSlackSpecial.ReplaceAllString(text, "@$1")
	text = reSlackLinkT.ReplaceAllString(text, "$2")
	text = reSlackLink.ReplaceAllString(text, "$1")
	return text
}

// loadSlackNames returns slack_user_id → name for every <@U…> mention found in
// the given texts (one query).
func loadSlackNames(ctx context.Context, db *pgxpool.Pool, texts ...string) map[string]string {
	idset := map[string]bool{}
	for _, t := range texts {
		for _, m := range reSlackUser.FindAllStringSubmatch(t, -1) {
			idset[m[1]] = true
		}
	}
	out := map[string]string{}
	if len(idset) == 0 {
		return out
	}
	ids := make([]string, 0, len(idset))
	for id := range idset {
		ids = append(ids, id)
	}
	rows, err := db.Query(ctx,
		`SELECT slack_user_id, COALESCE(NULLIF(real_name,''), display_name)
		 FROM graph.slack_users WHERE slack_user_id = ANY($1)`, ids)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if rows.Scan(&id, &name) == nil {
			out[id] = name
		}
	}
	return out
}

// ── Subscription CRUD over HTTP ──────────────────────────────────────────────

// Subscriptions serves the topic-subscription REST endpoints.
type Subscriptions struct {
	db          *pgxpool.Pool
	defaultUser string
	log         zerolog.Logger
	runner      string
	machineID   string
}

// NewSubscriptions builds the subscription HTTP handler.
func NewSubscriptions(deps Deps) *Subscriptions {
	return &Subscriptions{
		db: deps.DB, defaultUser: deps.SlackDMUserID, log: deps.Logger,
		runner: deps.Runner, machineID: deps.MachineID,
	}
}

// listSubscriptions loads subscriptions; activeOnly restricts to active rows.
func listSubscriptions(ctx context.Context, db *pgxpool.Pool, activeOnly bool) ([]subscription, error) {
	q := `SELECT id, subscriber_slack_id, topic, channel_filter,
	             min_participants, max_author_depth, active, created_at,
	             COALESCE(sources,'[]'::jsonb), COALESCE(scope_definition,''),
	             COALESCE(scope_summary,''), COALESCE(scope_status,''),
	             COALESCE(scope_error,''), scope_refreshed_at
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
		var srcRaw []byte
		if err := rows.Scan(&s.ID, &s.SubscriberSlack, &s.Topic, &s.ChannelFilter,
			&s.MinParticipants, &s.MaxAuthorDepth, &s.Active, &s.CreatedAt,
			&srcRaw, &s.ScopeDefinition, &s.ScopeSummary, &s.ScopeStatus,
			&s.ScopeError, &s.ScopeRefreshedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(srcRaw, &s.Sources)
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
		Topic           string        `json:"topic"`
		SubscriberSlack string        `json:"subscriber_slack_id"`
		ChannelFilter   []string      `json:"channel_filter"`
		MinParticipants *int          `json:"min_participants"`
		MaxAuthorDepth  *int          `json:"max_author_depth"`
		Sources         []topicSource `json:"sources"`
		ScopeDefinition *string       `json:"scope_definition"`
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
	// A lone-message subscription (min_participants < 2) must be channel-scoped:
	// min_participants=1 makes the volume gate true for every thread, collapsing
	// detection onto a per-thread LLM judge call across the whole workspace.
	if minP < 2 && len(filter) == 0 {
		http.Error(w, "min_participants < 2 requires a non-empty channel_filter (an unscoped lone-message subscription would run the LLM judge on every thread in the workspace)", http.StatusBadRequest)
		return
	}
	sources := req.Sources
	if sources == nil {
		sources = []topicSource{}
	}
	srcJSON, _ := json.Marshal(sources)
	scopeDef := ""
	if req.ScopeDefinition != nil {
		scopeDef = strings.TrimSpace(*req.ScopeDefinition)
	}
	var s subscription
	err := h.db.QueryRow(r.Context(), `
		INSERT INTO graph.topic_subscriptions
		  (subscriber_slack_id, topic, channel_filter, min_participants, max_author_depth,
		   sources, scope_definition, scope_refreshed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,CASE WHEN $7 <> '' THEN NOW() ELSE NULL END)
		RETURNING id, subscriber_slack_id, topic, channel_filter,
		          min_participants, max_author_depth, active, created_at, COALESCE(scope_status,'')`,
		subscriber, req.Topic, filter, minP, maxD, srcJSON, scopeDef).
		Scan(&s.ID, &s.SubscriberSlack, &s.Topic, &s.ChannelFilter,
			&s.MinParticipants, &s.MaxAuthorDepth, &s.Active, &s.CreatedAt, &s.ScopeStatus)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Sources = sources
	writeJSON(w, http.StatusOK, s)
}

// refresh enqueues a refresh_topic_scope job for the subscription (reads its
// sources, ingests them, distills the scope). The UI polls the subscription's
// scope_status / scope_summary for the result.
func (h *Subscriptions) refresh(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	tx, err := h.db.Begin(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()

	ct, err := tx.Exec(r.Context(),
		`UPDATE graph.topic_subscriptions SET scope_status='queued' WHERE id=$1`, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if ct.RowsAffected() == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if _, err := jobs.Enqueue(r.Context(), tx, "refresh_topic_scope",
		map[string]any{"subscription_id": id},
		jobs.EnqueueOptions{Priority: 3, TargetRunner: h.runner, MachineID: h.machineID}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

// update applies a partial change to a subscription: any of sources,
// min_participants, scope_definition, or active present in the body is updated;
// omitted fields are left untouched. A sources change is typically followed by
// refresh (POST …/refresh) to re-read + re-distill the scope.
func (h *Subscriptions) update(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	var req struct {
		Sources         *[]topicSource `json:"sources"`
		MinParticipants *int           `json:"min_participants"`
		ScopeDefinition *string        `json:"scope_definition"`
		Active          *bool          `json:"active"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	// Guard (mirrors create): lowering min_participants below 2 requires the sub
	// to be channel-scoped. channel_filter isn't editable here, so check the
	// stored value.
	if req.MinParticipants != nil && *req.MinParticipants < 2 {
		var filter []string
		switch err := h.db.QueryRow(r.Context(),
			`SELECT channel_filter FROM graph.topic_subscriptions WHERE id=$1`, id).Scan(&filter); {
		case errors.Is(err, pgx.ErrNoRows):
			http.Error(w, "not found", http.StatusNotFound)
			return
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if len(filter) == 0 {
			http.Error(w, "min_participants < 2 requires a non-empty channel_filter (an unscoped lone-message subscription would run the LLM judge on every thread in the workspace)", http.StatusBadRequest)
			return
		}
	}

	// Partial update: build SET from only the fields present in the body.
	sets := make([]string, 0, 4)
	args := []any{id}
	if req.Sources != nil {
		srcJSON, _ := json.Marshal(*req.Sources)
		args = append(args, srcJSON)
		sets = append(sets, fmt.Sprintf("sources=$%d", len(args)))
	}
	if req.MinParticipants != nil {
		args = append(args, *req.MinParticipants)
		sets = append(sets, fmt.Sprintf("min_participants=$%d", len(args)))
	}
	if req.ScopeDefinition != nil {
		args = append(args, *req.ScopeDefinition)
		placeholder := fmt.Sprintf("$%d", len(args))
		sets = append(sets,
			"scope_refreshed_at=CASE WHEN scope_definition IS DISTINCT FROM "+placeholder+" THEN NOW() ELSE scope_refreshed_at END",
			"scope_definition="+placeholder)
	}
	if req.Active != nil {
		args = append(args, *req.Active)
		sets = append(sets, fmt.Sprintf("active=$%d", len(args)))
	}
	if len(sets) == 0 {
		w.WriteHeader(http.StatusNoContent) // nothing to change
		return
	}
	ct, err := h.db.Exec(r.Context(),
		"UPDATE graph.topic_subscriptions SET "+strings.Join(sets, ", ")+" WHERE id=$1", args...)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if ct.RowsAffected() == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
