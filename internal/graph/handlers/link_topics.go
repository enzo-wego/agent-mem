package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

const (
	topicLinkShortlistLimit = 12
	topicLinkMinFloor       = 0.65
	// topicLinkIdentifierRarityCap: an identifier appearing in more indexed
	// artifacts than this is generic (a widely-quoted error code, a hot Jira
	// ticket) and nominates no candidates — the judge would drown in noise.
	topicLinkIdentifierRarityCap = 6
	// topicLinkConfirmConcurrency bounds parallel judge calls per job. With the
	// job pool (see NewLinkTopicsHandler) this caps total in-flight Gemini
	// requests at pool×concurrency — sized well under the API's rate limit;
	// 429s degrade to client-side backoff, never data loss.
	topicLinkConfirmConcurrency = 3
)

type linkTopicsPayload struct {
	NodeID string `json:"node_id"`
	Force  bool   `json:"force,omitempty"`
	// ExtraCandidates are node ids to judge in addition to the generated
	// candidates — the neighbors panel queues threads it displayed without a
	// verdict, so every visible pair converges to confirmed/refused.
	ExtraCandidates []string `json:"extra_candidates,omitempty"`
}

type topicLinkNode struct {
	NodeID      string
	Type        string
	Scope       string
	Summary     string
	SummaryKind string
	Department  string
	Kind        string // thread_summaries.kind: "chatter" never links
}

type topicLinkCandidate struct {
	topicLinkNode
	Cosine float64
	// SharedIDs is non-empty for identifier-nominated candidates: the rare
	// identifiers found in BOTH artifacts' raw text.
	SharedIDs []string
}

// topicLinkContext carries the per-pair time context shown to the judge.
type topicLinkContext struct {
	SourceWindow string
	CandWindow   string
	TimeDesc     string
}

type topicLinkJudgment struct {
	SameTopic  bool
	Confidence float64
	Tag        string
	Topic      string
	Why        string
}

// NewLinkTopicsHandler returns the job entry for exact-topic link generation.
func NewLinkTopicsHandler(deps Deps) jobs.Entry {
	return jobs.Entry{
		Handler:  linkTopicsHandler(deps),
		Systems:  []string{"gemini"},
		PoolSize: 4,
		Lease:    120 * time.Second,
	}
}

func linkTopicsHandler(deps Deps) jobs.Handler {
	return func(ctx context.Context, payload []byte) error {
		var p linkTopicsPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return fmt.Errorf("%w: link_topics unmarshal: %v", jobs.ErrFatal, err)
		}
		if p.NodeID == "" {
			return fmt.Errorf("%w: link_topics: node_id is required", jobs.ErrFatal)
		}
		if deps.Gemini == nil {
			return nil
		}

		source, err := loadTopicLinkSource(ctx, deps, p.NodeID)
		if err != nil || strings.TrimSpace(source.Summary) == "" {
			return err
		}
		if skipTopicLinkSource(source) {
			return nil
		}
		// Deterministic first: pasted Slack permalinks become root→root
		// REFERS_TO edges before any LLM judging.
		if source.Type == "slack" || source.Type == "slack_thread" {
			if err := materializeThreadReferences(ctx, deps, source.NodeID); err != nil {
				deps.Logger.Warn().Err(err).Str("node_id", p.NodeID).Msg("link_topics: materialize REFERS_TO failed")
			}
		}

		embCands, err := shortlistTopicLinks(ctx, deps, p.NodeID)
		if err != nil {
			return err
		}
		idCands, err := identifierCandidates(ctx, deps, p.NodeID)
		if err != nil {
			return err
		}
		var extraCands []topicLinkCandidate
		if len(p.ExtraCandidates) > 0 {
			if extraCands, err = explicitTopicCandidates(ctx, deps, p.NodeID, p.ExtraCandidates); err != nil {
				return err
			}
		}
		edgeCands, err := existingSameTopicCandidates(ctx, deps, p.NodeID)
		if err != nil {
			return err
		}
		cands := mergeTopicCandidates(idCands, extraCands, edgeCands, embCands)
		srcStart, srcEnd, srcOK := nodeActivityWindow(ctx, deps, p.NodeID)

		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(topicLinkConfirmConcurrency)
		for _, cand := range cands {
			cand := cand
			if strings.TrimSpace(cand.Summary) == "" {
				continue
			}
			g.Go(func() error {
				candStart, candEnd, candOK := nodeActivityWindow(gctx, deps, cand.NodeID)
				timeDesc, timeBucket := timeRelation(srcStart, srcEnd, srcOK, candStart, candEnd, candOK)
				from, to, sourceSummary, targetSummary := canonicalTopicPair(source.NodeID, cand.NodeID, source.Summary, cand.Summary)
				contentHash := topicLinkContentHash(from, to, sourceSummary, targetSummary, cand.SharedIDs, timeBucket)
				judgment, cached, err := cachedTopicLinkJudgment(gctx, deps, from, to, contentHash)
				if err != nil {
					return err
				}
				if !cached || p.Force {
					judgment, err = confirmTopicLink(gctx, deps, source, cand, topicLinkContext{
						SourceWindow: formatWindow(srcStart, srcEnd, srcOK),
						CandWindow:   formatWindow(candStart, candEnd, candOK),
						TimeDesc:     timeDesc,
					})
					if err != nil {
						return err
					}
					if err := saveTopicLinkJudgment(gctx, deps, from, to, contentHash, judgment); err != nil {
						return err
					}
				}
				if judgment.SameTopic {
					return upsertSameTopicEdge(gctx, deps, from, to, judgment, cand, timeDesc)
				}
				return deleteSameTopicEdge(gctx, deps, from, to)
			})
		}
		if err := g.Wait(); err != nil {
			return err
		}
		return propagateCaseTopics(ctx, deps, source.NodeID)
	}
}

// caseRefSQL matches identifiers that name ONE concrete case: payment/source/
// dispute refs (p·s·d + 9 chars, at least one digit) and action refs (a + 14).
// Stricter than extractIdentifiers on purpose — propagation OVERRIDES a judge
// verdict, so it takes precision over recall:
//
//   - Jira keys and PR refs are excluded (tie-breaker #2: stand-ups and release
//     lists quote ticket ids in passing).
//   - Request UUIDs are excluded. The pattern is sanctioned by tie-breaker #1,
//     but in this corpus 4 such ids spanned 51 pairs — session/artifact ids, not
//     cases (one linked a node titled "Claude Artifact" to a PWA service-worker
//     PR). Rarity capping does not help: 3 of the 4 sit under the cap.
//   - A word with a trailing counter is refused. "scheduler1" is a legal
//     payment-ref shape (s + 9 body chars + a digit) and reached production as a
//     shared identifier. Digit-count thresholds cannot separate these: the
//     verified real ref pxx6xgkdtl also carries a single digit.
//
// ponytail: the word-plus-counter guard covers the observed class only. The real
// fix is in extractIdentifiers, which needs re-indexing to change — see the bead.
const caseRefSQL = `(sid ~ '^[psd][0-9b-oqrt-z]{9}$' AND sid ~ '[0-9]'
     AND sid !~ '^[psd][a-z]{8}[0-9]$')
  OR sid ~ '^a[0-9b-oqrt-z]{14}$'`

// casePropagationMethod marks edges derived by propagateCaseTopics rather than
// judged directly. Propagation never chains off its own output (one hop only)
// and existingSameTopicCandidates skips these partners — re-judging a pair the
// rule re-derives anyway just burns a judge call.
const casePropagationMethod = "case-propagation"

// casePropagatedHash marks a judgment row the LLM never saw for this pair. It
// can never equal a real content hash, so the next run re-judges the pair and
// propagation re-applies afterwards — the end state of a run is consistent
// whichever way the pairwise judge went.
const casePropagatedHash = "case-propagated"

// propagateCaseTopics closes the triangle the pairwise judge cannot see: two
// threads joined by a shared payment/order reference are ONE case (rules
// tie-breaker #1, "these identify one concrete case"), so a topic verdict
// against either of them holds for both. Without this the judge contradicts
// itself — verified on slack:C048WV1BZTK:1784600389.693489, where in one run it
// confirmed the #ext-wego-juspay thread for order p0yy6hmqdw and refused the
// #payments-team thread for the SAME order, which were themselves linked at
// confidence 1.0.
//
// Deterministic, no LLM call. Only fills gaps: a directly judged edge keeps its
// own metadata, and a stored verdict is overwritten only when it says DIFFERENT.
func propagateCaseTopics(ctx context.Context, deps Deps, nodeID string) error {
	const casesCTE = `
WITH confirmed AS (
  SELECT CASE WHEN e.from_node_id=$1 THEN e.to_node_id ELSE e.from_node_id END AS p,
         e.metadata AS meta
  FROM graph.edges e
  WHERE e.kind='SAME_TOPIC' AND $1 IN (e.from_node_id, e.to_node_id)
    AND COALESCE(e.metadata->>'method','') <> '` + casePropagationMethod + `'
),
cases AS (
  SELECT c.p,
         CASE WHEN e.from_node_id=c.p THEN e.to_node_id ELSE e.from_node_id END AS q,
         c.meta,
         e.metadata->>'shared_ids' AS shared
  FROM confirmed c
  JOIN graph.edges e ON e.kind='SAME_TOPIC' AND c.p IN (e.from_node_id, e.to_node_id)
  WHERE e.metadata->>'method' = 'shared-identifier + llm-confirm'
    AND COALESCE(NULLIF(e.metadata->>'confidence','')::float8, 0) >= 0.9
    AND EXISTS (
      SELECT 1 FROM jsonb_array_elements_text(e.metadata->'shared_ids') AS t(sid)
      WHERE ` + caseRefSQL + `)
),
-- DISTINCT ON: one node can be a case-mate of SEVERAL confirmed partners (two
-- threads about this case both confirmed), which would emit the same pair twice
-- and make ON CONFLICT DO UPDATE fail with "cannot affect row a second time"
-- (hit in production on the p9y0yhtbd5 thread). Strongest verdict wins.
derived AS (
  SELECT DISTINCT ON (a, b) a, b, meta FROM (
  SELECT LEAST($1, q) AS a, GREATEST($1, q) AS b,
         jsonb_build_object(
           'method', '` + casePropagationMethod + `',
           'confidence', COALESCE(NULLIF(meta->>'confidence','')::float8, 0.9),
           'tag', COALESCE(meta->>'tag',''),
           'topic', COALESCE(meta->>'topic',''),
           'via', p,
           'why', 'Same concrete case as ' || p || ' (shared ' || COALESCE(shared,'identifier') ||
                  '), which is confirmed same topic as this thread.'
         ) AS meta
  FROM cases
  WHERE q <> $1
  ) d
  ORDER BY a, b, (meta->>'confidence')::float8 DESC
)`
	// derived deliberately does NOT filter out pairs that already have an edge:
	// both statements below run this same CTE, so a filter here would leave the
	// judgment upsert nothing to do once the edge insert had run. Each statement
	// carries its own guard instead — DO NOTHING keeps a directly judged edge's
	// metadata, and the judgment upsert only overwrites a DIFFERENT verdict.

	if _, err := deps.DB.Exec(ctx, casesCTE+`
INSERT INTO graph.edges (from_node_id, to_node_id, kind, metadata, machine_id)
SELECT a, b, 'SAME_TOPIC', meta, $2 FROM derived
ON CONFLICT (from_node_id, to_node_id, kind) DO NOTHING`,
		nodeID, deps.MachineID); err != nil {
		return err
	}

	// The panel and every other reader take the verdict from the judgment table,
	// not the edge — so a propagated link that leaves a stale DIFFERENT row would
	// still render as ✕ refused.
	_, err := deps.DB.Exec(ctx, casesCTE+`
INSERT INTO graph.topic_link_judgments
  (source_node_id, target_node_id, content_hash, same_topic, confidence, tag, topic, why, judged_at, machine_id)
SELECT a, b, '`+casePropagatedHash+`', TRUE,
       COALESCE(NULLIF(meta->>'confidence','')::float8, 0.9),
       COALESCE(meta->>'tag',''), COALESCE(meta->>'topic',''), COALESCE(meta->>'why',''), NOW(), $2
FROM derived
ON CONFLICT (source_node_id, target_node_id) DO UPDATE SET
  content_hash=EXCLUDED.content_hash,
  same_topic=TRUE,
  confidence=EXCLUDED.confidence,
  tag=EXCLUDED.tag,
  topic=EXCLUDED.topic,
  why=EXCLUDED.why,
  judged_at=NOW(),
  machine_id=EXCLUDED.machine_id
WHERE NOT graph.topic_link_judgments.same_topic`,
		nodeID, deps.MachineID)
	return err
}

func loadTopicLinkSource(ctx context.Context, deps Deps, nodeID string) (topicLinkNode, error) {
	var n topicLinkNode
	err := deps.DB.QueryRow(ctx, `
SELECT n.id, n.type, COALESCE(n.scope,''), COALESCE(ai.summary,''), COALESCE(ai.summary_kind,''), COALESCE(p.department,''),
       COALESCE(ts.kind,'')
FROM graph.nodes n
JOIN graph.artifact_index ai ON ai.node_id = n.id
LEFT JOIN graph.people p ON p.id = n.author_person_id
LEFT JOIN graph.thread_summaries ts
  ON ts.channel_id = REPLACE(n.scope,'slack:','')
  AND ts.thread_ts = COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3))
WHERE n.id=$1 AND n.deleted_at IS NULL`,
		nodeID,
	).Scan(&n.NodeID, &n.Type, &n.Scope, &n.Summary, &n.SummaryKind, &n.Department, &n.Kind)
	return n, err
}

// topicLinkMinSummaryChars: below this, a summary carries no topic signal —
// the 806-edge "identical content ('Context')" class was Jira stubs whose
// whole summary was one boilerplate heading. ponytail: raise-only knob; the
// real fix is substantive Jira summaries (describe-then-embed bead).
const topicLinkMinSummaryChars = 40

// skipTopicLinkSource keeps the Slack thread as the linking unit: individual
// Slack messages carry only a heuristic (raw-text) summary, and linking on raw
// text is exactly the noise this feature replaces. Only thread roots embedding
// their resource-aware summary link out. Files never link (identical HTML
// exports judged "same topic" 252 times), and no-substance summaries can't
// establish a topic at all.
func skipTopicLinkSource(source topicLinkNode) bool {
	if source.Type == "slack_file" || source.Type == "jira_attachment" {
		return true
	}
	if source.Kind == "chatter" {
		return true // leave notices, greetings, acks — nothing to topic-link
	}
	if len(strings.TrimSpace(source.Summary)) < topicLinkMinSummaryChars {
		return true
	}
	if source.Type != "slack" && source.Type != "slack_thread" {
		return false
	}
	return strings.HasPrefix(source.Scope, "slack:D") || source.SummaryKind != "thread_summary"
}

func shortlistTopicLinks(ctx context.Context, deps Deps, nodeID string) ([]topicLinkCandidate, error) {
	rows, err := deps.DB.Query(ctx, `
WITH src AS (
  SELECT ai.embedding AS emb,
         REPLACE(n.scope,'slack:','') AS ch,
         COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3)) AS tt
  FROM graph.nodes n
  JOIN graph.artifact_index ai ON ai.node_id = n.id
  WHERE n.id = $1 AND ai.embedding IS NOT NULL
),
sims AS (
  SELECT n.id, n.type, COALESCE(ai.summary,'') AS summary, COALESCE(p.department,'') AS department,
         1.0 - (ai.embedding <=> src.emb) AS cosine,
         REPLACE(n.scope,'slack:','') AS ch,
         COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3)) AS tt
  FROM graph.artifact_index ai
  JOIN graph.nodes n ON n.id = ai.node_id
  LEFT JOIN graph.people p ON p.id = n.author_person_id
  CROSS JOIN src
  WHERE ai.node_id <> $1
    AND ai.embedding IS NOT NULL
    AND n.deleted_at IS NULL
    AND NOT (n.type IN ('slack','slack_thread') AND ai.summary_kind <> 'thread_summary')
  AND n.type NOT IN ('slack_file','jira_attachment')
  AND length(TRIM(COALESCE(ai.summary,''))) >= 40
  AND NOT EXISTS (SELECT 1 FROM graph.thread_summaries tsk
    WHERE n.type IN ('slack','slack_thread')
      AND tsk.channel_id = REPLACE(n.scope,'slack:','')
      AND tsk.thread_ts = COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3))
      AND tsk.kind = 'chatter')
    AND NOT (n.type IN ('slack','slack_thread') AND n.scope LIKE 'slack:D%')
    AND NOT (n.type IN ('slack','slack_thread')
      AND REPLACE(n.scope,'slack:','') = src.ch
      AND COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3)) = src.tt)
),
stats AS (
  SELECT avg(cosine) AS mean, stddev_pop(cosine) AS sigma FROM sims
)
SELECT id, type, summary, department, cosine
FROM sims, stats
WHERE cosine >= GREATEST(COALESCE(mean + 2 * sigma, $2), $2)
ORDER BY cosine DESC
LIMIT $3`,
		nodeID, topicLinkMinFloor, topicLinkShortlistLimit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []topicLinkCandidate
	for rows.Next() {
		var c topicLinkCandidate
		if err := rows.Scan(&c.NodeID, &c.Type, &c.Summary, &c.Department, &c.Cosine); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func canonicalTopicPair(a, b, aSummary, bSummary string) (from, to, fromSummary, toSummary string) {
	if a <= b {
		return a, b, aSummary, bSummary
	}
	return b, a, bSummary, aSummary
}

// topicLinkContentHash keys the judgment cache on everything the judge saw:
// the rules version (a rules change must re-judge cached pairs), summaries,
// shared identifiers, and the COARSE time bucket (exact timestamps would
// invalidate the cache on every new reply).
func topicLinkContentHash(from, to, fromSummary, toSummary string, sharedIDs []string, timeBucket string) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("rules-v%d\x00", loadedTopicRules.Version) +
		from + "\x00" + to + "\x00" + fromSummary + "\x00" + toSummary +
		"\x00" + strings.Join(sharedIDs, ",") + "\x00" + timeBucket))
	return hex.EncodeToString(h[:])
}

// existingSameTopicCandidates re-verifies the node's current SAME_TOPIC
// partners. Without this, a pair confirmed under old rules would keep its
// edge forever once it drops out of the shortlist — re-judging (cache-missed
// by the rules version above) deletes edges the new rules refuse.
func existingSameTopicCandidates(ctx context.Context, deps Deps, nodeID string) ([]topicLinkCandidate, error) {
	rows, err := deps.DB.Query(ctx, `
SELECT CASE WHEN from_node_id=$1 THEN to_node_id ELSE from_node_id END
FROM graph.edges
WHERE kind='SAME_TOPIC' AND (from_node_id=$1 OR to_node_id=$1)
  AND COALESCE(metadata->>'method','') <> '`+casePropagationMethod+`'`, nodeID)
	if err != nil {
		return nil, err
	}
	var partners []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return nil, err
		}
		partners = append(partners, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(partners) == 0 {
		return nil, err
	}
	return explicitTopicCandidates(ctx, deps, nodeID, partners)
}

// identifierCandidates nominates artifacts sharing at least one RARE
// identifier with the source — regardless of embedding distance and age. This
// is the recall path embeddings structurally miss: an investigation and its
// partner-raise are worded differently even when about the same payment.
func identifierCandidates(ctx context.Context, deps Deps, nodeID string) ([]topicLinkCandidate, error) {
	rows, err := deps.DB.Query(ctx, `
WITH src AS (
  SELECT ai.embedding AS emb, ai.identifiers AS ids,
         REPLACE(n.scope,'slack:','') AS ch,
         COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3)) AS tt
  FROM graph.nodes n
  JOIN graph.artifact_index ai ON ai.node_id = n.id
  WHERE n.id = $1
),
rare AS (
  SELECT ident
  FROM src, unnest(src.ids) AS ident
  WHERE (SELECT count(*) FROM graph.artifact_index ai2 WHERE ai2.identifiers @> ARRAY[ident]) <= $2
)
SELECT n.id, n.type, COALESCE(ai.summary,''), COALESCE(p.department,''),
       COALESCE(1.0 - (ai.embedding <=> src.emb), 0) AS cosine,
       ARRAY(SELECT r.ident FROM rare r WHERE ai.identifiers @> ARRAY[r.ident] ORDER BY r.ident) AS shared
FROM graph.artifact_index ai
JOIN graph.nodes n ON n.id = ai.node_id
LEFT JOIN graph.people p ON p.id = n.author_person_id
CROSS JOIN src
WHERE ai.node_id <> $1
  AND ai.identifiers && (SELECT COALESCE(array_agg(ident), '{}') FROM rare)
  AND n.deleted_at IS NULL
  AND NOT (n.type IN ('slack','slack_thread') AND ai.summary_kind <> 'thread_summary')
  AND n.type NOT IN ('slack_file','jira_attachment')
  AND length(TRIM(COALESCE(ai.summary,''))) >= 40
  AND NOT EXISTS (SELECT 1 FROM graph.thread_summaries tsk
    WHERE n.type IN ('slack','slack_thread')
      AND tsk.channel_id = REPLACE(n.scope,'slack:','')
      AND tsk.thread_ts = COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3))
      AND tsk.kind = 'chatter')
  AND NOT (n.type IN ('slack','slack_thread') AND n.scope LIKE 'slack:D%')
  AND NOT (n.type IN ('slack','slack_thread')
    AND REPLACE(n.scope,'slack:','') = src.ch
    AND COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3)) = src.tt)
ORDER BY cosine DESC
LIMIT $3`,
		nodeID, topicLinkIdentifierRarityCap, topicLinkShortlistLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []topicLinkCandidate
	for rows.Next() {
		var c topicLinkCandidate
		if err := rows.Scan(&c.NodeID, &c.Type, &c.Summary, &c.Department, &c.Cosine, &c.SharedIDs); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// mergeTopicCandidates concatenates candidate lists in priority order,
// deduping by node id — earlier lists win (identifier candidates keep their
// SharedIDs over an embedding duplicate).
func mergeTopicCandidates(lists ...[]topicLinkCandidate) []topicLinkCandidate {
	var out []topicLinkCandidate
	seen := map[string]struct{}{}
	for _, l := range lists {
		for _, c := range l {
			if _, ok := seen[c.NodeID]; ok {
				continue
			}
			seen[c.NodeID] = struct{}{}
			out = append(out, c)
		}
	}
	return out
}

// explicitTopicCandidates loads specific node ids as judge candidates (same
// gates as generated candidates) — pairs a viewer saw without a verdict.
func explicitTopicCandidates(ctx context.Context, deps Deps, nodeID string, ids []string) ([]topicLinkCandidate, error) {
	rows, err := deps.DB.Query(ctx, `
SELECT n.id, n.type, COALESCE(ai.summary,''), COALESCE(p.department,''),
       COALESCE(1.0 - (ai.embedding <=> src.emb), 0) AS cosine
FROM graph.artifact_index ai
JOIN graph.nodes n ON n.id = ai.node_id
LEFT JOIN graph.people p ON p.id = n.author_person_id
LEFT JOIN (SELECT embedding AS emb FROM graph.artifact_index WHERE node_id = $1) src ON TRUE
WHERE ai.node_id = ANY($2) AND ai.node_id <> $1
  AND n.deleted_at IS NULL
  AND NOT (n.type IN ('slack','slack_thread') AND ai.summary_kind <> 'thread_summary')
  AND n.type NOT IN ('slack_file','jira_attachment')
  AND length(TRIM(COALESCE(ai.summary,''))) >= 40
  AND NOT EXISTS (SELECT 1 FROM graph.thread_summaries tsk
    WHERE n.type IN ('slack','slack_thread')
      AND tsk.channel_id = REPLACE(n.scope,'slack:','')
      AND tsk.thread_ts = COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3))
      AND tsk.kind = 'chatter')
  AND NOT (n.type IN ('slack','slack_thread') AND n.scope LIKE 'slack:D%')`,
		nodeID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []topicLinkCandidate
	for rows.Next() {
		var c topicLinkCandidate
		if err := rows.Scan(&c.NodeID, &c.Type, &c.Summary, &c.Department, &c.Cosine); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// enqueueLinkTopicsWithCandidates queues a link_topics run that additionally
// judges the given thread roots. Deduped against any pending link_topics job
// for the node; bounded so a busy popup can't flood the judge.
func enqueueLinkTopicsWithCandidates(ctx context.Context, db *pgxpool.Pool, nodeID string, extras []string) {
	if db == nil || nodeID == "" || len(extras) == 0 {
		return
	}
	var pending bool
	_ = db.QueryRow(ctx, `
SELECT EXISTS(
  SELECT 1 FROM graph.jobs
  WHERE type='link_topics' AND status IN ('queued','running')
    AND payload->>'node_id'=$1)`, nodeID).Scan(&pending)
	if pending {
		return
	}
	if len(extras) > 12 {
		extras = extras[:12]
	}
	_, _ = jobs.Enqueue(ctx, db, "link_topics", linkTopicsPayload{
		NodeID: nodeID, ExtraCandidates: extras,
	}, jobs.EnqueueOptions{Priority: 4})
}

// nodeActivityWindow returns when a node was actually active: for Slack
// threads the first→last message time; for other artifacts first_seen→updated
// (ingest-time approximation).
func nodeActivityWindow(ctx context.Context, deps Deps, nodeID string) (start, end time.Time, ok bool) {
	err := deps.DB.QueryRow(ctx, `
WITH nd AS (
  SELECT n.type, COALESCE(n.scope,'') AS scope,
         COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3)) AS tt,
         n.first_seen_at, n.updated_at
  FROM graph.nodes n WHERE n.id = $1
)
SELECT
  CASE WHEN nd.type IN ('slack','slack_thread') THEN
    COALESCE((SELECT min(COALESCE(to_timestamp(NULLIF(m.metadata->>'ts','')::float8), m.first_seen_at))
              FROM graph.nodes m
              WHERE m.scope = nd.scope AND m.deleted_at IS NULL
                AND COALESCE(NULLIF(m.metadata->>'thread_ts',''), split_part(m.id,':',3)) = nd.tt), nd.first_seen_at)
  ELSE nd.first_seen_at END,
  CASE WHEN nd.type IN ('slack','slack_thread') THEN
    COALESCE((SELECT max(COALESCE(to_timestamp(NULLIF(m.metadata->>'ts','')::float8), m.first_seen_at))
              FROM graph.nodes m
              WHERE m.scope = nd.scope AND m.deleted_at IS NULL
                AND COALESCE(NULLIF(m.metadata->>'thread_ts',''), split_part(m.id,':',3)) = nd.tt), nd.updated_at)
  ELSE nd.updated_at END
FROM nd`, nodeID).Scan(&start, &end)
	ok = err == nil && !start.IsZero() && !end.IsZero()
	return start, end, ok
}

// timeRelation describes how two activity windows relate, plus a coarse
// bucket for the judgment cache key. The buckets mirror the thresholds in
// topic_rules.json time_affinity (7/30 days).
func timeRelation(aStart, aEnd time.Time, aOK bool, bStart, bEnd time.Time, bOK bool) (desc, bucket string) {
	if !aOK || !bOK {
		return "unknown (missing activity window)", "na"
	}
	if !aStart.After(bEnd) && !bStart.After(aEnd) {
		return "activity windows overlap", "overlap"
	}
	var gap time.Duration
	if aEnd.Before(bStart) {
		gap = bStart.Sub(aEnd)
	} else {
		gap = aStart.Sub(bEnd)
	}
	days := int(gap.Hours() / 24)
	switch {
	case days < 7:
		return fmt.Sprintf("gap of %d day(s) between activity windows", days), "lt7d"
	case days < 30:
		return fmt.Sprintf("gap of %d days between activity windows", days), "lt30d"
	default:
		return fmt.Sprintf("gap of %d days between activity windows", days), "gt30d"
	}
}

func formatWindow(start, end time.Time, ok bool) string {
	if !ok {
		return "unknown"
	}
	const day = "2006-01-02"
	if start.Format(day) == end.Format(day) {
		return start.Format(day)
	}
	return start.Format(day) + " → " + end.Format(day)
}

// materializeThreadReferences turns message-level REFERENCES edges (created
// at ingest from pasted wego.slack.com permalinks) into root→root REFERS_TO
// edges — the deterministic "raised elsewhere, citing this thread" signal.
// 100% precision, no LLM.
func materializeThreadReferences(ctx context.Context, deps Deps, rootID string) error {
	_, err := deps.DB.Exec(ctx, `
INSERT INTO graph.edges (from_node_id, to_node_id, kind, metadata, machine_id)
SELECT DISTINCT $1, root.id, 'REFERS_TO', jsonb_build_object('method','slack-permalink'), $2
FROM graph.nodes m
JOIN graph.edges e ON e.from_node_id = m.id AND e.kind = 'REFERENCES'
JOIN graph.nodes t ON t.id = e.to_node_id
  AND t.type IN ('slack','slack_thread') AND t.deleted_at IS NULL
JOIN graph.nodes root ON root.id =
  'slack:' || REPLACE(t.scope,'slack:','') || ':' ||
  COALESCE(NULLIF(t.metadata->>'thread_ts',''), split_part(t.id,':',3))
WHERE m.deleted_at IS NULL
  AND m.scope = (SELECT scope FROM graph.nodes WHERE id = $1)
  AND COALESCE(NULLIF(m.metadata->>'thread_ts',''), split_part(m.id,':',3)) = split_part($1,':',3)
  AND root.id <> $1
ON CONFLICT (from_node_id, to_node_id, kind) DO NOTHING`, rootID, deps.MachineID)
	return err
}

func cachedTopicLinkJudgment(ctx context.Context, deps Deps, from, to, contentHash string) (topicLinkJudgment, bool, error) {
	var j topicLinkJudgment
	var cachedHash string
	err := deps.DB.QueryRow(ctx, `
SELECT content_hash, same_topic, confidence, tag, topic, why
FROM graph.topic_link_judgments
WHERE source_node_id=$1 AND target_node_id=$2`,
		from, to,
	).Scan(&cachedHash, &j.SameTopic, &j.Confidence, &j.Tag, &j.Topic, &j.Why)
	if err != nil {
		return topicLinkJudgment{}, false, nil
	}
	return j, cachedHash == contentHash, nil
}

func saveTopicLinkJudgment(ctx context.Context, deps Deps, from, to, contentHash string, j topicLinkJudgment) error {
	_, err := deps.DB.Exec(ctx, `
INSERT INTO graph.topic_link_judgments
  (source_node_id, target_node_id, content_hash, same_topic, confidence, tag, topic, why, judged_at, machine_id)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NOW(),$9)
ON CONFLICT (source_node_id, target_node_id) DO UPDATE SET
  content_hash=EXCLUDED.content_hash,
  same_topic=EXCLUDED.same_topic,
  confidence=EXCLUDED.confidence,
  tag=EXCLUDED.tag,
  topic=EXCLUDED.topic,
  why=EXCLUDED.why,
  judged_at=NOW(),
  machine_id=EXCLUDED.machine_id`,
		from, to, contentHash, j.SameTopic, j.Confidence, j.Tag, j.Topic, j.Why, deps.MachineID,
	)
	return err
}

func confirmTopicLink(ctx context.Context, deps Deps, source topicLinkNode, cand topicLinkCandidate, tc topicLinkContext) (topicLinkJudgment, error) {
	sys := `You decide whether two graph artifacts are substantively about the same exact topic,
by applying the tag rules below (the same rules humans see at /live/rules).
Return JSON only: {"tag":"one tag from the list","same_topic":true|false,"confidence":0.0-1.0,"topic":"short shared topic label","why":"one short factual reason citing the rule applied"}.

` + topicRulesPromptDigest()
	var extra strings.Builder
	fmt.Fprintf(&extra, "\n\nTime relation: %s", tc.TimeDesc)
	if len(cand.SharedIDs) > 0 {
		fmt.Fprintf(&extra, "\nShared identifiers (found in BOTH artifacts' raw text): %s", strings.Join(cand.SharedIDs, ", "))
	}
	user := fmt.Sprintf(
		"Artifact A (%s, department %s, active %s):\n%s\n\nArtifact B (%s, department %s, active %s, cosine %.3f):\n%s%s",
		source.Type, blankAsUnknown(source.Department), tc.SourceWindow, source.Summary,
		cand.Type, blankAsUnknown(cand.Department), tc.CandWindow, cand.Cosine, cand.Summary,
		extra.String(),
	)
	out, err := deps.Gemini.GenerateCheap(ctx, sys, user)
	if err != nil {
		return topicLinkJudgment{}, fmt.Errorf("%w: link_topics confirm: %v", jobs.ErrTransient, err)
	}
	if strings.TrimSpace(out) == "" {
		return topicLinkJudgment{}, fmt.Errorf("%w: link_topics confirm: empty response", jobs.ErrTransient)
	}
	var parsed struct {
		Tag        string  `json:"tag"`
		SameTopic  bool    `json:"same_topic"`
		Confidence float64 `json:"confidence"`
		Topic      string  `json:"topic"`
		Why        string  `json:"why"`
	}
	if json.Unmarshal([]byte(out), &parsed) != nil {
		return topicLinkJudgment{}, fmt.Errorf("%w: link_topics confirm: invalid JSON", jobs.ErrTransient)
	}
	return topicLinkJudgment{
		SameTopic:  parsed.SameTopic,
		Confidence: clamp01(parsed.Confidence),
		Tag:        firstLine(parsed.Tag, 40),
		Topic:      firstLine(parsed.Topic, 120),
		Why:        firstLine(parsed.Why, 240),
	}, nil
}

func upsertSameTopicEdge(ctx context.Context, deps Deps, from, to string, j topicLinkJudgment, cand topicLinkCandidate, timeDesc string) error {
	method := "cosine-shortlist + llm-confirm"
	if len(cand.SharedIDs) > 0 {
		method = "shared-identifier + llm-confirm"
	}
	fields := map[string]any{
		"confidence": j.Confidence,
		"tag":        j.Tag,
		"topic":      j.Topic,
		"why":        j.Why,
		"method":     method,
		"cosine":     cand.Cosine,
		"time":       timeDesc,
	}
	if len(cand.SharedIDs) > 0 {
		fields["shared_ids"] = cand.SharedIDs
	}
	meta, _ := json.Marshal(fields)
	_, err := deps.DB.Exec(ctx, `
INSERT INTO graph.edges (from_node_id, to_node_id, kind, metadata, machine_id)
VALUES ($1,$2,'SAME_TOPIC',$3::jsonb,$4)
ON CONFLICT (from_node_id, to_node_id, kind) DO UPDATE SET
  metadata=EXCLUDED.metadata,
  machine_id=EXCLUDED.machine_id`,
		from, to, string(meta), deps.MachineID,
	)
	return err
}

func deleteSameTopicEdge(ctx context.Context, deps Deps, from, to string) error {
	_, err := deps.DB.Exec(ctx,
		`DELETE FROM graph.edges WHERE from_node_id=$1 AND to_node_id=$2 AND kind='SAME_TOPIC'`,
		from, to,
	)
	return err
}

func blankAsUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

func clamp01(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func enqueueLinkTopics(ctx context.Context, deps Deps, nodeID string, force bool) {
	if deps.DB == nil || nodeID == "" {
		return
	}
	if _, err := jobs.Enqueue(ctx, deps.DB, "link_topics", map[string]any{
		"node_id": nodeID,
		"force":   force,
	}, jobs.EnqueueOptions{Priority: 7, MachineID: deps.MachineID}); err != nil {
		deps.Logger.Warn().Err(err).Str("node_id", nodeID).Msg("enqueue link_topics failed")
	}
}

func linkTopicsForceFromIndexArtifact(_ bool) bool {
	return false
}
