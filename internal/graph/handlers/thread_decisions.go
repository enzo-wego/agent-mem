package handlers

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/agent-mem/agent-mem/internal/graph/ids"
	"github.com/jackc/pgx/v5/pgxpool"
)

// threadDecision is one settled point of a Slack thread. URL is never stored:
// readers build it from channel + ts (slackMessagePermalink).
type threadDecision struct {
	Text string `json:"text"`
	By   string `json:"by"`
	Date string `json:"date"`
	TS   string `json:"ts"`
	URL  string `json:"url,omitempty"`
}

func groundDecisions(in []threadDecision, dateOf map[string]string) []threadDecision {
	out := []threadDecision{}
	for _, d := range in {
		ts := strings.TrimPrefix(strings.TrimSpace(d.TS), "ts=")
		date, ok := dateOf[ts]
		if !ok || strings.TrimSpace(d.Text) == "" {
			continue // grounding guard: the model cited a ts that is not in this thread
		}
		out = append(out, threadDecision{Text: strings.TrimSpace(d.Text), By: strings.TrimSpace(d.By), Date: date, TS: ts})
	}
	if len(out) > 20 {
		out = out[len(out)-20:]
	}
	return out
}

func cleanOpenQuestions(in []string) []string {
	out := []string{}
	for _, q := range in {
		if q = strings.TrimSpace(q); q != "" {
			out = append(out, q)
			if len(out) == 10 {
				break
			}
		}
	}
	return out
}

func slackMessagePermalink(channelID, threadTs, ts string) string {
	u := slackPermalink(ids.SlackMessage(channelID, ts))
	if u == "" || ts == threadTs {
		return u
	}
	return u + "?thread_ts=" + threadTs + "&cid=" + channelID
}

func decodeThreadDecisions(channelID, threadTs string, decRaw, oqRaw []byte) ([]threadDecision, []string) {
	var decisions []threadDecision
	var questions []string
	if json.Unmarshal(decRaw, &decisions) != nil {
		decisions = nil
	}
	if json.Unmarshal(oqRaw, &questions) != nil {
		questions = nil
	}
	for i := range decisions {
		decisions[i].URL = slackMessagePermalink(channelID, threadTs, decisions[i].TS)
	}
	return decisions, questions
}

const decisionsScopeSQL = `WITH sized AS (
  SELECT ts.channel_id, ts.thread_ts,
         (SELECT COALESCE(sum(least(length(COALESCE(NULLIF(n.body,''), n.title, '')), 2000)), 0)
            FROM graph.nodes n
           WHERE n.scope = 'slack:' || ts.channel_id AND n.deleted_at IS NULL
             AND COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3)) = ts.thread_ts) AS chars
  FROM graph.thread_summaries ts
  WHERE ts.decisions IS NULL AND ts.kind <> 'chatter'
    AND ($1::text[] IS NULL OR (ts.channel_id, ts.thread_ts) IN (SELECT unnest($1::text[]), unnest($2::text[])))
)
SELECT channel_id, thread_ts, chars FROM sized WHERE chars > 7000`

type decisionsCandidate struct {
	ChannelID string `json:"channel_id"`
	ThreadTs  string `json:"thread_ts"`
	Chars     int    `json:"chars"`
}

// BackfillThreadDecisions selects only legacy summaries whose old transcript was
// truncated. It defaults to a read-only scope preview at the HTTP boundary.
func BackfillThreadDecisions(ctx context.Context, db *pgxpool.Pool, limit int, dryRun bool, chans, tss []string) (picked []decisionsCandidate, enqueued, remaining int, err error) {
	picked = []decisionsCandidate{}
	if err = db.QueryRow(ctx, "SELECT count(*) FROM ("+decisionsScopeSQL+") s", chans, tss).Scan(&remaining); err != nil {
		return
	}
	rows, e := db.Query(ctx, decisionsScopeSQL+" ORDER BY chars DESC, channel_id, thread_ts LIMIT $3", chans, tss, limit)
	if e != nil {
		err = e
		return
	}
	for rows.Next() {
		var c decisionsCandidate
		if err = rows.Scan(&c.ChannelID, &c.ThreadTs, &c.Chars); err != nil {
			rows.Close()
			return
		}
		picked = append(picked, c)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return
	}
	if !dryRun {
		for _, c := range picked {
			enqueueSummarize(ctx, db, summarizeThreadPayload{ChannelID: c.ChannelID, ThreadTs: c.ThreadTs, SkipJudging: true, FillDecisions: true})
			enqueued++
		}
	}
	return
}
