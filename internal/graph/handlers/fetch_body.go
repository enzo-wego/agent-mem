package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/extractor"
	"github.com/agent-mem/agent-mem/internal/graph/identity"
	"github.com/agent-mem/agent-mem/internal/graph/ids"
	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// fetchBodyPayload is the JSON payload for the fetch_body job type.
type fetchBodyPayload struct {
	NodeID string `json:"node_id"`
	URL    string `json:"url"`
	Source string `json:"source"`
}

// NewFetchBodyHandler returns a HandlerInfo for the "fetch_body" job type.
// It fetches, normalises, and upserts a single artifact node plus its edges.
func NewFetchBodyHandler(deps Deps) jobs.Entry {
	return jobs.Entry{
		Handler:  fetchBodyHandler(deps),
		Systems:  []string{}, // source resolved at runtime per-payload
		PoolSize: 8,
		Lease:  90 * time.Second,
	}
}

func fetchBodyHandler(deps Deps) jobs.Handler {
	return func(ctx context.Context, payload []byte) error {
		var p fetchBodyPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return fmt.Errorf("%w: fetch_body unmarshal: %v", jobs.ErrFatal, err)
		}

		// Resolve the reference: prefer node_id, fall back to url.
		ref := p.NodeID
		if ref == "" {
			ref = p.URL
		}
		if ref == "" {
			return fmt.Errorf("%w: fetch_body: both node_id and url are empty", jobs.ErrFatal)
		}

		// Step 1: find the right fetcher.
		fetcher, ok := deps.Fetchers.For(ref)
		if !ok {
			return fmt.Errorf("%w: fetch_body: no fetcher for %q", jobs.ErrFatal, ref)
		}

		// Step 2: fetch.
		body, err := fetcher.Fetch(ctx, ref)
		if err != nil {
			return fmt.Errorf("%w: fetch_body fetch: %v", jobs.ErrTransient, err)
		}

		// Step 3: normalise.
		norm, normOK := deps.Normalizers.For(fetcher.Source())
		var plainText string
		var mentions []string
		if normOK {
			result, err := norm.Normalize(ctx, body.Raw, body.Metadata)
			if err != nil {
				deps.Logger.Warn().Err(err).Str("node_id", body.NodeID).Msg("fetch_body: normalizer error; using empty text")
			} else {
				plainText = result.Text
				for _, m := range result.Mentions {
					mentions = append(mentions, m.ExternalID)
				}
			}
		} else {
			deps.Logger.Warn().Str("source", fetcher.Source()).Msg("fetch_body: no normalizer found; using raw bytes as text")
			plainText = string(body.Raw)
		}

		// Step 4: resolve author.
		var authorPersonID *int64
		if body.Author.ExternalID != "" {
			pid, _, err := deps.Identity.EnsurePerson(ctx, identity.Ref{
				Source:      body.Author.Source,
				ExternalID:  body.Author.ExternalID,
				DisplayName: body.Author.DisplayName,
				Email:       body.Author.Email,
				IsBot:       body.Author.IsBot,
			})
			if err != nil {
				deps.Logger.Warn().Err(err).Msg("fetch_body: EnsurePerson failed; proceeding without author")
			} else {
				authorPersonID = &pid
				// If email is present, attempt a merge to deduplicate.
				if body.Author.Email != "" {
					if _, mergeErr := deps.Identity.MergeByEmail(ctx, body.Author.Email); mergeErr != nil {
						deps.Logger.Warn().Err(mergeErr).Str("email", body.Author.Email).Msg("fetch_body: MergeByEmail failed")
					}
				}
			}
		}

		// Step 5: derive natural_key and scope.
		naturalKey, _ := ids.ParseNaturalKey(body.NodeID)
		scope := deriveScope(fetcher.Source(), body.Metadata)

		// Build metadata JSON.
		var metaJSON []byte
		if body.Metadata != nil {
			metaJSON, err = json.Marshal(body.Metadata)
			if err != nil {
				metaJSON = nil
			}
		}

		// Upsert graph.nodes — never overwrite newer body_ts.
		_, err = deps.DB.Exec(ctx, `
			INSERT INTO graph.nodes
				(id, type, natural_key, url, title, body, body_revision, body_ts,
				 author_person_id, scope, metadata, updated_at, machine_id)
			VALUES
				($1, $2, $3, $4, $5, $6, 1, $7,
				 $8, $9, $10, NOW(), $11)
			ON CONFLICT (id) DO UPDATE SET
				url              = EXCLUDED.url,
				title            = EXCLUDED.title,
				body             = EXCLUDED.body,
				body_revision    = graph.nodes.body_revision + 1,
				body_ts          = EXCLUDED.body_ts,
				author_person_id = COALESCE(EXCLUDED.author_person_id, graph.nodes.author_person_id),
				scope            = EXCLUDED.scope,
				metadata         = EXCLUDED.metadata,
				updated_at       = NOW(),
				machine_id       = EXCLUDED.machine_id
			WHERE EXCLUDED.body_ts >= graph.nodes.body_ts`,
			body.NodeID,
			string(body.Type),
			naturalKey,
			body.URL,
			body.Title,
			plainText,
			body.BodyTS,
			authorPersonID,
			scope,
			metaJSON,
			deps.MachineID,
		)
		if err != nil {
			return fmt.Errorf("fetch_body: upsert node: %w", err)
		}

		// Step 6: upsert graph.artifact_bodies.
		_, err = deps.DB.Exec(ctx, `
			INSERT INTO graph.artifact_bodies (node_id, body_full, fetched_at)
			VALUES ($1, $2, NOW())
			ON CONFLICT (node_id) DO UPDATE SET
				body_full  = EXCLUDED.body_full,
				fetched_at = NOW()`,
			body.NodeID, plainText,
		)
		if err != nil {
			return fmt.Errorf("fetch_body: upsert artifact_bodies: %w", err)
		}

		// Step 7: reconcile edges from extractor findings.
		extractResult, err := deps.Extractor.Extract(ctx, plainText)
		if err != nil {
			deps.Logger.Warn().Err(err).Msg("fetch_body: extractor failed; skipping edge reconciliation")
		} else {
			upsertedEdgeIDs, edgeErr := reconcileEdges(ctx, deps, body.NodeID, extractResult.Findings)
			if edgeErr != nil {
				deps.Logger.Warn().Err(edgeErr).Msg("fetch_body: reconcileEdges failed")
			} else {
				// Delete stale edges that no longer appear in the body.
				if err := pruneStaleEdges(ctx, deps, body.NodeID, upsertedEdgeIDs); err != nil {
					deps.Logger.Warn().Err(err).Msg("fetch_body: pruneStaleEdges failed")
				}
			}

			// Enqueue fetch_body for target nodes with empty body.
			for _, f := range extractResult.Findings {
				enqueueFetchIfEmpty(ctx, deps, f.NodeID, f.Type)
			}
		}

		// Step 8: attachment edges + describe jobs.
		for _, att := range body.Attachments {
			attNK, _ := ids.ParseNaturalKey(att.NodeID)
			attType, _ := ids.ParseType(att.NodeID)
			_, uErr := deps.DB.Exec(ctx, `
				INSERT INTO graph.nodes (id, type, natural_key, url, updated_at, machine_id)
				VALUES ($1, $2, $3, $4, NOW(), $5)
				ON CONFLICT (id) DO NOTHING`,
				att.NodeID, string(attType), attNK, att.URLPrivate, deps.MachineID,
			)
			if uErr != nil {
				deps.Logger.Warn().Err(uErr).Str("att_node_id", att.NodeID).Msg("fetch_body: upsert attachment node failed")
				continue
			}
			_, uErr = deps.DB.Exec(ctx, `
				INSERT INTO graph.edges (from_node_id, to_node_id, kind, source_msg_id, updated_at)
				VALUES ($1, $2, 'REFERENCES', $3, NOW())
				ON CONFLICT (from_node_id, to_node_id, kind) DO UPDATE SET
					source_msg_id = EXCLUDED.source_msg_id,
					updated_at    = NOW()`,
				body.NodeID, att.NodeID, body.NodeID,
			)
			if uErr != nil {
				deps.Logger.Warn().Err(uErr).Str("att_node_id", att.NodeID).Msg("fetch_body: upsert attachment edge failed")
			}

			// Enqueue describe_attachment.
			descPayload := map[string]string{
				"node_id":      att.NodeID,
				"external_url": att.URLPrivate,
				"mime":         att.MimeType,
				"source":       fetcher.Source(),
			}
			if _, jErr := jobs.Enqueue(ctx, deps.DB, "describe_attachment", descPayload, jobs.EnqueueOptions{
				Priority:  5,
				MachineID: deps.MachineID,
			}); jErr != nil {
				deps.Logger.Warn().Err(jErr).Str("att_node_id", att.NodeID).Msg("fetch_body: enqueue describe_attachment failed")
			}
		}

		// Step 9: enqueue index_artifact.
		if _, jErr := jobs.Enqueue(ctx, deps.DB, "index_artifact", map[string]any{
			"node_id": body.NodeID,
			"force":   false,
		}, jobs.EnqueueOptions{
			Priority:  5,
			MachineID: deps.MachineID,
		}); jErr != nil {
			deps.Logger.Warn().Err(jErr).Str("node_id", body.NodeID).Msg("fetch_body: enqueue index_artifact failed")
		}

		// Suppress unused variable warning for mentions (used for future
		// explicit mention edges; currently just extracted via extractor too).
		_ = mentions

		return nil
	}
}

// deriveScope builds the scope string from the source and metadata.
func deriveScope(source string, meta map[string]any) string {
	switch source {
	case "slack":
		if ch, ok := meta["channel_id"].(string); ok && ch != "" {
			return "slack:" + ch
		}
	case "jira":
		if proj, ok := meta["project_key"].(string); ok && proj != "" {
			return "jira:" + proj
		}
	case "github":
		if repo, ok := meta["repo"].(string); ok && repo != "" {
			return "github:" + repo
		}
	case "confluence":
		if space, ok := meta["space_key"].(string); ok && space != "" {
			return "confluence:" + space
		}
	}
	return source
}

// reconcileEdges upserts edge rows for each Finding and returns their IDs.
func reconcileEdges(ctx context.Context, deps Deps, fromNodeID string, findings []extractor.Finding) ([]int64, error) {
	var edgeIDs []int64
	for _, f := range findings {
		// Upsert target node stub if it doesn't exist.
		naturalKey, _ := ids.ParseNaturalKey(f.NodeID)
		_, err := deps.DB.Exec(ctx, `
			INSERT INTO graph.nodes (id, type, natural_key, updated_at, machine_id)
			VALUES ($1, $2, $3, NOW(), $4)
			ON CONFLICT (id) DO NOTHING`,
			f.NodeID, string(f.Type), naturalKey, deps.MachineID,
		)
		if err != nil {
			deps.Logger.Warn().Err(err).Str("target_node_id", f.NodeID).Msg("reconcileEdges: upsert target node failed")
			continue
		}

		// Upsert edge.
		var edgeID int64
		err = deps.DB.QueryRow(ctx, `
			INSERT INTO graph.edges (from_node_id, to_node_id, kind, source_msg_id, updated_at)
			VALUES ($1, $2, $3, $4, NOW())
			ON CONFLICT (from_node_id, to_node_id, kind) DO UPDATE SET
				source_msg_id = EXCLUDED.source_msg_id,
				updated_at    = NOW()
			RETURNING id`,
			fromNodeID, f.NodeID, f.EdgeKind, fromNodeID,
		).Scan(&edgeID)
		if err != nil {
			deps.Logger.Warn().Err(err).Str("to_node_id", f.NodeID).Msg("reconcileEdges: upsert edge failed")
			continue
		}
		edgeIDs = append(edgeIDs, edgeID)
	}
	return edgeIDs, nil
}

// pruneStaleEdges removes edges from fromNodeID that are no longer in the current body.
func pruneStaleEdges(ctx context.Context, deps Deps, fromNodeID string, keepIDs []int64) error {
	if len(keepIDs) == 0 {
		_, err := deps.DB.Exec(ctx, `
			DELETE FROM graph.edges WHERE from_node_id = $1 AND source_msg_id = $1`,
			fromNodeID,
		)
		return err
	}
	_, err := deps.DB.Exec(ctx, `
		DELETE FROM graph.edges
		WHERE from_node_id = $1
		  AND source_msg_id = $1
		  AND id != ALL($2)`,
		fromNodeID, keepIDs,
	)
	return err
}

// enqueueFetchIfEmpty enqueues a fetch_body job for a target node if its body is empty.
func enqueueFetchIfEmpty(ctx context.Context, deps Deps, nodeID string, nodeType ids.NodeType) {
	var bodyVal *string
	err := deps.DB.QueryRow(ctx,
		`SELECT body FROM graph.nodes WHERE id = $1`, nodeID,
	).Scan(&bodyVal)
	if err != nil {
		return // node doesn't exist yet or query failed — skip
	}
	if bodyVal != nil && *bodyVal != "" {
		return // already has content
	}
	if _, jErr := jobs.Enqueue(ctx, deps.DB, "fetch_body", map[string]string{
		"node_id": nodeID,
	}, jobs.EnqueueOptions{
		Priority:  5,
		MachineID: deps.MachineID,
	}); jErr != nil {
		deps.Logger.Warn().Err(jErr).Str("node_id", nodeID).Msg("enqueueFetchIfEmpty: enqueue failed")
	}
}
