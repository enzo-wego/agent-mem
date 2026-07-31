package handlers

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/ids"
	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// deriveFeatureEntityPayload is the JSON payload for the derive_feature_entity job type.
type deriveFeatureEntityPayload struct {
	NodeID string `json:"node_id"`
}

// deriveFeatureResult is the JSON shape Gemini returns for feature derivation.
type deriveFeatureResult struct {
	IsFeature bool     `json:"is_feature"`
	Name      string   `json:"name"`
	Aliases   []string `json:"aliases"`
}

// NewDeriveFeatureEntityHandler returns the job entry for "derive_feature_entity":
// it inspects a Jira node and, if the ticket defines a product feature, derives a
// "feature:" entity (LLM-extracted name + aliases) so the extractor can auto-link
// Slack messages that mention the feature.
func NewDeriveFeatureEntityHandler(deps Deps) jobs.Entry {
	return jobs.Entry{
		Handler:  deriveFeatureEntityHandler(deps),
		Systems:  []string{"gemini"},
		PoolSize: 2,
		Lease:    SummaryLease, // reaches TextGenerator; see SummaryLease
	}
}

func deriveFeatureEntityHandler(deps Deps) jobs.Handler {
	return func(ctx context.Context, payload []byte) error {
		var p deriveFeatureEntityPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return fmt.Errorf("%w: derive_feature_entity unmarshal: %v", jobs.ErrFatal, err)
		}
		if p.NodeID == "" {
			return fmt.Errorf("%w: derive_feature_entity: node_id required", jobs.ErrFatal)
		}
		if deps.Gemini == nil {
			return nil // no LLM configured
		}

		// Load the jira node.
		var title, body, naturalKey string
		err := deps.DB.QueryRow(ctx, `
SELECT COALESCE(title,''), LEFT(COALESCE(body,''),2000), natural_key
FROM graph.nodes
WHERE id=$1 AND type='jira' AND deleted_at IS NULL`,
			p.NodeID).Scan(&title, &body, &naturalKey)
		if err != nil {
			return nil // not found (or gone); nothing to derive
		}

		const sys = `You are given a Jira ticket title and body. Decide whether it defines a PRODUCT FEATURE or capability (not a bug, chore, incident, or task). If it does, produce a short feature name and the natural phrases people use in chat to refer to it. Respond as JSON: {"is_feature": bool, "name": "short feature name", "aliases": ["phrase", ...]}. Aliases: include the short name; 2-6 entries; natural lowercase-able phrases; NO ticket keys; NO generic words.`
		userMsg := "Title: " + title + "\n\n" + body

		out, genErr := deps.Gemini.Generate(ctx, sys, userMsg)
		if genErr != nil || out == "" {
			return genErr // transient: retry later
		}
		var res deriveFeatureResult
		if json.Unmarshal([]byte(out), &res) != nil {
			return nil // unparseable LLM output; nothing to do
		}
		if !res.IsFeature || res.Name == "" {
			return nil
		}

		// Aliases to store = LLM aliases + the ticket key. De-dup, drop empties.
		aliases := dedupeNonEmpty(append(res.Aliases, naturalKey))

		entityID := ids.Feature(res.Name)
		_, err = deps.DB.Exec(ctx, `
INSERT INTO graph.entities (id, kind, display_name, aliases, source, machine_id)
VALUES ($1,'feature',$2,$3,'derived:jira',$4)
ON CONFLICT (id) DO UPDATE SET
	display_name=EXCLUDED.display_name,
	aliases=(SELECT array_agg(DISTINCT a) FROM unnest(graph.entities.aliases || EXCLUDED.aliases) a)`,
			entityID, res.Name, aliases, deps.MachineID)
		if err != nil {
			return err
		}

		deps.Logger.Info().Str("entity_id", entityID).Int("aliases", len(aliases)).Msg("derive_feature_entity: upserted feature entity")
		return nil
	}
}

// dedupeNonEmpty returns the input with empty strings dropped and duplicates removed,
// preserving first-seen order.
func dedupeNonEmpty(in []string) []string {
	seen := make(map[string]bool, len(in))
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// enqueueDeriveFeatureEntity enqueues a derive_feature_entity job unless one is
// already queued/running for the same node — cheap dedup so a jira re-ingest
// doesn't pile up duplicate LLM jobs. Errors are ignored (best-effort).
func enqueueDeriveFeatureEntity(ctx context.Context, db *pgxpool.Pool, nodeID string) {
	if nodeID == "" {
		return
	}
	var exists bool
	_ = db.QueryRow(ctx, `
SELECT EXISTS(
  SELECT 1 FROM graph.jobs
  WHERE type='derive_feature_entity' AND status IN ('queued','running')
    AND payload->>'node_id'=$1)`,
		nodeID).Scan(&exists)
	if exists {
		return
	}
	_, _ = jobs.Enqueue(ctx, db, "derive_feature_entity", deriveFeatureEntityPayload{
		NodeID: nodeID,
	}, jobs.EnqueueOptions{Priority: 7})
}
