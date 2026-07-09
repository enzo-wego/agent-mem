package handlers

import (
	"context"
	"regexp"
	"strings"
	"time"
)

const alertFingerprintSpikeThreshold = 20

var (
	reAlertTimestamp = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[tT ][0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.\d+)?(?:[zZ]|[+-][0-9]{2}:?[0-9]{2})?\b`)
	reAlertURL       = regexp.MustCompile(`https?://\S+`)
	reAlertNumber    = regexp.MustCompile(`\b\d+(?:\.\d+)?\b`)
)

type alertBotDecision struct {
	Skip     bool
	Escalate bool
}

func isAlertChannelName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	return n == "alerts" ||
		strings.HasPrefix(n, "alerts-") ||
		strings.HasPrefix(n, "alerts_") ||
		strings.HasSuffix(n, "-alert") ||
		strings.HasSuffix(n, "_alert") ||
		strings.HasSuffix(n, "-alerts") ||
		strings.HasSuffix(n, "_alerts")
}

func alertFingerprint(text string) string {
	s := strings.ToLower(text)
	s = reAlertURL.ReplaceAllString(s, "<url>")
	s = reAlertTimestamp.ReplaceAllString(s, "<time>")
	s = reAlertNumber.ReplaceAllString(s, "<num>")
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimSpace(s)
}

// channelIsAlert reports whether channelID is an alert channel (by its name).
func channelIsAlert(ctx context.Context, deps Deps, channelID string) bool {
	if deps.DB == nil || channelID == "" {
		return false
	}
	var name string
	_ = deps.DB.QueryRow(ctx,
		`SELECT COALESCE(name,'') FROM graph.slack_channels WHERE slack_channel_id=$1`,
		channelID,
	).Scan(&name)
	return isAlertChannelName(name)
}

func decideAlertBot(ctx context.Context, deps Deps, channelID, text string, automated bool) alertBotDecision {
	if !automated || !channelIsAlert(ctx, deps, channelID) {
		return alertBotDecision{}
	}
	fp := alertFingerprint(text)
	if fp == "" {
		return alertBotDecision{Skip: true}
	}
	escalate := recordAlertFingerprint(ctx, deps, channelID, fp, text)
	return alertBotDecision{Skip: !escalate, Escalate: escalate}
}

func recordAlertFingerprint(ctx context.Context, deps Deps, channelID, fingerprint, text string) bool {
	var countTotal int
	var lastEscalatedAt *time.Time
	err := deps.DB.QueryRow(ctx,
		`SELECT count_total, last_escalated_at
FROM graph.alert_fingerprints
WHERE channel_id=$1 AND fingerprint=$2`,
		channelID, fingerprint,
	).Scan(&countTotal, &lastEscalatedAt)

	_, _ = deps.DB.Exec(ctx,
		`INSERT INTO graph.alert_fingerprint_events(channel_id, fingerprint, seen_at, machine_id)
VALUES ($1,$2,NOW(),$3)`,
		channelID, fingerprint, deps.MachineID,
	)

	if err != nil {
		_, _ = deps.DB.Exec(ctx,
			`INSERT INTO graph.alert_fingerprints
  (channel_id, fingerprint, representative_text, count_total, first_seen_at, last_seen_at, last_escalated_at, machine_id)
VALUES ($1,$2,$3,1,NOW(),NOW(),NOW(),$4)
ON CONFLICT (channel_id, fingerprint) DO UPDATE SET
  count_total=graph.alert_fingerprints.count_total+1,
  last_seen_at=NOW()`,
			channelID, fingerprint, truncateRunes(strings.TrimSpace(text), 500), deps.MachineID,
		)
		return true // novel alert template
	}

	_, _ = deps.DB.Exec(ctx,
		`UPDATE graph.alert_fingerprints
SET count_total=count_total+1, last_seen_at=NOW()
WHERE channel_id=$1 AND fingerprint=$2`,
		channelID, fingerprint,
	)
	var countRecent int
	_ = deps.DB.QueryRow(ctx,
		`SELECT count(*) FROM graph.alert_fingerprint_events
WHERE channel_id=$1 AND fingerprint=$2 AND seen_at >= NOW() - INTERVAL '1 hour'`,
		channelID, fingerprint,
	).Scan(&countRecent)
	if countRecent >= alertFingerprintSpikeThreshold &&
		(lastEscalatedAt == nil || time.Since(*lastEscalatedAt) >= time.Hour) {
		_, _ = deps.DB.Exec(ctx,
			`UPDATE graph.alert_fingerprints
SET last_escalated_at=NOW()
WHERE channel_id=$1 AND fingerprint=$2`,
			channelID, fingerprint,
		)
		return true
	}
	return false
}

func slackMessageAutomated(msg slackMessage) bool {
	return msg.BotID != "" || msg.Subtype == "bot_message"
}

func shouldSkipSlackMessageForAlertPolicy(msg slackMessage, decision alertBotDecision, forceAlertThread bool) bool {
	automated := slackMessageAutomated(msg)
	forcedBotRoot := forceAlertThread && automated
	if decision.Skip && !forcedBotRoot {
		return true
	}
	if msg.Subtype != "" && !decision.Escalate && !forcedBotRoot {
		return true
	}
	if msg.BotID != "" && msg.User == "" && !decision.Escalate && !forcedBotRoot {
		return true
	}
	return false
}

func forceAlertThreadBackfill(msg slackMessage, decision alertBotDecision) bool {
	return decision.Skip && msg.ReplyCount > 0 && slackMessageAutomated(msg)
}

// alertThreadHasNonBotReply reports whether a skipped alert thread's repliers
// include anyone who isn't a known bot — the signal that a human is engaging and
// the thread deserves full treatment. It errs toward "yes": unknown repliers
// (not yet resolved in graph.people) and DB errors both count as human, so a real
// incident is never dropped just because a replier isn't matched yet. Only a
// thread whose every replier is a known bot returns false — the bot talking to
// itself, which stays noise. Gate it behind forceAlertThreadBackfill so the query
// runs only for skipped bot roots that actually have replies.
func alertThreadHasNonBotReply(ctx context.Context, deps Deps, replyUsers []string) bool {
	if deps.DB == nil {
		return false
	}
	present := make([]string, 0, len(replyUsers))
	for _, u := range replyUsers {
		if strings.TrimSpace(u) != "" {
			present = append(present, u)
		}
	}
	if len(present) == 0 {
		return false
	}
	var hasNonBot bool
	err := deps.DB.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM unnest($1::text[]) AS ru
  WHERE ru NOT IN (
    SELECT slack_user_id FROM graph.people
    WHERE is_bot = true AND slack_user_id IS NOT NULL
  )
)`, present).Scan(&hasNonBot)
	if err != nil {
		return true // err toward caring: don't drop a possible incident
	}
	return hasNonBot
}
