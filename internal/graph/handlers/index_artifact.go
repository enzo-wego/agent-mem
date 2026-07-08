package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
	"github.com/pgvector/pgvector-go"
)

// indexArtifactPayload is the JSON payload for the index_artifact job type.
type indexArtifactPayload struct {
	NodeID string `json:"node_id"`
	Force  bool   `json:"force"`
}

// NewIndexArtifactHandler returns a HandlerInfo for the "index_artifact" job type.
func NewIndexArtifactHandler(deps Deps) jobs.Entry {
	return jobs.Entry{
		Handler:  indexArtifactHandler(deps),
		Systems:  []string{"gemini"},
		PoolSize: 4,
		Lease:    60 * time.Second,
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

		// Step 1: skip if refreshed_at is < 24h old and, for Slack thread
		// roots, not older than the cached thread summary it should embed.
		if !p.Force {
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
WHERE ai.node_id = $1`, p.NodeID,
			).Scan(&refreshedAt, &summaryUpdatedAt)
			if err == nil && refreshedAt != nil && time.Since(*refreshedAt) < 24*time.Hour &&
				(summaryUpdatedAt == nil || !summaryUpdatedAt.After(*refreshedAt)) {
				return nil // fresh enough
			}
		}

		// Step 2: read node metadata and body_full, falling back to nodes.body.
		var nodeType, scope, threadTs, ownTs, bodyFull string
		err := deps.DB.QueryRow(ctx,
			`SELECT n.type,
       COALESCE(n.scope,''),
       COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3)),
       split_part(n.id,':',3),
       COALESCE(ab.body_full, n.body, '')
FROM graph.nodes n
LEFT JOIN graph.artifact_bodies ab ON ab.node_id = n.id
WHERE n.id = $1`, p.NodeID,
		).Scan(&nodeType, &scope, &threadTs, &ownTs, &bodyFull)
		if err != nil {
			return fmt.Errorf("%w: index_artifact: node not found: %v", jobs.ErrFatal, err)
		}

		if bodyFull == "" {
			// Nothing to index yet; not an error.
			return nil
		}

		// Step 3: compute summary text. Slack thread roots prefer the cached
		// resource-aware thread summary built by summarize_thread.
		summary := heuristicSummary(p.NodeID, bodyFull)
		summaryKind := "heuristic"
		if (nodeType == "slack" || nodeType == "slack_thread") && threadTs == ownTs && strings.HasPrefix(scope, "slack:") {
			var topic, overview string
			_ = deps.DB.QueryRow(ctx,
				`SELECT COALESCE(summary,''), COALESCE(overview,'')
FROM graph.thread_summaries
WHERE channel_id=$1 AND thread_ts=$2`,
				strings.TrimPrefix(scope, "slack:"), threadTs,
			).Scan(&topic, &overview)
			if threadSummary, kind := indexSummaryForSlackRoot(topic, overview); threadSummary != "" {
				summary = threadSummary
				summaryKind = kind
			}
		}

		// Step 4: embed.
		embedding, err := deps.Gemini.EmbedWithOptions(ctx, summary, graphEmbeddingOptions())
		if err != nil {
			return fmt.Errorf("%w: index_artifact embed: %v", jobs.ErrTransient, err)
		}

		// Step 5: UPSERT graph.artifact_index.
		_, err = deps.DB.Exec(ctx, `
			INSERT INTO graph.artifact_index (node_id, summary, summary_kind, embedding, refreshed_at, machine_id)
			VALUES ($1, $2, $3, $4, NOW(), $5)
			ON CONFLICT (node_id) DO UPDATE SET
				summary      = EXCLUDED.summary,
				summary_kind = EXCLUDED.summary_kind,
				embedding    = EXCLUDED.embedding,
				refreshed_at = NOW()`,
			p.NodeID, summary, summaryKind, pgvector.NewVector(embedding), deps.MachineID,
		)
		if err != nil {
			return fmt.Errorf("index_artifact: upsert artifact_index: %w", err)
		}

		enqueueLinkTopics(ctx, deps, p.NodeID, p.Force)
		return nil
	}
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

// heuristicSummary produces a short summary based on the node type and body.
func heuristicSummary(nodeID, body string) string {
	const maxChars = 200

	// Determine type prefix.
	colonIdx := strings.Index(nodeID, ":")
	var typePrefix string
	if colonIdx > 0 {
		typePrefix = nodeID[:colonIdx]
	}

	switch typePrefix {
	case "jira":
		// Take description's first paragraph.
		return firstParagraph(body, maxChars)

	case "gh_pr":
		// title + first 3 lines.
		lines := strings.SplitN(body, "\n", 4)
		count := min(len(lines), 3)
		return truncateRunes(strings.Join(lines[:count], "\n"), maxChars)

	default:
		return firstParagraph(body, maxChars)
	}
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
