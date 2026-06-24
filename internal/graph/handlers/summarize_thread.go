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
       (EXTRACT(EPOCH FROM COALESCE(to_timestamp(NULLIF(metadata->>'ts','')::float8), first_seen_at)) * 1000)::bigint AS ts_ms
FROM graph.nodes
WHERE scope = 'slack:' || $1 AND deleted_at IS NULL AND COALESCE(metadata->>'thread_ts','') = $2
ORDER BY COALESCE(to_timestamp(NULLIF(metadata->>'ts','')::float8), first_seen_at) ASC`,
			p.ChannelID, p.ThreadTs)
		if err != nil {
			return err
		}
		var b strings.Builder
		var count int
		var lastMs int64
		for rows.Next() {
			var body, author string
			var ts int64
			if err := rows.Scan(&body, &author, &ts); err != nil {
				rows.Close()
				return err
			}
			count++
			if ts > lastMs {
				lastMs = ts
			}
			line := author + ": " + firstLine(body, 200) + "\n"
			if b.Len()+len(line) <= 4000 {
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

		sig := fmt.Sprintf("%d:%d", count, lastMs)
		// Skip if the cached summary already matches the current signature.
		var existingSig string
		_ = deps.DB.QueryRow(ctx,
			`SELECT signature FROM graph.thread_summaries WHERE channel_id=$1 AND thread_ts=$2`,
			p.ChannelID, p.ThreadTs).Scan(&existingSig)
		if existingSig == sig {
			return nil
		}

		summary := genThreadSummary(ctx, deps.Gemini, b.String())
		if summary == "" {
			return nil // transient LLM failure; leave prior summary, retry later
		}
		_, err = deps.DB.Exec(ctx,
			`INSERT INTO graph.thread_summaries(channel_id,thread_ts,signature,summary,updated_at)
			 VALUES($1,$2,$3,$4,NOW())
			 ON CONFLICT (channel_id,thread_ts) DO UPDATE SET signature=excluded.signature, summary=excluded.summary, updated_at=NOW()`,
			p.ChannelID, p.ThreadTs, sig, summary)
		return err
	}
}

// genThreadSummary asks Gemini for a one-line topic label. Returns "" on error.
func genThreadSummary(ctx context.Context, g GeminiClient, transcript string) string {
	const sys = `You label a Slack conversation with a short, factual topic (max 10 words). No quotes, no trailing period. Respond as JSON: {"topic":"..."}`
	out, err := g.Generate(ctx, sys, transcript)
	if err != nil || out == "" {
		return ""
	}
	var parsed struct {
		Topic string `json:"topic"`
	}
	if json.Unmarshal([]byte(out), &parsed) != nil {
		return ""
	}
	return firstLine(parsed.Topic, 90)
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
