package handlers

import (
	"context"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
)

const prBoilerplateMinPRs = 10

var prBoilerplateKey = regexp.MustCompile(`\b([A-Z]{2,10}-\d+)\b`)

// stripPRBoilerplate drops body lines that also appear, normalized, in at least
// prBoilerplateMinPRs other gh_pr bodies. Template text then can't create edges.
func stripPRBoilerplate(ctx context.Context, db *pgxpool.Pool, nodeID, body string) string {
	lines := strings.Split(body, "\n")
	norms := make([]string, len(lines))
	var candidates, keys []string
	for i, line := range lines {
		key := prBoilerplateKey.FindString(line)
		if key == "" {
			continue
		}
		norm := strings.Map(func(r rune) rune {
			if r >= 'A' && r <= 'Z' {
				return r + ('a' - 'A')
			}
			if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, line)
		if len(norm) < 20 {
			continue
		}
		norms[i] = norm
		candidates = append(candidates, norm)
		keys = append(keys, key)
	}
	if len(candidates) == 0 {
		return body
	}

	// ponytail: scans every PR body for the key (about 1,990 today, ~0.9 s
	// per PR fetch). If PR fetch_body gets slow, store normalized lines in a table.
	rows, err := db.Query(ctx, `
SELECT c.norm
FROM unnest($2::text[], $3::text[]) AS c(norm, key)
WHERE (SELECT count(*) FROM graph.nodes n
       WHERE n.type = 'gh_pr' AND n.id <> $1 AND n.deleted_at IS NULL
         AND n.body ILIKE '%' || c.key || '%'
         AND lower(regexp_replace(n.body, '[^A-Za-z0-9]', '', 'g')) LIKE '%' || c.norm || '%') >= $4
`, nodeID, candidates, keys, prBoilerplateMinPRs)
	if err != nil {
		log.Warn().Err(err).Str("node_id", nodeID).Msg("fetch_body: PR boilerplate query failed; keeping full text")
		return body
	}
	defer rows.Close()
	drop := make(map[string]bool)
	for rows.Next() {
		var norm string
		if err := rows.Scan(&norm); err != nil {
			log.Warn().Err(err).Str("node_id", nodeID).Msg("fetch_body: PR boilerplate scan failed; keeping full text")
			return body
		}
		drop[norm] = true
	}
	if err := rows.Err(); err != nil {
		log.Warn().Err(err).Str("node_id", nodeID).Msg("fetch_body: PR boilerplate rows failed; keeping full text")
		return body
	}
	if len(drop) == 0 {
		return body
	}
	kept := lines[:0]
	for i, line := range lines {
		if !drop[norms[i]] {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}
