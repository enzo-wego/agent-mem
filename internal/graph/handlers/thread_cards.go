package handlers

import (
	"context"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// threadCard is the per-thread header data the /search page shows on a Slack
// card: who started it, how big it is, who talked, and its time span.
type threadCard struct {
	RootAuthor       string
	MsgCount         int
	Participants     []string // up to 3, by message count desc then name asc
	ParticipantCount int
	FirstTSMs        int64
	LastTSMs         int64
	Channel          string // human channel name
}

// slackRootParts splits a Slack root id "slack:<C>:<ts>" into channel and ts.
func slackRootParts(rootID string) (channel, ts string, ok bool) {
	parts := strings.Split(rootID, ":")
	if len(parts) != 3 || parts[0] != "slack" || parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// threadCards returns card data for every Slack thread root id in one query.
// A thread is every non-deleted node with the same scope and the same
// COALESCE(NULLIF(metadata->>'thread_ts',”), split_part(id,':',3)) — the rule
// neighbors.go and pins.go use. Root ids with no thread messages are absent.
func threadCards(ctx context.Context, db *pgxpool.Pool, rootIDs []string) (map[string]threadCard, error) {
	out := map[string]threadCard{}
	var chans, threads []string
	seen := map[string]bool{}
	for _, id := range rootIDs {
		c, ts, ok := slackRootParts(id)
		if !ok || seen[id] {
			continue
		}
		seen[id] = true
		chans = append(chans, c)
		threads = append(threads, ts)
	}
	if len(chans) == 0 {
		return out, nil
	}
	// Name rule is the one in pins.go: bots keep display_name; a raw B…/U… id
	// falls back to the author name stored on the message; unknown is ''.
	rows, err := db.Query(ctx, `
WITH p AS (SELECT DISTINCT unnest($1::text[]) AS channel_id, unnest($2::text[]) AS thread_ts)
SELECT p.channel_id, p.thread_ts, COALESCE(sc.name,''), n.id = 'slack:' || p.channel_id || ':' || p.thread_ts,
       CASE WHEN pe.is_bot
            THEN COALESCE(NULLIF(pe.display_name,''), '')
            ELSE COALESCE(NULLIF(CASE WHEN pe.display_name ~ '^[BU][A-Z0-9]{6,}$' THEN '' ELSE pe.display_name END,''), NULLIF(n.metadata->'author'->>'display_name',''), '')
       END,
       (EXTRACT(EPOCH FROM COALESCE(to_timestamp(NULLIF(n.metadata->>'ts','')::float8), n.created_at, n.first_seen_at)) * 1000)::bigint
FROM p
JOIN graph.nodes n
  ON n.scope = 'slack:' || p.channel_id AND n.deleted_at IS NULL
 AND COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3)) = p.thread_ts
LEFT JOIN graph.people pe ON pe.id = n.author_person_id
LEFT JOIN graph.slack_channels sc ON sc.slack_channel_id = p.channel_id`,
		chans, threads)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type acc struct {
		card   threadCard
		counts map[string]int
	}
	accs := map[string]*acc{}
	for rows.Next() {
		var ch, ts, channel, name string
		var isRoot bool
		var ms int64
		if err := rows.Scan(&ch, &ts, &channel, &isRoot, &name, &ms); err != nil {
			return nil, err
		}
		if looksLikeSlackID(name) {
			name = ""
		}
		key := "slack:" + ch + ":" + ts
		a := accs[key]
		if a == nil {
			a = &acc{counts: map[string]int{}}
			a.card.Channel = channel
			a.card.FirstTSMs, a.card.LastTSMs = ms, ms
			accs[key] = a
		}
		a.card.MsgCount++
		if ms < a.card.FirstTSMs {
			a.card.FirstTSMs = ms
		}
		if ms > a.card.LastTSMs {
			a.card.LastTSMs = ms
		}
		if isRoot {
			a.card.RootAuthor = name
		}
		if name != "" {
			a.counts[name]++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for key, a := range accs {
		names := make([]string, 0, len(a.counts))
		for n := range a.counts {
			names = append(names, n)
		}
		sort.Slice(names, func(i, j int) bool {
			if a.counts[names[i]] != a.counts[names[j]] {
				return a.counts[names[i]] > a.counts[names[j]]
			}
			return names[i] < names[j]
		})
		a.card.ParticipantCount = len(names)
		if len(names) > 3 {
			names = names[:3]
		}
		a.card.Participants = names
		out[key] = a.card
	}
	return out, nil
}
