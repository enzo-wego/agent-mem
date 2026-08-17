package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/ids"
	"github.com/agent-mem/agent-mem/internal/graph/jobs"
	"github.com/agent-mem/agent-mem/internal/llmjson"
)

// threadSummarySigVersion is the single source of truth for the thread-summary
// cache-key version. Bumping it invalidates every cached row in
// graph.thread_summaries. Every reader and writer of a signature MUST derive it
// from this constant — it used to be hardcoded in two places kept in sync by
// comment, and a divergence would make every thread read permanently stale.
//
// v2..v8 history: see the comment block in summarizeThreadHandler above the
// signature computation.
// v9: the LLM transcript builder no longer truncates each message at its first
// newline (agent-mem-t8r), so every summary cached under v5..v8 was written from
// a transcript missing everything after line 1 of every multi-line message.
const threadSummarySigVersion = "v9"

// threadSummarySignature builds a thread-summary cache key from the version
// constant, the ingested message count, and the newest message updated_at (ms).
// It is the only place the "v<N>:count:ms" format is produced.
func threadSummarySignature(count int, newestUpdatedMs int64) string {
	return fmt.Sprintf("%s:%d:%d", threadSummarySigVersion, count, newestUpdatedMs)
}

// summarizeThreadPayload is the JSON payload for the summarize_thread job type.
// There is deliberately no Force field: the signature pair below covers both
// what the messages say and what the linked resources are called, so every
// legitimate reason to regenerate is already detected. A force flag used to
// exist for "only the links changed" and cost 1,335 LLM calls/hour for 3 real
// updates, because it bypassed the dedup and the skip check together.
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
		Lease:    SummaryLease, // must outlast a gateway Claude call; see SummaryLease
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
SELECT COALESCE(NULLIF(n.title,''), n.body), COALESCE(NULLIF(CASE WHEN p.display_name ~ '^[BU][A-Z0-9]{6,}$' THEN '' ELSE p.display_name END,''), NULLIF(n.metadata->'author'->>'display_name',''), ''),
       COALESCE(p.department,''), COALESCE(p.job_title,''),
       COALESCE(dr.domain,''), COALESCE(dr.role_label,''),
       (EXTRACT(EPOCH FROM n.updated_at) * 1000)::bigint AS upd_ms
FROM graph.nodes n
LEFT JOIN graph.people p ON p.id = n.author_person_id
LEFT JOIN graph.person_derived_roles dr ON dr.eeid = p.eeid
WHERE n.scope = 'slack:' || $1 AND n.deleted_at IS NULL
  AND COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3)) = $2
ORDER BY COALESCE(to_timestamp(NULLIF(n.metadata->>'ts','')::float8), n.first_seen_at) ASC`,
			p.ChannelID, p.ThreadTs)
		if err != nil {
			return err
		}
		var b strings.Builder
		var count int
		var maxUpdated int64
		var hasDiscussion bool
		for rows.Next() {
			var body, author, dept, jobTitle, domain, role string
			var upd int64
			if err := rows.Scan(&body, &author, &dept, &jobTitle, &domain, &role, &upd); err != nil {
				rows.Close()
				return err
			}
			count++
			if upd > maxUpdated {
				maxUpdated = upd
			}
			if !linkOnly(body) {
				hasDiscussion = true
			}
			if author == "" {
				author = "someone"
			}
			line := withDept(author, dept, jobTitle, domain, role) + ": " + flattenLines(body, 400) + "\n"
			if b.Len()+len(line) <= 7000 {
				b.WriteString(line)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if count == 0 {
			return nil // nothing ingested yet; /topics re-enqueues on next view
		}

		// Signature reflects content state: message count + newest updated_at. A new
		// reply (count), an edit (updated_at bumps), or a delete (count) all change it,
		// so the cached summary regenerates instead of going stale. The version prefix
		// is threadSummarySigVersion (see its doc comment) — bumping it there
		// invalidates every cached row. The "v2:" prefix invalidated rows cached under
		// the old one-line-only logic so they regenerated with the deep
		// overview+highlights.
		// v4: author now falls back to the resolved person's display_name when the
		// Slack author is empty (bot notifications), so summaries name the real actor
		// instead of "someone". Bump to regenerate rows cached with anonymous authors.
		// v8: author labels prefer evidence-backed domain roles when available.
		sig := threadSummarySignature(count, maxUpdated)

		// Prepend the thread's linked-resource titles so the summary can name the
		// ticket/doc it is about instead of ignoring it. When the thread is only a
		// shared link with no discussion, the resource IS the content — include a
		// short body excerpt too so the summary can describe it.
		//
		// Built BEFORE the skip check, not after: its hash is half the cache key.
		// One cheap SELECT on the skip path buys us never making a needless LLM
		// call, which is the trade that matters.
		resBlock := linkedResourceBlock(ctx, deps, p.ChannelID, p.ThreadTs, !hasDiscussion)
		linkSig := linkSignature(resBlock)

		// Skip when BOTH the messages and the linked-resource titles are unchanged.
		var existingSig, existingLinkSig string
		_ = deps.DB.QueryRow(ctx,
			`SELECT signature, COALESCE(link_signature,'')
			   FROM graph.thread_summaries WHERE channel_id=$1 AND thread_ts=$2`,
			p.ChannelID, p.ThreadTs).Scan(&existingSig, &existingLinkSig)
		if skip, backfill := summarySkip(existingSig, sig, existingLinkSig, linkSig); skip {
			if backfill {
				_, _ = deps.DB.Exec(ctx,
					`UPDATE graph.thread_summaries SET link_signature=$3
					  WHERE channel_id=$1 AND thread_ts=$2`,
					p.ChannelID, p.ThreadTs, linkSig)
			}
			return nil
		}

		topic, overview, highlights, kind := genThreadDeepSummary(ctx, deps.Gemini, resBlock+b.String())
		if topic == "" && overview == "" {
			return nil // transient LLM failure; leave prior summary, retry later
		}
		hlJSON, e := json.Marshal(highlights)
		if e != nil || hlJSON == nil {
			hlJSON = []byte("[]")
		}
		_, err = deps.DB.Exec(ctx,
			`INSERT INTO graph.thread_summaries(channel_id,thread_ts,signature,link_signature,summary,overview,highlights,kind,updated_at)
			 VALUES($1,$2,$3,$4,$5,$6,$7,$8,NOW())
			 ON CONFLICT (channel_id,thread_ts) DO UPDATE SET
			   signature=excluded.signature, link_signature=excluded.link_signature,
			   summary=excluded.summary,
			   overview=excluded.overview, highlights=excluded.highlights,
			   kind=excluded.kind, updated_at=NOW()`,
			p.ChannelID, p.ThreadTs, sig, linkSig, topic, overview, hlJSON, kind)
		if err == nil {
			if _, jErr := jobs.Enqueue(ctx, deps.DB, "index_artifact", map[string]any{
				"node_id": ids.SlackMessage(p.ChannelID, p.ThreadTs),
				"force":   true,
			}, jobs.EnqueueOptions{Priority: 5, MachineID: deps.MachineID}); jErr != nil {
				deps.Logger.Warn().Err(jErr).Str("channel_id", p.ChannelID).Str("thread_ts", p.ThreadTs).
					Msg("summarize_thread: enqueue index_artifact failed")
			}
		}
		return err
	}
}

// summarySkip decides whether a cached summary is still current, and whether its
// link_signature needs backfilling. Both signatures must match for a skip: the
// message pair (count + newest updated_at) and the linked-resource hash.
//
// A row written before link_signature existed stores "". That reads as "unknown",
// never as "changed" — otherwise the first run after deploy re-summarizes every
// thread that has links, which is exactly the burn being fixed. Such a row skips
// and gets its hash backfilled with no LLM call, so the NEXT real title change is
// caught normally.
func summarySkip(existingSig, sig, existingLinkSig, linkSig string) (skip, backfill bool) {
	if existingSig != sig {
		return false, false
	}
	if existingLinkSig == "" && linkSig != "" {
		return true, true // legacy row: trust it, record the hash for next time
	}
	return existingLinkSig == linkSig, false
}

// linkSignature hashes the linked-resource prompt block, so a referenced
// ticket/doc whose title finally landed regenerates the summary while an
// unchanged one skips. Returns "" for a thread with no linked resources.
//
// Deliberately a SEPARATE column from `signature` rather than folded into it:
// channels.go recomputes signature (count + newest updated_at) from graph.nodes
// on every channel view to spot staleness. It cannot cheaply recompute a link
// hash for every visible thread, so a combined key would look mismatched on
// every view and re-enqueue the whole channel — the same amplification through
// a different door.
func linkSignature(resBlock string) string {
	if resBlock == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(resBlock))
	return hex.EncodeToString(sum[:8])
}

// reURLToken matches bare and Slack-wrapped (<http…>, <http…|label>) URLs.
var reURLToken = regexp.MustCompile(`<?https?://[^\s>]+(\|[^>]*)?>?`)

// linkOnly reports whether text carries no substance beyond URLs — a bare
// shared link, possibly with a word or two around it.
func linkOnly(text string) bool {
	return len(strings.TrimSpace(reURLToken.ReplaceAllString(text, ""))) < 20
}

// linkedResourceBlock builds the "Linked resources:" prompt block listing the
// non-Slack resources referenced by the thread's messages. The root message is
// matched by id too — a lone root has no thread_ts metadata. withExcerpt
// additionally inlines a short body excerpt per resource, for link-only threads
// where the resource is the only content. Returns "" when there are none.
func linkedResourceBlock(ctx context.Context, deps Deps, channelID, threadTs string, withExcerpt bool) string {
	rrows, err := deps.DB.Query(ctx, `
SELECT DISTINCT r.type, COALESCE(NULLIF(r.title,''), left(COALESCE(r.body,''),120)), left(COALESCE(r.body,''),500)
FROM graph.nodes n
JOIN graph.edges e ON e.from_node_id = n.id AND e.kind = 'REFERENCES'
JOIN graph.nodes r ON r.id = e.to_node_id AND r.deleted_at IS NULL
WHERE n.scope = 'slack:' || $1 AND n.deleted_at IS NULL
  AND (n.id = 'slack:' || $1 || ':' || $2 OR COALESCE(n.metadata->>'thread_ts','') = $2)
  AND r.type NOT IN ('slack','slack_thread','slack_file')
ORDER BY 1`, channelID, threadTs)
	if err != nil {
		return ""
	}
	defer rrows.Close()
	var resBlock strings.Builder
	for rrows.Next() {
		var typ, title, excerpt string
		if rrows.Scan(&typ, &title, &excerpt) != nil || strings.TrimSpace(title) == "" {
			continue
		}
		if resBlock.Len() == 0 {
			resBlock.WriteString("Linked resources:\n")
		}
		resBlock.WriteString("- " + friendlySource(typ) + ": " + firstLine(title, 120) + "\n")
		if excerpt = strings.TrimSpace(excerpt); withExcerpt && excerpt != "" && excerpt != strings.TrimSpace(title) {
			resBlock.WriteString("  " + strings.Join(strings.Fields(excerpt), " ") + "\n")
		}
	}
	if resBlock.Len() == 0 {
		return ""
	}
	resBlock.WriteString("\nThread (oldest first):\n")
	return resBlock.String()
}

// genThreadDeepSummary asks the LLM for a one-line topic label PLUS a short
// overview, chronological highlights, and a kind classification (substantive
// vs chatter) for a single Slack thread. Returns ("","",nil,"") on error.
func genThreadDeepSummary(ctx context.Context, g GeminiClient, transcript string) (string, string, []string, string) {
	const sys = `You are given one Slack thread (messages oldest first, as "author: text").
An author may be written as "Name (Department)" — when you name that person, keep
their team label exactly as given on first mention, e.g. "Hazwan (Flights · Senior Engineer)
reported…". Never invent a department or job title that isn't given.
A "Linked resources" list may precede the thread — use those titles to identify
what the thread is about (name the ticket/doc), but summarize the thread's
discussion, not the resources themselves. EXCEPTION: if the thread is only a
shared link with no discussion, describe the linked resource itself (from its
title and excerpt) instead of saying no context was provided.
Summarize it so a teammate understands it quickly and deeply. Respond as JSON:
{"topic":"short factual label, max 10 words, no trailing period",
 "overview":"2-3 sentences: what was raised and the current state/outcome",
 "highlights":["chronological key points / decisions, each one short line, max 6 items"],
 "kind":"substantive|chatter"}

kind is "chatter" ONLY for threads with no work content at all: leave/on-call/
absence notices, greetings, thanks, and social acknowledgements that carry no
information. A confirmation that validates work state IS work content and is
"substantive" ("yes, that's correct", "confirmed, the fix is deployed", an
approval of a proposal) — losing it would lose the fact that something was
confirmed. Anything discussing work — a question, bug, task, decision, doc —
is "substantive", however short.

STRICT GROUNDING — follow exactly:
- Use ONLY facts, names, and ids that literally appear in the thread.
- KEEP concrete identifiers verbatim — payment/order ids (pxx6xgkdtl), ticket
  keys, error codes. Put the central one in the overview; downstream linking
  depends on it surviving summarization.
- NEVER invent ticket ids, people, dates, fixes, or outcomes.
- Do NOT assume the issue was resolved/deployed unless the text says so.
- If the thread is thin or inconclusive, write a short overview and return fewer
  (or zero) highlights rather than filling gaps.
No markdown, no quotes around the whole thing.`
	out, err := g.Generate(ctx, sys, transcript)
	if err != nil || out == "" {
		return "", "", nil, ""
	}
	var parsed struct {
		Topic      string   `json:"topic"`
		Overview   string   `json:"overview"`
		Highlights []string `json:"highlights"`
		Kind       string   `json:"kind"`
	}
	if json.Unmarshal(llmjson.ExtractJSON(out), &parsed) != nil {
		// Non-JSON prose reply (see prose() in cluster_summary.go): keep it as the
		// overview so the thread still summarizes and caches instead of retrying
		// forever. Topic label stays empty; callers fall back to the thread's title.
		return "", prose(out), nil, ""
	}
	kind := parsed.Kind
	if kind != "chatter" {
		kind = "substantive" // anything unexpected defaults to visible
	}
	return firstLine(parsed.Topic, 90), strings.TrimSpace(parsed.Overview), parsed.Highlights, kind
}

// BackfillMissingThreadSummaries enqueues summarize_thread for threads that
// have real discussion (2+ ingested messages, or a whole-thread fetched node)
// but no summary row yet. Called on worker startup when an LLM is configured;
// idempotent — summarized threads no longer match, and enqueueSummarizeThread
// dedups pending jobs. Returns how many were enqueued.
func BackfillMissingThreadSummaries(ctx context.Context, db *pgxpool.Pool, limit int) int {
	rows, err := db.Query(ctx, `
SELECT REPLACE(t.scope,'slack:','') AS ch, t.tt
FROM (
  SELECT n.scope,
         COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3)) AS tt,
         count(*) AS c,
         bool_or(n.type = 'slack_thread') AS has_thread_node
  FROM graph.nodes n
  WHERE n.type IN ('slack','slack_thread') AND n.deleted_at IS NULL
    AND n.scope LIKE 'slack:C%'
  GROUP BY 1, 2
) t
LEFT JOIN graph.thread_summaries ts
  ON ts.channel_id = REPLACE(t.scope,'slack:','') AND ts.thread_ts = t.tt
WHERE (t.c >= 2 OR t.has_thread_node) AND ts.channel_id IS NULL
LIMIT $1`, limit)
	if err != nil {
		return 0
	}
	type th struct{ ch, tt string }
	var todo []th
	for rows.Next() {
		var t th
		if err := rows.Scan(&t.ch, &t.tt); err != nil {
			rows.Close()
			return 0
		}
		todo = append(todo, t)
	}
	rows.Close()
	for _, t := range todo {
		enqueueSummarizeThread(ctx, db, t.ch, t.tt)
	}
	return len(todo)
}

// staleSummaryWhereSQL is the shared WHERE predicate for the stale-summary
// sweep, embedded verbatim by BOTH the capped SELECT (staleSummarySelectSQL) and
// the remaining COUNT(*) (staleSummaryCountSQL) so the two can never carry
// different filters. $1 is the current-version pattern ("vN:%").
//
// The live-node EXISTS guard is the agent-mem-9ll fix. It keeps a row out of the
// sweep unless the thread still has a non-deleted Slack node, mirroring the node
// lookup summarizeThreadHandler itself runs. An orphaned row (nodes gone) would
// otherwise be selected on every run forever: the handler returns at its
// `count == 0` early check before touching graph.thread_summaries, so the row's
// updated_at never advances and ORDER BY updated_at ASC keeps re-picking the same
// oldest orphans, never reaching a real thread. The node join mirrors
// BackfillMissingThreadSummaries above; keep the two consistent in style.
const staleSummaryWhereSQL = `ts.signature NOT LIKE $1
  AND EXISTS (
    SELECT 1 FROM graph.nodes n
    WHERE n.scope = 'slack:' || ts.channel_id
      AND COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3)) = ts.thread_ts
      AND n.deleted_at IS NULL)`

// staleSummaryCountSQL counts every stale-but-live row, ignoring the cap, so a
// capped run can report how much real work remains.
const staleSummaryCountSQL = `SELECT count(*) FROM graph.thread_summaries ts WHERE ` + staleSummaryWhereSQL

// staleSummarySelectSQL returns one capped, oldest-first page of stale-but-live
// rows to enqueue. $2 is the cap.
const staleSummarySelectSQL = `SELECT ts.channel_id, ts.thread_ts
FROM graph.thread_summaries ts
WHERE ` + staleSummaryWhereSQL + `
ORDER BY ts.updated_at ASC
LIMIT $2`

// BackfillStaleThreadSummaries enqueues summarize_thread for cached rows whose
// signature is not on the current threadSummarySigVersion. It exists because a
// version bump (agent-mem-8q4) invalidates rows only lazily, when a channel view
// renders, and BackfillMissingThreadSummaries covers only threads with NO
// summary row — so a bump strands every un-reopened channel on the old,
// superseded summary. This is a bulk LLM re-run: it is triggered ONLY by an
// explicit POST (NewBackfillStaleSummariesHandler), never on startup or a
// schedule.
//
// Rows are taken oldest-updated_at first so repeated capped runs make monotonic
// progress instead of re-picking the same rows. enqueueSummarizeThread dedups
// against a queued/running job for the same (channel, thread), so overlapping
// runs cannot pile up.
//
// The capped SELECT and the remaining COUNT(*) share staleSummaryWhereSQL, whose
// live-node EXISTS guard (agent-mem-9ll) keeps orphaned rows — those whose Slack
// nodes are gone — out of the sweep. Without it such a row is re-selected on
// every run forever, because summarizeThreadHandler returns at its `count == 0`
// early check before graph.thread_summaries.updated_at can advance, so
// oldest-first ordering keeps re-picking the same orphans and never reaches a
// real thread. Sharing the fragment keeps the two predicates from diverging.
//
// Returns matched (rows the capped query returned), enqueued, and remaining (the
// total stale count without the limit, so an operator can see how much work is
// left after a capped run). enqueued counts the rows enqueueSummarizeThread was
// called for: that helper is best-effort and silently skips duplicates, and per
// the agent-mem-8q4 non-goals must not change to report back, so enqueued is
// best-effort and can overcount rows a concurrent run already queued.
func BackfillStaleThreadSummaries(ctx context.Context, db *pgxpool.Pool, limit int) (matched, enqueued, remaining int) {
	if limit <= 0 {
		limit = backfillStaleSummariesDefaultLimit
	}
	stalePattern := threadSummarySigVersion + ":%"

	// Cheap total-remaining count without the limit, so a capped run can report
	// how much work is left.
	_ = db.QueryRow(ctx, staleSummaryCountSQL, stalePattern).Scan(&remaining)

	rows, err := db.Query(ctx, staleSummarySelectSQL, stalePattern, limit)
	if err != nil {
		return 0, 0, remaining
	}
	type th struct{ ch, tt string }
	var todo []th
	for rows.Next() {
		var t th
		if err := rows.Scan(&t.ch, &t.tt); err != nil {
			rows.Close()
			return len(todo), enqueued, remaining
		}
		todo = append(todo, t)
	}
	rows.Close()
	matched = len(todo)
	for _, t := range todo {
		enqueueSummarizeThread(ctx, db, t.ch, t.tt)
		enqueued++
	}
	return matched, enqueued, remaining
}

// enqueueSummarizeThread enqueues a summarize_thread job unless one is already
// queued/running for the same (channel, thread) — cheap dedup so a backfill or a
// burst of replies doesn't pile up duplicate LLM jobs. Errors are ignored
// (best-effort; the /topics endpoint re-enqueues on the next miss).
func enqueueSummarizeThread(ctx context.Context, db *pgxpool.Pool, channelID, threadTs string) {
	if channelID == "" || threadTs == "" {
		return
	}
	// Always dedup against a pending job for the same thread. Safe now that the
	// skip check covers link titles as well as messages: whenever that pending
	// job runs it recomputes both signatures, so it picks up a title that landed
	// after it was enqueued instead of skipping. There is nothing left for a
	// second queued job to catch that this one won't.
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
