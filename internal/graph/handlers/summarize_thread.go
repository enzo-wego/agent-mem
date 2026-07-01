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
// Force bypasses the signature-skip so a thread re-summarizes even when its
// messages are unchanged — used when only its linked resources changed (a new
// Jira/Confluence reference, or a referenced resource's title finally landing).
type summarizeThreadPayload struct {
	ChannelID string `json:"channel_id"`
	ThreadTs  string `json:"thread_ts"`
	Force     bool   `json:"force,omitempty"`
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
SELECT COALESCE(NULLIF(n.title,''), n.body), COALESCE(NULLIF(n.metadata->'author'->>'display_name',''), p.display_name, ''),
       COALESCE(p.department,''),
       (EXTRACT(EPOCH FROM n.updated_at) * 1000)::bigint AS upd_ms
FROM graph.nodes n
LEFT JOIN graph.people p ON p.id = n.author_person_id
WHERE n.scope = 'slack:' || $1 AND n.deleted_at IS NULL AND COALESCE(n.metadata->>'thread_ts','') = $2
ORDER BY COALESCE(to_timestamp(NULLIF(n.metadata->>'ts','')::float8), n.first_seen_at) ASC`,
			p.ChannelID, p.ThreadTs)
		if err != nil {
			return err
		}
		var b strings.Builder
		var count int
		var maxUpdated int64
		for rows.Next() {
			var body, author, dept string
			var upd int64
			if err := rows.Scan(&body, &author, &dept, &upd); err != nil {
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
			line := withDept(author, dept) + ": " + firstLine(body, 280) + "\n"
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
		// v4: author now falls back to the resolved person's display_name when the
		// Slack author is empty (bot notifications), so summaries name the real actor
		// instead of "someone". Bump to regenerate rows cached with anonymous authors.
		// Keep this prefix in sync with channels.go.
		sig := fmt.Sprintf("v5:%d:%d", count, maxUpdated)
		// Skip if the cached summary already matches the current signature.
		var existingSig string
		_ = deps.DB.QueryRow(ctx,
			`SELECT signature FROM graph.thread_summaries WHERE channel_id=$1 AND thread_ts=$2`,
			p.ChannelID, p.ThreadTs).Scan(&existingSig)
		if !p.Force && existingSig == sig {
			return nil
		}

		// Prepend the thread's linked-resource titles so the summary can name the
		// ticket/doc it is about instead of ignoring it. Titles only — the deep
		// content lives in search and the cluster view; full bodies would bloat the
		// prompt and drift the summary toward the resource instead of the thread.
		var resBlock strings.Builder
		if rrows, rerr := deps.DB.Query(ctx, `
SELECT DISTINCT r.type, COALESCE(NULLIF(r.title,''), left(COALESCE(r.body,''),120))
FROM graph.nodes n
JOIN graph.edges e ON e.from_node_id = n.id AND e.kind = 'REFERENCES'
JOIN graph.nodes r ON r.id = e.to_node_id AND r.deleted_at IS NULL
WHERE n.scope = 'slack:' || $1 AND n.deleted_at IS NULL
  AND COALESCE(n.metadata->>'thread_ts','') = $2
  AND r.type NOT IN ('slack','slack_thread','slack_file')
ORDER BY 1`, p.ChannelID, p.ThreadTs); rerr == nil {
			var lines []string
			for rrows.Next() {
				var typ, title string
				if rrows.Scan(&typ, &title) == nil && strings.TrimSpace(title) != "" {
					lines = append(lines, "- "+friendlySource(typ)+": "+firstLine(title, 120))
				}
			}
			rrows.Close()
			if len(lines) > 0 {
				resBlock.WriteString("Linked resources:\n")
				for _, l := range lines {
					resBlock.WriteString(l + "\n")
				}
				resBlock.WriteString("\nThread (oldest first):\n")
			}
		}

		topic, overview, highlights := genThreadDeepSummary(ctx, deps.Gemini, resBlock.String()+b.String())
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
An author may be written as "Name (Department)" — when you name that person, keep
their department on first mention, e.g. "Hazwan (Flights) reported…". Do not add a
department that isn't given.
A "Linked resources" list may precede the thread — use those titles to identify
what the thread is about (name the ticket/doc), but summarize the thread's
discussion, not the resources themselves.
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
func enqueueSummarizeThread(ctx context.Context, db *pgxpool.Pool, channelID, threadTs string, force bool) {
	if channelID == "" || threadTs == "" {
		return
	}
	// Non-force jobs dedup against a pending job for the same thread. A force job
	// (a resource link changed) always enqueues, so a pending non-force job that
	// would signature-skip can't swallow the refresh.
	if !force {
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
	}
	_, _ = jobs.Enqueue(ctx, db, "summarize_thread", summarizeThreadPayload{
		ChannelID: channelID, ThreadTs: threadTs, Force: force,
	}, jobs.EnqueueOptions{Priority: 6})
}
