package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// watchContinentID is the continent (channel group) whose every message gets
// DM'd. "partners" = the Payment Partners group on the globe — kept in sync via
// the shared graph_continents config, so adding a channel there auto-watches it.
const watchContinentID = "partners"

const (
	watchInterval = 5 * time.Minute
	watchLookback = 6 * time.Hour // bounds the scan; dedup prevents re-notify
	// settings key holding the activation timestamp — messages before it are not
	// back-notified, so turning this on doesn't dump recent history as a burst.
	watchActivatedKey = "watch_channels_activated_at"
)

// continentsConfig is the subset of graph_continents the notifier needs to
// classify a channel into a group (mirrors dashboard/src/continents.ts).
type continentsConfig struct {
	Continents []struct {
		ID    string   `json:"id"`
		Match []string `json:"match"`
	} `json:"continents"`
	Overrides map[string]string `json:"overrides"`
	Names     map[string]string `json:"names"`
	// Ignore lists channel ids muted from ALL notifications (hot-topic + watch).
	Ignore []string `json:"ignore"`
}

// ignoredChannelIDs returns the set of slack channel ids muted from every
// notification path, read from the graph_continents "ignore" list.
func ignoredChannelIDs(ctx context.Context, db *pgxpool.Pool) map[string]bool {
	cfg := loadContinentsConfig(ctx, db)
	out := make(map[string]bool, len(cfg.Ignore))
	for _, id := range cfg.Ignore {
		out[id] = true
	}
	return out
}

// continentOf classifies a channel into a continent id, faithfully mirroring the
// dashboard's continentOf: an explicit override wins; else the first continent
// whose match list contains "*" or a prefix of the (config or Slack) name.
func continentOf(channelID, slackName string, cfg continentsConfig) string {
	if o := cfg.Overrides[channelID]; o != "" {
		return o
	}
	name := cfg.Names[channelID]
	if name == "" {
		name = slackName
	}
	if name == "" {
		name = channelID
	}
	for _, c := range cfg.Continents {
		for _, m := range c.Match {
			if m == "*" || strings.HasPrefix(name, m) {
				return c.ID
			}
		}
	}
	return ""
}

// loadContinentsConfig reads the graph_continents settings blob (the same config
// the globe uses), falling back to the built-in default if unset/unparseable.
func loadContinentsConfig(ctx context.Context, db *pgxpool.Pool) continentsConfig {
	var raw string
	if err := db.QueryRow(ctx, `SELECT value FROM settings WHERE key='graph_continents'`).Scan(&raw); err != nil || raw == "" {
		raw = defaultContinents
	}
	var cfg continentsConfig
	if json.Unmarshal([]byte(raw), &cfg) != nil {
		_ = json.Unmarshal([]byte(defaultContinents), &cfg)
	}
	return cfg
}

// NewNotifyWatchChannels returns the self-rescheduling handler that DMs the
// subscriber for every new message in a watched channel group (Payment Partners).
// No topic/volume gate — these channels are important enough that every message
// matters. Dedup via graph.channel_notifications; activation watermark avoids a
// first-run history dump.
func NewNotifyWatchChannels(deps Deps) jobs.Handler {
	log := deps.Logger
	return func(ctx context.Context, _ []byte) error {
		defer func() {
			if _, err := jobs.Enqueue(ctx, deps.DB, "notify_watch_channels", map[string]any{},
				jobs.EnqueueOptions{
					AvailableAt:  time.Now().Add(watchInterval),
					TargetRunner: deps.Runner,
					MachineID:    deps.MachineID,
				}); err != nil {
				log.Warn().Err(err).Msg("notify_watch_channels: reschedule failed")
			}
		}()

		to := deps.SlackDMUserID
		if to == "" || deps.SlackBotToken == "" {
			return nil // nothing to notify to
		}

		// Which channels are in the watched group? Classify every known channel
		// against the shared config, so the set tracks the globe automatically.
		cfg := loadContinentsConfig(ctx, deps.DB)
		watched := watchedChannelIDs(ctx, deps.DB, cfg)
		if len(watched) == 0 {
			return nil
		}

		// Activation watermark: on first run, set it to now and notify nothing, so
		// enabling this doesn't replay recent history.
		var activated time.Time
		var hasActivated bool
		err := deps.DB.QueryRow(ctx,
			`SELECT value::timestamptz FROM settings WHERE key=$1`, watchActivatedKey).Scan(&activated)
		hasActivated = err == nil
		if !hasActivated {
			_, _ = deps.DB.Exec(ctx,
				`INSERT INTO settings(key,value) VALUES($1, now()::text)
				 ON CONFLICT(key) DO NOTHING`, watchActivatedKey)
			log.Info().Msg("notify_watch_channels: activated; future messages will be notified")
			return nil
		}

		msgs, err := newWatchedMessages(ctx, deps.DB, watched, activated)
		if err != nil {
			log.Warn().Err(err).Msg("notify_watch_channels: query failed")
			return nil
		}
		for _, m := range msgs {
			// Claim the message (dedup); skip if already notified.
			ct, err := deps.DB.Exec(ctx,
				`INSERT INTO graph.channel_notifications(node_id) VALUES($1) ON CONFLICT DO NOTHING`, m.nodeID)
			if err != nil || ct.RowsAffected() == 0 {
				continue
			}
			if err := slackDM(ctx, deps.SlackBotToken, to, buildChannelMsg(ctx, deps, m)); err != nil {
				log.Warn().Err(err).Str("node", m.nodeID).Msg("notify_watch_channels: DM failed")
				// Roll back the claim so a retry can re-send.
				_, _ = deps.DB.Exec(ctx, `DELETE FROM graph.channel_notifications WHERE node_id=$1`, m.nodeID)
				continue
			}
			log.Info().Str("node", m.nodeID).Str("channel", m.channelName).Msg("watch-channel alert sent")
		}
		return nil
	}
}

// watchedChannelIDs returns the slack channel ids classified into watchContinentID.
func watchedChannelIDs(ctx context.Context, db *pgxpool.Pool, cfg continentsConfig) []string {
	rows, err := db.Query(ctx, `SELECT slack_channel_id, COALESCE(name,'') FROM graph.slack_channels`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	ignored := make(map[string]bool, len(cfg.Ignore))
	for _, id := range cfg.Ignore {
		ignored[id] = true
	}
	var out []string
	for rows.Next() {
		var id, name string
		if rows.Scan(&id, &name) == nil && !ignored[id] && continentOf(id, name, cfg) == watchContinentID {
			out = append(out, id)
		}
	}
	return out
}

// watchedMsg is one message in a watched channel.
type watchedMsg struct {
	nodeID      string
	channel     string
	channelName string
	author      string
	dept        string
	title       string
	text        string
	rootNodeID  string
}

// newWatchedMessages returns messages in the watched channels created since the
// activation watermark (bounded by watchLookback) that haven't been notified yet.
func newWatchedMessages(ctx context.Context, db *pgxpool.Pool, channels []string, activated time.Time) ([]watchedMsg, error) {
	const q = `
SELECT n.id,
       replace(n.scope,'slack:','')                                 AS channel,
       COALESCE(c.name,'')                                          AS channel_name,
       COALESCE(NULLIF(CASE WHEN p.display_name ~ '^[BU][A-Z0-9]{6,}$' THEN '' ELSE p.display_name END,''), NULLIF(n.metadata->'author'->>'display_name',''), '') AS author,
       COALESCE(p.department,'')                                    AS dept,
       COALESCE(p.job_title,'')                                     AS job_title,
       COALESCE(NULLIF(n.title,''), n.body, '')                     AS text,
       CASE WHEN COALESCE(NULLIF(n.metadata->>'thread_ts',''),'') <> ''
            THEN 'slack:'||replace(n.scope,'slack:','')||':'||(n.metadata->>'thread_ts')
            ELSE n.id END                                           AS root_node_id
FROM graph.nodes n
LEFT JOIN graph.people p ON p.id = n.author_person_id
LEFT JOIN graph.slack_channels c ON c.slack_channel_id = replace(n.scope,'slack:','')
WHERE n.type IN ('slack','slack_thread')
  AND n.deleted_at IS NULL
  AND replace(n.scope,'slack:','') = ANY($1)
  AND COALESCE(n.created_at, n.first_seen_at) >= GREATEST($2::timestamptz, now() - make_interval(hours => $3))
  AND NOT EXISTS (SELECT 1 FROM graph.channel_notifications cn WHERE cn.node_id = n.id)
ORDER BY COALESCE(to_timestamp(NULLIF(n.metadata->>'ts','')::float8), n.first_seen_at) ASC
LIMIT 50`
	rows, err := db.Query(ctx, q, channels, activated, int(watchLookback.Hours()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []watchedMsg
	for rows.Next() {
		var m watchedMsg
		if err := rows.Scan(&m.nodeID, &m.channel, &m.channelName, &m.author, &m.dept, &m.title, &m.text, &m.rootNodeID); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// buildChannelMsg composes the DM for a single watched-channel message: channel,
// the thread topic (so a bare reply has context), author (with department), the
// text (Slack codes humanized), and a permalink.
func buildChannelMsg(ctx context.Context, deps Deps, m watchedMsg) string {
	topic := threadTopic(ctx, deps.DB, m.rootNodeID)
	names := loadSlackNames(ctx, deps.DB, m.text, topic)
	text := humanizeSlack(m.text, names)
	author := m.author
	if author == "" {
		author = "someone"
	}
	channel := m.channelName
	if channel == "" {
		channel = m.channel
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🤝 *Payment partner* · #%s\n", channel)
	if topic != "" {
		fmt.Fprintf(&b, "_Thread: %s_\n", humanizeSlack(topic, names))
	}
	fmt.Fprintf(&b, "*%s:* %s\n", withDept(author, m.dept, m.title), firstLine(text, 600))
	if link := slackPermalink(m.rootNodeID); link != "" {
		b.WriteString(link)
	}
	return b.String()
}

// splitSlackRoot parses a slack root node id "slack:<channel>:<thread_ts>" into
// its channel and thread ts. ok is false for any other shape (a non-thread
// message whose id isn't a 3-part slack root).
func splitSlackRoot(rootNodeID string) (channel, ts string, ok bool) {
	parts := strings.SplitN(rootNodeID, ":", 3)
	if len(parts) != 3 || parts[0] != "slack" || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// threadTopic returns the cached short topic label for the message's thread, or
// "" when the message isn't in a thread or no summary is cached yet.
// ponytail: cached-only by design — the watch loop runs every 5 min over many
// messages, so it must not block on an LLM call. summarize_thread (enqueued on
// ingest) fills the cache; a brand-new thread just shows no topic line until then.
func threadTopic(ctx context.Context, db *pgxpool.Pool, rootNodeID string) string {
	channel, ts, ok := splitSlackRoot(rootNodeID)
	if !ok {
		return ""
	}
	var topic string
	_ = db.QueryRow(ctx,
		`SELECT COALESCE(summary,'') FROM graph.thread_summaries WHERE channel_id=$1 AND thread_ts=$2`,
		channel, ts).Scan(&topic)
	return topic
}
