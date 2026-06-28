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

// summarizeThreadPayload is the JSON payload for the summarize_thread job type.
type summarizeThreadPayload struct {
	ChannelID string `json:"channel_id"`
	ThreadTs  string `json:"thread_ts"`
}

// NewSummarizeThreadHandler returns the job entry for "summarize_thread": it
// (re)generates the cached one-line LLM topic summary for a Slack thread so the
// /topics endpoint can stay read-only and clicking a channel is instant.
func NewSummarizeThreadHandler(deps Deps) jobs.Entry {
	return jobs.Entry{
		Handler:  summarizeThreadHandler(deps),
		Systems:  []string{"gemini"},
		PoolSize: 3,
		Lease:    60 * time.Second,
	}
}

func summarizeThreadHandler(deps Deps) jobs.Handler {
	return func(ctx context.Context, payload []byte) error {
		var p summarizeThreadPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return fmt.Errorf("%w: summarize_thread unmarshal: %v", jobs.ErrFatal, err)
		}
		if p.ChannelID == "" || p.ThreadTs == "" {
			return fmt.Errorf("%w: summarize_thread: channel_id and thread_ts required", jobs.ErrFatal)
		}
		if deps.Gemini == nil {
			return nil // no LLM configured; topics falls back to first line
		}

		// Load the thread's messages (root + replies), oldest first.
		rows, err := deps.DB.Query(ctx, `
SELECT COALESCE(NULLIF(title,''), body), COALESCE(metadata->'author'->>'display_name',''),
       (EXTRACT(EPOCH FROM updated_at) * 1000)::bigint AS upd_ms
FROM graph.nodes
WHERE scope = 'slack:' || $1 AND deleted_at IS NULL AND COALESCE(metadata->>'thread_ts','') = $2
ORDER BY COALESCE(to_timestamp(NULLIF(metadata->>'ts','')::float8), first_seen_at) ASC`,
			p.ChannelID, p.ThreadTs)
		if err != nil {
			return err
		}
		var b strings.Builder
		var count int
		var maxUpdated int64
		for rows.Next() {
			var body, author string
			var upd int64
			if err := rows.Scan(&body, &author, &upd); err != nil {
				rows.Close()
				return err
			}
			count++
			if upd > maxUpdated {
				maxUpdated = upd
			}
			if author == "" {
				author = "someone"
			}
			line := author + ": " + firstLine(body, 280) + "\n"
			if b.Len()+len(line) <= 7000 {
				b.WriteString(line)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if count <= 1 {
			return nil // single message: /topics shows its first line, no LLM needed
		}

		// Signature reflects content state: message count + newest updated_at. A new
		// reply (count), an edit (updated_at bumps), or a delete (count) all change it,
		// so the cached summary regenerates instead of going stale. The "v2:" prefix
		// invalidates rows cached under the old one-line-only logic so they regenerate
		// with the deep overview+highlights. Keep this prefix in sync with channels.go.
		sig := fmt.Sprintf("v2:%d:%d", count, maxUpdated)
		// Skip if the cached summary already matches the current signature.
		var existingSig string
		_ = deps.DB.QueryRow(ctx,
			`SELECT signature FROM graph.thread_summaries WHERE channel_id=$1 AND thread_ts=$2`,
			p.ChannelID, p.ThreadTs).Scan(&existingSig)
		if existingSig == sig {
			return nil
		}

		topic, overview, highlights := genThreadDeepSummary(ctx, deps.Gemini, b.String())
		if topic == "" && overview == "" {
			return nil // transient LLM failure; leave prior summary, retry later
		}
		hlJSON, e := json.Marshal(highlights)
		if e != nil || hlJSON == nil {
			hlJSON = []byte("[]")
		}
		_, err = deps.DB.Exec(ctx,
			`INSERT INTO graph.thread_summaries(channel_id,thread_ts,signature,summary,overview,highlights,updated_at)
			 VALUES($1,$2,$3,$4,$5,$6,NOW())
			 ON CONFLICT (channel_id,thread_ts) DO UPDATE SET
			   signature=excluded.signature, summary=excluded.summary,
			   overview=excluded.overview, highlights=excluded.highlights, updated_at=NOW()`,
			p.ChannelID, p.ThreadTs, sig, topic, overview, hlJSON)
		return err
	}
}

// genThreadDeepSummary asks the LLM for a one-line topic label PLUS a short
// overview and chronological highlights for a single Slack thread, so a thread
// can be understood quickly and deeply. Returns ("","",nil) on error.
func genThreadDeepSummary(ctx context.Context, g GeminiClient, transcript string) (string, string, []string) {
	const sys = `You are given one Slack thread (messages oldest first, as "author: text").
Summarize it so a teammate understands it quickly and deeply. Respond as JSON:
{"topic":"short factual label, max 10 words, no trailing period",
 "overview":"2-3 sentences: what was raised and the current state/outcome",
 "highlights":["chronological key points / decisions, each one short line, max 6 items"]}

STRICT GROUNDING — follow exactly:
- Use ONLY facts, names, and ids that literally appear in the thread.
- NEVER invent ticket ids, people, dates, fixes, or outcomes.
- Do NOT assume the issue was resolved/deployed unless the text says so.
- If the thread is thin or inconclusive, write a short overview and return fewer
  (or zero) highlights rather than filling gaps.
No markdown, no quotes around the whole thing.`
	out, err := g.Generate(ctx, sys, transcript)
	if err != nil || out == "" {
		return "", "", nil
	}
	var parsed struct {
		Topic      string   `json:"topic"`
		Overview   string   `json:"overview"`
		Highlights []string `json:"highlights"`
	}
	if json.Unmarshal([]byte(out), &parsed) != nil {
		return "", "", nil
	}
	return firstLine(parsed.Topic, 90), strings.TrimSpace(parsed.Overview), parsed.Highlights
}

// enqueueSummarizeThread enqueues a summarize_thread job unless one is already
// queued/running for the same (channel, thread) — cheap dedup so a backfill or a
// burst of replies doesn't pile up duplicate LLM jobs. Errors are ignored
// (best-effort; the /topics endpoint re-enqueues on the next miss).
func enqueueSummarizeThread(ctx context.Context, db *pgxpool.Pool, channelID, threadTs string) {
	if channelID == "" || threadTs == "" {
		return
	}
	var exists bool
	_ = db.QueryRow(ctx, `
SELECT EXISTS(
  SELECT 1 FROM graph.jobs
  WHERE type='summarize_thread' AND status IN ('queued','running')
    AND payload->>'channel_id'=$1 AND payload->>'thread_ts'=$2)`,
		channelID, threadTs).Scan(&exists)
	if exists {
		return
	}
	_, _ = jobs.Enqueue(ctx, db, "summarize_thread", summarizeThreadPayload{
		ChannelID: channelID, ThreadTs: threadTs,
	}, jobs.EnqueueOptions{Priority: 6})
}
