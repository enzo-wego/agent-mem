package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// monitor_hourly_report is a temporary (7-day) self-rescheduling job that DMs an
// hourly health report to the subscriber, all in ONE Slack thread. Its purpose is
// to confirm the per-channel ignore/incident filters (graph.channel_filters) are
// working: it compares Slack messages processed this hour against the same clock
// hour over the previous 7 days, and separately checks that the ignored channels
// are now producing ~zero nodes. Cost is reported as-is (no comparison).
//
// State lives in settings: monitor.activated_at (window start; the job stops
// rescheduling 7 days after it) and monitor.thread_ts (root DM message, so every
// hour replies in the same thread). No history table — the baseline is queried
// live from graph.nodes.
const (
	monitorInterval     = time.Hour
	monitorWindow       = 7 * 24 * time.Hour
	monitorActivatedKey = "monitor.activated_at"
	monitorThreadKey    = "monitor.thread_ts"
	monitorDoneKey      = "monitor.done"
	openRouterKeyURL    = "https://openrouter.ai/api/v1/key"
)

// monitorTZ is the display timezone for report labels (subscriber is UTC+7).
var monitorTZ = time.FixedZone("+07", 7*3600)

var monitorHTTP = &http.Client{Timeout: 15 * time.Second}

// NewMonitorHourlyReport returns the self-rescheduling hourly-monitor handler.
func NewMonitorHourlyReport(deps Deps) jobs.Handler {
	log := deps.Logger
	return func(ctx context.Context, _ []byte) error {
		now := time.Now().UTC()
		activated := loadOrSetActivated(ctx, deps.DB, now)
		expired := now.After(activated.Add(monitorWindow))

		// Reschedule for the top of the next hour unless the 7-day window is over.
		if !expired {
			defer func() {
				next := now.Truncate(time.Hour).Add(monitorInterval)
				if _, err := jobs.Enqueue(ctx, deps.DB, "monitor_hourly_report", map[string]any{},
					jobs.EnqueueOptions{AvailableAt: next, TargetRunner: deps.Runner, MachineID: deps.MachineID}); err != nil {
					log.Warn().Err(err).Msg("monitor_hourly_report: reschedule failed")
				}
			}()
		}

		to := deps.SlackDMUserID
		if to == "" || deps.SlackBotToken == "" {
			return nil // nothing to notify to
		}
		channelID, err := slackOpenIM(ctx, deps.SlackBotToken, to)
		if err != nil {
			log.Warn().Err(err).Msg("monitor_hourly_report: open DM failed")
			return nil
		}

		// Ensure the thread root exists (first run posts it and stores its ts).
		threadTS := loadSetting(ctx, deps.DB, monitorThreadKey)
		if threadTS == "" {
			ts, perr := slackPostMessageTS(ctx, deps.SlackBotToken, channelID, monitorRootMessage(now), "")
			if perr != nil {
				log.Warn().Err(perr).Msg("monitor_hourly_report: post root failed")
				return nil
			}
			threadTS = ts
			saveSetting(ctx, deps.DB, monitorThreadKey, ts)
		}

		if expired {
			// Post the completion notice once; a restart after expiry re-enqueues
			// this job, and without the flag it would repeat the message each time.
			if loadSetting(ctx, deps.DB, monitorDoneKey) == "" {
				_, _ = slackPostMessageTS(ctx, deps.SlackBotToken, channelID,
					"✅ 7-day monitor complete — this thread will stop updating.", threadTS)
				saveSetting(ctx, deps.DB, monitorDoneKey, now.Format(time.RFC3339))
			}
			return nil
		}

		report := buildMonitorReport(ctx, deps, now)
		if _, err := slackPostMessageTS(ctx, deps.SlackBotToken, channelID, report, threadTS); err != nil {
			log.Warn().Err(err).Msg("monitor_hourly_report: post report failed")
		}
		return nil
	}
}

func monitorRootMessage(now time.Time) string {
	return fmt.Sprintf("📊 *agent-mem hourly monitor* — started %s, running ~7 days.\n"+
		"Each hour I'll reply here: Slack messages processed vs the same hour over the prior 7 days "+
		"(confirms the ignore/incident filters), plus current OpenRouter spend.",
		now.In(monitorTZ).Format("Mon Jan 2 15:04 -07"))
}

// buildMonitorReport composes one hour's report for the just-completed clock hour.
func buildMonitorReport(ctx context.Context, deps Deps, now time.Time) string {
	hEnd := now.Truncate(time.Hour)
	hStart := hEnd.Add(-time.Hour)

	processed := slackNodesInWindow(ctx, deps.DB, hStart, hEnd, nil)
	baseline := slackNodesBaseline(ctx, deps.DB, hStart, hEnd, nil)

	ignore := loadChannelFilters(ctx, deps.DB).ignoreList()
	ignoredNow := slackNodesInWindow(ctx, deps.DB, hStart, hEnd, ignore)
	ignoredBase := slackNodesBaseline(ctx, deps.DB, hStart, hEnd, ignore)

	top := topChannelsInWindow(ctx, deps.DB, hStart, hEnd, 5)

	var b strings.Builder
	fmt.Fprintf(&b, "🕐 *%s–%s*\n",
		hStart.In(monitorTZ).Format("Mon Jan 2 15:04"), hEnd.In(monitorTZ).Format("15:04 -07"))
	fmt.Fprintf(&b, "📥 Processed: *%d* msgs  (7d same-hr avg %.0f · %s)\n",
		processed, baseline, deltaLabel(float64(processed), baseline))
	if top != "" {
		fmt.Fprintf(&b, "   top: %s\n", top)
	}
	// Filter health: ignored channels should be ~0 now (baseline shows the saving).
	icon := "✅"
	if ignoredNow > 0 {
		icon = "⚠️"
	}
	fmt.Fprintf(&b, "🚫 Ignored channels: *%d* msgs %s  (7d same-hr avg %.0f)\n",
		ignoredNow, icon, ignoredBase)

	if daily, limit, remaining, ok := fetchOpenRouterSpend(ctx, deps.DB); ok {
		if limit > 0 {
			fmt.Fprintf(&b, "💸 OpenRouter: today $%.2f · $%.2f left", daily, remaining)
		} else {
			fmt.Fprintf(&b, "💸 OpenRouter: today $%.2f", daily)
		}
	} else {
		b.WriteString("💸 OpenRouter: usage unavailable")
	}
	return b.String()
}

// deltaLabel renders a percent change from baseline (guards divide-by-zero).
func deltaLabel(cur, base float64) string {
	if base <= 0 {
		if cur == 0 {
			return "flat"
		}
		return "no baseline"
	}
	pct := (cur - base) / base * 100
	sign := "+"
	if pct < 0 {
		sign = ""
	}
	return fmt.Sprintf("Δ %s%.0f%%", sign, pct)
}

// slackNodesInWindow counts slack messages created in [start,end). When scopes is
// non-empty the count is restricted to those channel scopes. deleted_at is
// ignored on purpose: this measures ingest throughput, so our own soft-deletes of
// filtered channels must not shrink the count.
func slackNodesInWindow(ctx context.Context, db *pgxpool.Pool, start, end time.Time, scopes []string) int {
	if db == nil {
		return 0
	}
	q := `SELECT count(*) FROM graph.nodes
	      WHERE type IN ('slack','slack_thread')
	        AND COALESCE(created_at, first_seen_at) >= $1
	        AND COALESCE(created_at, first_seen_at) <  $2`
	args := []any{start, end}
	if len(scopes) > 0 {
		q += ` AND scope = ANY($3)`
		args = append(args, scopeList(scopes))
	}
	var n int
	_ = db.QueryRow(ctx, q, args...).Scan(&n)
	return n
}

// slackNodesBaseline returns the mean count over the same clock hour on each of
// the previous 7 days.
func slackNodesBaseline(ctx context.Context, db *pgxpool.Pool, start, end time.Time, scopes []string) float64 {
	if db == nil {
		return 0
	}
	q := `SELECT COALESCE(AVG(cnt),0) FROM (
	        SELECT (SELECT count(*) FROM graph.nodes n
	                WHERE n.type IN ('slack','slack_thread')
	                  AND COALESCE(n.created_at, n.first_seen_at) >= $1::timestamptz - make_interval(days => d)
	                  AND COALESCE(n.created_at, n.first_seen_at) <  $2::timestamptz - make_interval(days => d)
	                  AND ($3::text[] IS NULL OR n.scope = ANY($3))) AS cnt
	        FROM generate_series(1,7) AS d
	      ) t`
	var scopeArg any
	if len(scopes) > 0 {
		scopeArg = scopeList(scopes)
	}
	var avg float64
	_ = db.QueryRow(ctx, q, start, end, scopeArg).Scan(&avg)
	return avg
}

// topChannelsInWindow returns a compact "name N · name N" line of the busiest
// channels processed in [start,end).
func topChannelsInWindow(ctx context.Context, db *pgxpool.Pool, start, end time.Time, limit int) string {
	if db == nil {
		return ""
	}
	q := `SELECT COALESCE(NULLIF(c.name,''), replace(n.scope,'slack:','')) AS ch, count(*) AS cnt
	      FROM graph.nodes n
	      LEFT JOIN graph.slack_channels c ON c.slack_channel_id = replace(n.scope,'slack:','')
	      WHERE n.type IN ('slack','slack_thread')
	        AND COALESCE(n.created_at, n.first_seen_at) >= $1
	        AND COALESCE(n.created_at, n.first_seen_at) <  $2
	      GROUP BY ch ORDER BY cnt DESC LIMIT $3`
	rows, err := db.Query(ctx, q, start, end, limit)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var ch string
		var cnt int
		if rows.Scan(&ch, &cnt) == nil {
			parts = append(parts, fmt.Sprintf("%s %d", ch, cnt))
		}
	}
	return strings.Join(parts, " · ")
}

// scopeList turns channel ids into the "slack:<id>" scopes stored on nodes.
func scopeList(channelIDs []string) []string {
	out := make([]string, 0, len(channelIDs))
	for _, id := range channelIDs {
		out = append(out, "slack:"+id)
	}
	return out
}

// ignoreList returns the configured ignore channel ids.
func (f *compiledChannelFilters) ignoreList() []string {
	out := make([]string, 0, len(f.ignore))
	for id := range f.ignore {
		out = append(out, id)
	}
	return out
}

// fetchOpenRouterSpend reads the OpenRouter key (from settings gemini_api_key) and
// returns today's spend, the limit, and remaining budget. ok=false on any failure.
func fetchOpenRouterSpend(ctx context.Context, db *pgxpool.Pool) (daily, limit, remaining float64, ok bool) {
	key := loadSetting(ctx, db, "gemini_api_key")
	if key == "" {
		return 0, 0, 0, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, openRouterKeyURL, nil)
	if err != nil {
		return 0, 0, 0, false
	}
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := monitorHTTP.Do(req)
	if err != nil {
		return 0, 0, 0, false
	}
	defer res.Body.Close()
	var env struct {
		Data struct {
			Limit          *float64 `json:"limit"`
			LimitRemaining *float64 `json:"limit_remaining"`
			UsageDaily     *float64 `json:"usage_daily"`
		} `json:"data"`
	}
	if json.NewDecoder(res.Body).Decode(&env) != nil {
		return 0, 0, 0, false
	}
	d := env.Data
	daily = derefF(d.UsageDaily)
	limit = derefF(d.Limit)
	remaining = derefF(d.LimitRemaining)
	return daily, limit, remaining, true
}

func derefF(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// loadOrSetActivated returns the monitor window start, initializing it to now on
// the first run.
func loadOrSetActivated(ctx context.Context, db *pgxpool.Pool, now time.Time) time.Time {
	var activated time.Time
	err := db.QueryRow(ctx, `SELECT value::timestamptz FROM settings WHERE key=$1`, monitorActivatedKey).Scan(&activated)
	if err == nil {
		return activated
	}
	_, _ = db.Exec(ctx, `INSERT INTO settings(key,value) VALUES($1,$2::text) ON CONFLICT(key) DO NOTHING`,
		monitorActivatedKey, now.Format(time.RFC3339))
	return now
}

// loadSetting reads a settings value ("" if absent).
func loadSetting(ctx context.Context, db *pgxpool.Pool, key string) string {
	var v string
	_ = db.QueryRow(ctx, `SELECT value FROM settings WHERE key=$1`, key).Scan(&v)
	return v
}

// saveSetting upserts a settings value.
func saveSetting(ctx context.Context, db *pgxpool.Pool, key, value string) {
	_, _ = db.Exec(ctx, `INSERT INTO settings(key,value) VALUES($1,$2)
	                     ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`, key, value)
}
