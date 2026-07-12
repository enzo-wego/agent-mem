package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// Identifier extraction (plan 15, C1). Concrete identifiers are pulled from
// RAW text — summaries drop them (verified: the ext-wego-juspay raise of
// pxx6xgkdtl summarized to "Juspay API limit on order age for refunds" with
// zero identifiers). Two artifacts sharing a rare identifier become topic-link
// candidates regardless of embedding distance.

const maxIdentifiersPerNode = 64

// Payment-family refs, format from wego/payments pkg/payment/entity/const.go:
// <prefix><9 chars> (payment p / source s / dispute d), action a<14 chars>.
// The body charset is refChars+refPaddingChars — digits plus letters excluding
// a, p, s — which alone rules out most English words; requiring at least one
// digit (checked in Go, RE2 has no lookahead) removes the rest ("prevention").
var (
	rePaymentRef = regexp.MustCompile(`\b[psd][0-9b-oqrt-z]{9}\b`)
	reActionRef  = regexp.MustCompile(`\ba[0-9b-oqrt-z]{14}\b`)
	reJiraKey    = regexp.MustCompile(`\b[A-Z][A-Z0-9]{1,9}-[0-9]{1,6}\b`)
	reGHPRURL    = regexp.MustCompile(`\bgithub\.com/(wego/[\w.-]+)/pull/([0-9]+)\b`)
	reGHPRShort  = regexp.MustCompile(`\b(wego/[\w.-]+)#([0-9]+)\b`)
	reRequestID  = regexp.MustCompile(`\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
)

// extractIdentifiers returns the deduplicated, sorted identifiers found in
// text. Frequency-based noise (an error code every thread quotes) is not
// filtered here — the candidate generator applies the rarity cap where the
// corpus-wide count is known.
func extractIdentifiers(text string) []string {
	if text == "" {
		return nil
	}
	seen := make(map[string]struct{})
	add := func(id string) {
		if _, ok := seen[id]; !ok && len(seen) < maxIdentifiersPerNode {
			seen[id] = struct{}{}
		}
	}
	for _, re := range []*regexp.Regexp{rePaymentRef, reActionRef} {
		for _, m := range re.FindAllString(text, -1) {
			if strings.ContainsAny(m, "0123456789") {
				add(m)
			}
		}
	}
	for _, m := range reJiraKey.FindAllString(text, -1) {
		add(m)
	}
	for _, m := range reGHPRURL.FindAllStringSubmatch(text, -1) {
		add(m[1] + "#" + m[2])
	}
	for _, m := range reGHPRShort.FindAllStringSubmatch(text, -1) {
		add(m[1] + "#" + m[2])
	}
	for _, m := range reRequestID.FindAllString(text, -1) {
		add(m)
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// threadRawText concatenates the raw bodies of every message in a Slack thread
// (root + replies), the same population summarize_thread reads. Extraction
// must see raw text: this is exactly the signal summaries destroy.
func threadRawText(ctx context.Context, deps Deps, channelID, threadTs string) (string, error) {
	rows, err := deps.DB.Query(ctx, `
SELECT COALESCE(NULLIF(n.title,''), n.body, '')
FROM graph.nodes n
WHERE n.scope = 'slack:' || $1 AND n.deleted_at IS NULL
  AND COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3)) = $2`,
		channelID, threadTs)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return "", err
		}
		b.WriteString(body)
		b.WriteString("\n")
	}
	return b.String(), rows.Err()
}

// identifiersForNode computes the identifiers for one indexed node: Slack
// thread roots read the whole thread's raw text, non-Slack resources read
// their full body, and non-root Slack messages get none (they never link out —
// same gate as link_topics).
func identifiersForNode(ctx context.Context, deps Deps, nodeType, scope, threadTs, ownTs, bodyFull string) ([]string, error) {
	if nodeType == "slack" || nodeType == "slack_thread" {
		if threadTs != ownTs || !strings.HasPrefix(scope, "slack:") || strings.HasPrefix(scope, "slack:D") {
			return nil, nil
		}
		raw, err := threadRawText(ctx, deps, strings.TrimPrefix(scope, "slack:"), threadTs)
		if err != nil {
			return nil, err
		}
		return extractIdentifiers(raw), nil
	}
	return extractIdentifiers(bodyFull), nil
}

// NewBackfillIdentifiersHandler returns the job entry for
// "backfill_identifiers": a one-shot sweep that (re)extracts identifiers for
// every indexed node. Regex-only, no LLM; idempotent, safe to re-run.
func NewBackfillIdentifiersHandler(deps Deps) jobs.Entry {
	return jobs.Entry{
		Handler:   backfillIdentifiersHandler(deps),
		Systems:   []string{},
		PoolSize:  1,
		Lease:     600 * time.Second,
		Heartbeat: true,
	}
}

func backfillIdentifiersHandler(deps Deps) jobs.Handler {
	return func(ctx context.Context, payload []byte) error {
		var p struct{}
		_ = json.Unmarshal(payload, &p)

		rows, err := deps.DB.Query(ctx, `
SELECT n.id, n.type, COALESCE(n.scope,''),
       COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3)),
       split_part(n.id,':',3),
       COALESCE(ab.body_full, n.body, '')
FROM graph.artifact_index ai
JOIN graph.nodes n ON n.id = ai.node_id
LEFT JOIN graph.artifact_bodies ab ON ab.node_id = n.id
WHERE n.deleted_at IS NULL`)
		if err != nil {
			return err
		}
		type row struct{ id, typ, scope, threadTs, ownTs, body string }
		var todo []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.id, &r.typ, &r.scope, &r.threadTs, &r.ownTs, &r.body); err != nil {
				rows.Close()
				return err
			}
			todo = append(todo, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		var updated, withIDs int
		for _, r := range todo {
			ids, err := identifiersForNode(ctx, deps, r.typ, r.scope, r.threadTs, r.ownTs, r.body)
			if err != nil {
				return fmt.Errorf("backfill_identifiers: %s: %w", r.id, err)
			}
			if ids == nil {
				ids = []string{}
			}
			if _, err := deps.DB.Exec(ctx,
				`UPDATE graph.artifact_index SET identifiers=$2 WHERE node_id=$1`,
				r.id, ids); err != nil {
				return fmt.Errorf("backfill_identifiers: update %s: %w", r.id, err)
			}
			updated++
			if len(ids) > 0 {
				withIDs++
			}
		}
		deps.Logger.Info().Int("updated", updated).Int("with_identifiers", withIDs).
			Msg("backfill_identifiers: sweep complete")
		return nil
	}
}
