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

func decideAlertBot(ctx context.Context, deps Deps, channelID, text string, automated bool) alertBotDecision {
	if deps.DB == nil || !automated || channelID == "" {
		return alertBotDecision{}
	}
	var name string
	_ = deps.DB.QueryRow(ctx,
		`SELECT COALESCE(name,'') FROM graph.slack_channels WHERE slack_channel_id=$1`,
		channelID,
	).Scan(&name)
	if !isAlertChannelName(name) {
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
