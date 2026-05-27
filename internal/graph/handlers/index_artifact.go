package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// indexArtifactPayload is the JSON payload for the index_artifact job type.
type indexArtifactPayload struct {
	NodeID string `json:"node_id"`
	Force  bool   `json:"force"`
}

// NewIndexArtifactHandler returns a HandlerInfo for the "index_artifact" job type.
func NewIndexArtifactHandler(deps Deps) jobs.HandlerInfo {
	return jobs.HandlerInfo{
		Handler:  indexArtifactHandler(deps),
		Systems:  []string{"gemini"},
		PoolSize: 4,
		Timeout:  60 * time.Second,
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

		// Step 1: skip if refreshed_at is < 24h old and not forced.
		if !p.Force {
			var refreshedAt *time.Time
			err := deps.DB.QueryRow(ctx,
				`SELECT refreshed_at FROM graph.artifact_index WHERE node_id = $1`, p.NodeID,
			).Scan(&refreshedAt)
			if err == nil && refreshedAt != nil && time.Since(*refreshedAt) < 24*time.Hour {
				return nil // fresh enough
			}
		}

		// Step 2: read body_full from artifact_bodies, fall back to nodes.body.
		var bodyFull string
		err := deps.DB.QueryRow(ctx,
			`SELECT body_full FROM graph.artifact_bodies WHERE node_id = $1`, p.NodeID,
		).Scan(&bodyFull)
		if err != nil {
			// Fall back to graph.nodes.body.
			var nodeBody *string
			err2 := deps.DB.QueryRow(ctx,
				`SELECT body FROM graph.nodes WHERE id = $1`, p.NodeID,
			).Scan(&nodeBody)
			if err2 != nil {
				return fmt.Errorf("%w: index_artifact: node not found: %v", jobs.ErrFatal, err2)
			}
			if nodeBody != nil {
				bodyFull = *nodeBody
			}
		}

		if bodyFull == "" {
			// Nothing to index yet; not an error.
			return nil
		}

		// Step 3: compute heuristic summary based on node type prefix.
		summary := heuristicSummary(p.NodeID, bodyFull)

		// Step 4: embed.
		embedding, err := deps.Gemini.Embed(ctx, summary)
		if err != nil {
			return fmt.Errorf("%w: index_artifact embed: %v", jobs.ErrTransient, err)
		}

		// Step 5: UPSERT graph.artifact_index.
		_, err = deps.DB.Exec(ctx, `
			INSERT INTO graph.artifact_index (node_id, summary, summary_kind, embedding, refreshed_at)
			VALUES ($1, $2, 'heuristic', $3, NOW())
			ON CONFLICT (node_id) DO UPDATE SET
				summary      = EXCLUDED.summary,
				summary_kind = EXCLUDED.summary_kind,
				embedding    = EXCLUDED.embedding,
				refreshed_at = NOW()`,
			p.NodeID, summary, embedding,
		)
		if err != nil {
			return fmt.Errorf("index_artifact: upsert artifact_index: %w", err)
		}

		return nil
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
		count := 3
		if len(lines) < count {
			count = len(lines)
		}
		result := strings.Join(lines[:count], "\n")
		if len(result) > maxChars {
			return result[:maxChars]
		}
		return result

	default:
		return firstParagraph(body, maxChars)
	}
}

// firstParagraph returns the first paragraph of text, capped at maxChars.
func firstParagraph(body string, maxChars int) string {
	// Split on blank line (paragraph break).
	idx := strings.Index(body, "\n\n")
	var para string
	if idx >= 0 {
		para = strings.TrimSpace(body[:idx])
	} else {
		para = strings.TrimSpace(body)
	}
	if len(para) > maxChars {
		return para[:maxChars]
	}
	return para
}
