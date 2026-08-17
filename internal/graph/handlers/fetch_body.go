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

		// Slack messages have no real title; the fetcher fills body.Title with the
		// raw message text (un-normalized <@U…> mentions). Use a snippet of the
		// NORMALIZED body instead so titles/labels never show raw ids.
		title := body.Title
		if fetcher.Source() == "slack" {
			title = firstLine(plainText, 120)
		}

		// Step 5: derive natural_key and scope.
		naturalKey, _ := ids.ParseNaturalKey(body.NodeID)
		scope := deriveScope(fetcher.Source(), body.Metadata)

		// Build metadata JSON. graph.nodes.metadata is NOT NULL, so default to
		// an empty object — sources that don't populate Metadata (jira, confluence,
		// github, …) would otherwise pass an explicit NULL and fail the upsert.
		metaJSON := []byte("{}")
		if body.Metadata != nil {
			if b, mErr := json.Marshal(body.Metadata); mErr == nil {
				metaJSON = b
			}
		}

		// Canonical created_at = the artifact's reported created time (fall back to
		// its body_ts/updated time, which is still a real source time, when absent).
		createdAt := body.CreatedAt
		if createdAt.IsZero() {
			createdAt = body.BodyTS
		}

		// Upsert graph.nodes — never overwrite newer body_ts.
		_, err = deps.DB.Exec(ctx, `
			INSERT INTO graph.nodes
				(id, type, natural_key, url, title, body, body_revision, body_ts,
				 created_at, author_person_id, scope, metadata, updated_at, machine_id)
			VALUES
				($1, $2, $3, $4, $5, $6, 1, $7,
				 $8, $9, $10, $11, NOW(), $12)
			ON CONFLICT (id) DO UPDATE SET
				url              = EXCLUDED.url,
				title            = EXCLUDED.title,
				body             = EXCLUDED.body,
				body_revision    = graph.nodes.body_revision + 1,
				body_ts          = EXCLUDED.body_ts,
				created_at       = COALESCE(graph.nodes.created_at, EXCLUDED.created_at),
				author_person_id = COALESCE(EXCLUDED.author_person_id, graph.nodes.author_person_id),
				scope            = EXCLUDED.scope,
				metadata         = EXCLUDED.metadata,
				updated_at       = NOW(),
				machine_id       = EXCLUDED.machine_id
			WHERE graph.nodes.body_ts IS NULL OR EXCLUDED.body_ts >= graph.nodes.body_ts`,
			body.NodeID,
			string(body.Type),
			naturalKey,
			body.URL,
			title,
			plainText,
			body.BodyTS,
			createdAt,
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
			INSERT INTO graph.artifact_bodies (node_id, body_full, fetched_at, machine_id)
			VALUES ($1, $2, NOW(), $3)
			ON CONFLICT (node_id) DO UPDATE SET
				body_full  = EXCLUDED.body_full,
				fetched_at = NOW()`,
			body.NodeID, plainText, deps.MachineID,
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

		// A thread's summary names its linked resources, so refresh the Slack
		// thread(s) whose resource links involve this node — this message gaining a
		// reference, or (when this node is a resource) its title landing for threads
		// that reference it. Capped and deduped; the summary handler decides whether
		// the links actually changed, so an unchanged title costs no LLM call.
		refreshThreadsForResourceLink(ctx, deps, body.NodeID)

		// Step 8: attachment edges + describe jobs.
		for _, att := range body.Attachments {
			attNK, _ := ids.ParseNaturalKey(att.NodeID)
			attType, _ := ids.ParseType(att.NodeID)
			// Title = filename so the dashboard shows "invoice.png", never a raw
			// node id. Keep-if-present: describe_attachment owns body, not title,
			// so an existing (possibly hand-fixed) title is never clobbered.
			_, uErr := deps.DB.Exec(ctx, `
				INSERT INTO graph.nodes (id, type, natural_key, url, title, updated_at, machine_id)
				VALUES ($1, $2, $3, $4, $5, NOW(), $6)
				ON CONFLICT (id) DO UPDATE SET
					title = COALESCE(NULLIF(graph.nodes.title,''), EXCLUDED.title),
					url   = EXCLUDED.url`,
				att.NodeID, string(attType), attNK, att.URLPrivate, att.Filename, deps.MachineID,
			)
			if uErr != nil {
				deps.Logger.Warn().Err(uErr).Str("att_node_id", att.NodeID).Msg("fetch_body: upsert attachment node failed")
				continue
			}
			_, uErr = deps.DB.Exec(ctx, `
				INSERT INTO graph.edges (from_node_id, to_node_id, kind, source_msg_id, machine_id)
				VALUES ($1, $2, 'REFERENCES', $3, $4)
				ON CONFLICT (from_node_id, to_node_id, kind) DO UPDATE SET
					source_msg_id = EXCLUDED.source_msg_id`,
				body.NodeID, att.NodeID, body.NodeID, deps.MachineID,
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

// refreshThreadsForResourceLinkCap bounds how many threads one fetched node may
// re-enqueue. A doc linked from hundreds of threads would otherwise fan one fetch
// out into hundreds of jobs, and fetch_body enqueues further fetch_body jobs for
// the links it finds, so each wave seeds the next.
//
// ponytail: flat cap, and threads past it keep a stale link title until their own
// messages change. Batch the refresh by resource instead if that ever bites.
const refreshThreadsForResourceLinkCap = 50

// refreshThreadsForResourceLink re-enqueues summarize_thread for every Slack
// thread whose resource graph involves nodeID: the Slack messages that reference
// this node (when it is a resource whose title just landed), and this node itself
// when it is a Slack message that references a non-Slack resource. Best-effort;
// errors are ignored.
//
// Enqueue is plain and deduped — no force. summarize_thread hashes the linked
// resource titles into link_signature, so a thread whose links genuinely changed
// regenerates and one whose links are byte-identical costs a cheap SELECT and no
// LLM call. Forcing here is what turned every fetch into 1,335 calls/hour for 3
// real updates; do not reintroduce it to "make sure" a refresh lands.
func refreshThreadsForResourceLink(ctx context.Context, deps Deps, nodeID string) {
	rows, err := deps.DB.Query(ctx, `
SELECT DISTINCT replace(sn.scope,'slack:',''),
       COALESCE(NULLIF(sn.metadata->>'thread_ts',''), split_part(sn.id,':',3))
FROM graph.nodes sn
WHERE sn.type IN ('slack','slack_thread') AND sn.scope LIKE 'slack:%' AND sn.deleted_at IS NULL
  AND sn.id IN (
      SELECT e.from_node_id FROM graph.edges e WHERE e.to_node_id=$1 AND e.kind='REFERENCES'
      UNION
      SELECT e.from_node_id FROM graph.edges e
        JOIN graph.nodes r ON r.id=e.to_node_id
       WHERE e.from_node_id=$1 AND e.kind='REFERENCES'
         AND r.type NOT IN ('slack','slack_thread','slack_file'))
LIMIT $2`, nodeID, refreshThreadsForResourceLinkCap+1) // +1 to detect truncation
	if err != nil {
		return
	}
	type ct struct{ channel, thread string }
	var threads []ct
	for rows.Next() {
		var c, t string
		if rows.Scan(&c, &t) == nil && c != "" && t != "" {
			threads = append(threads, ct{c, t})
		}
	}
	rows.Close()
	// Say so when the cap bites, rather than silently looking like full coverage.
	if len(threads) > refreshThreadsForResourceLinkCap {
		threads = threads[:refreshThreadsForResourceLinkCap]
		deps.Logger.Warn().Str("node_id", nodeID).Int("cap", refreshThreadsForResourceLinkCap).
			Msg("fetch_body: resource-link refresh capped; remaining threads keep their link titles until their messages change")
	}
	for _, th := range threads {
		enqueueSummarizeThread(ctx, deps.DB, th.channel, th.thread, false)
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
	case "wegohub":
		// Wego Hub is internal-public: any signed-in @wego.com user can read
		// every published file. The acl builder grants "public" to all askers.
		return "public"
	case "claude_artifact":
		// Shared Claude artifacts are link-accessible; treat as public so any
		// asker who has it ingested can retrieve it.
		return "public"
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
			INSERT INTO graph.edges (from_node_id, to_node_id, kind, source_msg_id, machine_id)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (from_node_id, to_node_id, kind) DO UPDATE SET
				source_msg_id = EXCLUDED.source_msg_id
			RETURNING id`,
			fromNodeID, f.NodeID, f.EdgeKind, fromNodeID, deps.MachineID,
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
