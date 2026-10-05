package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"
)

// indexArtifactPayload is the JSON payload for the index_artifact job type.
type indexArtifactPayload struct {
	NodeID      string `json:"node_id"`
	Force       bool   `json:"force"`
	SkipJudging bool   `json:"skip_judging,omitempty"`
}

// NewIndexArtifactHandler returns a HandlerInfo for the "index_artifact" job type.
func NewIndexArtifactHandler(deps Deps) jobs.Entry {
	return jobs.Entry{
		Handler:  indexArtifactHandler(deps),
		Systems:  []string{"gemini"},
		PoolSize: 4,
		Lease:    60 * time.Second,
		UsesLLM:  true,
	}
}

func indexArtifactHandler(deps Deps) jobs.Handler {
	return func(ctx context.Context, payload []byte) error {
		var p indexArtifactPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return fmt.Errorf("%w: index_artifact unmarshal: %v", jobs.ErrFatal, err)
		}
		if p.NodeID == "" {
			return fmt.Errorf("%w: index_artifact: node_id is required", jobs.ErrFatal)
		}
		return indexArtifactNode(ctx, deps, p.NodeID, p.Force, p.SkipJudging)
	}
}

// artifactEmbedError distinguishes skippable provider failures from indexing
// failures while retaining the queue's transient-error classification.
type artifactEmbedError struct {
	err error
}

func (e *artifactEmbedError) Error() string        { return "index_artifact embed: " + e.err.Error() }
func (e *artifactEmbedError) Unwrap() error        { return e.err }
func (e *artifactEmbedError) Is(target error) bool { return target == jobs.ErrTransient }

func indexArtifactNode(ctx context.Context, deps Deps, nodeID string, force, skipJudging bool) error {
	// Step 1: skip if refreshed_at is < 24h old and, for Slack thread
	// roots, not older than the cached thread summary it should embed.
	if !force {
		var refreshedAt *time.Time
		var summaryUpdatedAt *time.Time
		err := deps.DB.QueryRow(ctx,
			`SELECT ai.refreshed_at, ts.updated_at
FROM graph.artifact_index ai
JOIN graph.nodes n ON n.id = ai.node_id
LEFT JOIN graph.thread_summaries ts
  ON n.type IN ('slack','slack_thread')
  AND ts.channel_id = REPLACE(n.scope,'slack:','')
  AND ts.thread_ts = COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3))
  AND COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3)) = split_part(n.id,':',3)
WHERE ai.node_id = $1`, nodeID,
		).Scan(&refreshedAt, &summaryUpdatedAt)
		if err == nil && refreshedAt != nil && time.Since(*refreshedAt) < 24*time.Hour &&
			(summaryUpdatedAt == nil || !summaryUpdatedAt.After(*refreshedAt)) {
			return nil // fresh enough
		}
	}

	// Step 2: read node metadata and body_full, falling back to nodes.body.
	var nodeType, scope, threadTs, ownTs, title, bodyFull string
	err := deps.DB.QueryRow(ctx,
		`SELECT n.type,
       COALESCE(n.scope,''),
       COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3)),
       split_part(n.id,':',3),
       COALESCE(n.title,''),
       COALESCE(ab.body_full, n.body, '')
FROM graph.nodes n
LEFT JOIN graph.artifact_bodies ab ON ab.node_id = n.id
WHERE n.id = $1`, nodeID,
	).Scan(&nodeType, &scope, &threadTs, &ownTs, &title, &bodyFull)
	if err != nil {
		return fmt.Errorf("%w: index_artifact: node not found: %v", jobs.ErrFatal, err)
	}

	if bodyFull == "" && (nodeType == "slack" || nodeType == "slack_thread" || strings.TrimSpace(title) == "") {
		// Nothing to index yet; not an error.
		return nil
	}

	// Step 3: compute summary text. Slack thread roots prefer the cached
	// resource-aware thread summary built by summarize_thread.
	summary := heuristicSummary(nodeID, title, bodyFull)
	if summary == "" && nodeType != "slack" && nodeType != "slack_thread" {
		return nil
	}
	summaryKind := "heuristic"
	var decisionsText *string // NULL unless this node embeds a thread summary
	if (nodeType == "slack" || nodeType == "slack_thread") && threadTs == ownTs && strings.HasPrefix(scope, "slack:") {
		var topic, overview string
		var decRaw []byte
		_ = deps.DB.QueryRow(ctx,
			`SELECT COALESCE(summary,''), COALESCE(overview,''), decisions
FROM graph.thread_summaries
WHERE channel_id=$1 AND thread_ts=$2`,
			strings.TrimPrefix(scope, "slack:"), threadTs,
		).Scan(&topic, &overview, &decRaw)
		if threadSummary, kind := indexSummaryForSlackRoot(topic, overview); threadSummary != "" {
			summary = threadSummary
			summaryKind = kind
			var decisions []threadDecision
			if len(decRaw) > 0 {
				_ = json.Unmarshal(decRaw, &decisions) // NULL or bad JSON: no decisions
			}
			dt := decisionsBlock(decisions)
			decisionsText = &dt
		}
	}

	// Step 4: extract identifiers from RAW text (thread roots read the
	// whole thread) — summaries drop the IDs that shared-identifier
	// candidates depend on.
	identifiers, err := identifiersForNode(ctx, deps, nodeID, nodeType, scope, threadTs, ownTs, bodyFull)
	if err != nil {
		return fmt.Errorf("index_artifact: extract identifiers: %w", err)
	}
	if identifiers == nil {
		identifiers = []string{}
	}

	// Step 5: identical heuristic summaries share one indexed
	// representative. The transaction-scoped advisory lock serializes the
	// check through the upsert across the handler's worker pool.
	var indexTx pgx.Tx
	skipEmbedding := false
	if summaryKind == "heuristic" {
		indexTx, err = deps.DB.Begin(ctx)
		if err != nil {
			return fmt.Errorf("index_artifact: begin heuristic dedup transaction: %w", err)
		}
		defer func() {
			_ = indexTx.Rollback(context.Background())
		}()
		if _, err = indexTx.Exec(ctx,
			`SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))`,
			summaryKind, summary); err != nil {
			return fmt.Errorf("index_artifact: lock heuristic summary: %w", err)
		}
		if err = indexTx.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1
  FROM graph.artifact_index
  WHERE summary = $1
    AND summary_kind = $2
    AND embedding IS NOT NULL
    AND node_id <> $3
)`, summary, summaryKind, nodeID).Scan(&skipEmbedding); err != nil {
			return fmt.Errorf("index_artifact: check duplicate heuristic summary: %w", err)
		}
	}

	embedInput := summary
	if decisionsText != nil && *decisionsText != "" {
		embedInput += indexDecisionsMarker + *decisionsText
	}
	var embedding any
	if !skipEmbedding {
		vector, err := deps.Gemini.EmbedWithOptions(ctx, embedInput, graphEmbeddingOptions())
		if err != nil {
			return &artifactEmbedError{err: err}
		}
		embedding = pgvector.NewVector(vector)
	}

	// Step 6: UPSERT graph.artifact_index.
	const upsertSQL = `
			INSERT INTO graph.artifact_index (node_id, summary, summary_kind, embedding, identifiers, refreshed_at, machine_id, decisions_text)
			VALUES ($1, $2, $3, $4, $5, NOW(), $6, $7)
			ON CONFLICT (node_id) DO UPDATE SET
				summary      = EXCLUDED.summary,
				summary_kind = EXCLUDED.summary_kind,
				embedding    = EXCLUDED.embedding,
				identifiers  = EXCLUDED.identifiers,
				decisions_text = EXCLUDED.decisions_text,
				refreshed_at = NOW()`
	if indexTx != nil {
		_, err = indexTx.Exec(ctx, upsertSQL,
			nodeID, summary, summaryKind, embedding, identifiers, deps.MachineID, decisionsText)
	} else {
		_, err = deps.DB.Exec(ctx, upsertSQL,
			nodeID, summary, summaryKind, embedding, identifiers, deps.MachineID, decisionsText)
	}
	if err != nil {
		return fmt.Errorf("index_artifact: upsert artifact_index: %w", err)
	}
	if indexTx != nil {
		if err = indexTx.Commit(ctx); err != nil {
			return fmt.Errorf("index_artifact: commit heuristic dedup transaction: %w", err)
		}
	}

	// Only thread roots (embedding their resource-aware summary) and
	// non-Slack resources link out — never raw-text Slack messages.
	if (nodeType != "slack" && nodeType != "slack_thread") || summaryKind == "thread_summary" {
		enqueueLinkTopics(ctx, deps, nodeID, linkTopicsForceFromIndexArtifact(force), skipJudging)
	}
	return nil
}

// indexDecisionsMarker joins a thread root's summary and its decisions block in
// the embedding input. Only the embedding sees it: the stored summary has none.
const indexDecisionsMarker = "\n\nDecisions:\n"

// The decisions block holds the latest indexMaxDecisions decisions, each cut
// to indexDecisionRunes runes: at most 8*202 + 7 = 1,623 runes.
const indexMaxDecisions = 8

const indexDecisionRunes = 200

func decisionsBlock(ds []threadDecision) string {
	lines := make([]string, 0, indexMaxDecisions)
	for _, d := range ds {
		t := truncateRunes(strings.Join(strings.Fields(d.Text), " "), indexDecisionRunes)
		if t == "" {
			continue
		}
		if len(lines) == indexMaxDecisions {
			copy(lines, lines[1:])
			lines[len(lines)-1] = t
		} else {
			lines = append(lines, t)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "- " + strings.Join(lines, "\n- ")
}

func indexSummaryForSlackRoot(topic, overview string) (summary, kind string) {
	topic = strings.TrimSpace(topic)
	overview = strings.TrimSpace(overview)
	switch {
	case topic != "" && overview != "":
		return topic + "\n\n" + overview, "thread_summary"
	case topic != "":
		return topic, "thread_summary"
	case overview != "":
		return overview, "thread_summary"
	default:
		return "", ""
	}
}

var reSummaryHeading = regexp.MustCompile(`^#{1,6}\s`)

// heuristicSummary keeps Slack's first paragraph; resources include their title.
func heuristicSummary(nodeID, title, body string) string {
	if strings.HasPrefix(nodeID, "slack:") || strings.HasPrefix(nodeID, "slack_thread:") {
		return firstParagraph(body, 200)
	}
	title = strings.TrimSpace(title)
	normalizeTitle := func(s string) string {
		return strings.ToLower(strings.Join(strings.Fields(strings.TrimLeft(strings.TrimSpace(s), "#")), " "))
	}
	// ponytail: label list + markdown heading strip; Jira bodies keep section headings since R1
	var kept []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || reSummaryHeading.MatchString(line) {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(strings.TrimRight(line, ":?"))) {
		case "background", "context", "problem", "problem statement", "description", "summary",
			"goal", "goals", "overview", "what", "why", "how", "scope", "out of scope",
			"acceptance criteria", "notes", "details", "requirements", "solution", "proposal",
			"testing", "test plan", "changes", "tasks", "references", "links":
			continue
		}
		kept = append(kept, line)
	}
	if len(kept) > 0 && normalizeTitle(kept[0]) == normalizeTitle(title) {
		kept = kept[1:]
	}
	if strings.HasPrefix(nodeID, "gh_pr:") && len(kept) > 3 {
		kept = kept[:3]
	}
	summary := title
	if len(kept) > 0 {
		if summary != "" {
			summary += "\n"
		}
		summary += strings.Join(kept, " ")
	}
	if summary == "" {
		summary = strings.TrimSpace(body)
	}
	return truncateRunes(summary, 400)
}

// truncateRunes caps s to at most n runes, never splitting a multi-byte
// UTF-8 character (a byte slice would, producing invalid UTF-8 that Postgres
// rejects with SQLSTATE 22021).
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// firstParagraph returns the first paragraph of text, capped at maxChars.
func firstParagraph(body string, maxChars int) string {
	// Split on blank line (paragraph break).
	first, _, _ := strings.Cut(body, "\n\n")
	return truncateRunes(strings.TrimSpace(first), maxChars)
}
