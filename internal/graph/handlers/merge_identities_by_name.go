package handlers

import (
	"context"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// NewMergeIdentitiesByNameHandler returns the job entry for "merge_identities_by_name":
// it bridges Slack identities to their BambooHR identities by exact full-name match
// (BambooHR people.display_name <-> Slack slack_users.real_name) when no shared email
// exists, so a person's Slack messages inherit their org-depth/seniority. Reads
// everything from the DB — no CSV needed. Conservative: only merges when the name is
// unique on both sides.
func NewMergeIdentitiesByNameHandler(deps Deps) jobs.Entry {
	return jobs.Entry{
		Handler:  mergeIdentitiesByNameHandler(deps),
		PoolSize: 1,
		Lease:    300 * time.Second,
	}
}

func mergeIdentitiesByNameHandler(deps Deps) jobs.Handler {
	return func(ctx context.Context, _ []byte) error {
		// BambooHR (eeid) people, keyed by lowercased display_name; skip ambiguous names.
		rows, err := deps.DB.Query(ctx,
			`SELECT id, lower(display_name) FROM graph.people
			 WHERE eeid IS NOT NULL AND merged_into IS NULL AND COALESCE(display_name,'') <> ''`)
		if err != nil {
			return err
		}
		nameCount := map[string]int{}
		nameID := map[string]int64{}
		for rows.Next() {
			var id int64
			var name string
			if err := rows.Scan(&id, &name); err != nil {
				rows.Close()
				return err
			}
			nameCount[name]++
			nameID[name] = id
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		merged := 0
		for name, cnt := range nameCount {
			if cnt != 1 {
				continue // ambiguous BambooHR name
			}
			canonicalID := nameID[name]
			// Exactly one un-merged, eeid-less Slack person with that real_name.
			var matches int
			var otherID int64
			if err := deps.DB.QueryRow(ctx, `
				SELECT count(*), COALESCE(min(p.id),0)
				FROM graph.slack_users su
				JOIN graph.people p ON p.slack_user_id = su.slack_user_id
				WHERE lower(su.real_name) = $1 AND p.merged_into IS NULL AND p.eeid IS NULL`,
				name).Scan(&matches, &otherID); err != nil {
				deps.Logger.Warn().Err(err).Str("name", name).Msg("merge_identities_by_name: lookup failed")
				continue
			}
			if matches != 1 || otherID == 0 || otherID == canonicalID {
				continue
			}
			if err := mergePersonInto(ctx, deps, canonicalID, otherID, ""); err != nil {
				deps.Logger.Warn().Err(err).Str("name", name).Msg("merge_identities_by_name: merge failed")
				continue
			}
			merged++
		}
		deps.Logger.Info().Int("bamboo_people", len(nameCount)).Int("merged", merged).Msg("merge_identities_by_name: done")
		return nil
	}
}
