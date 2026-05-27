// Package entities seeds and manages graph.entities rows from two sources:
// auto-discovery of payment partners from a repo checkout, and CSV import
// for manual entries.
package entities

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/ids"
)

// SeedFromPaymentsRepo walks <root>/pkg/payment/<partner>/ and inserts each
// directory name as a partner entity. Idempotent — uses ON CONFLICT.
// Caller passes the absolute path of the wego/payments checkout, typically
// from env WEGO_PAYMENTS_PATH or AGENT_MEM_GRAPH_PAYMENTS_PATH.
// If root is empty or doesn't exist, returns (0, nil) — no error.
func SeedFromPaymentsRepo(ctx context.Context, db *pgxpool.Pool, root string, log zerolog.Logger) (count int, err error) {
	if root == "" {
		return 0, nil
	}

	paymentDir := filepath.Join(root, "pkg", "payment")
	entries, err := os.ReadDir(paymentDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("entities: read payment dir %s: %w", paymentDir, err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		partnerID := ids.Partner(name)
		displayName := titleCase(name)
		aliases := []string{name, strings.ToLower(name), displayName}
		// Deduplicate aliases.
		aliases = dedupStrings(aliases)

		_, execErr := db.Exec(ctx, `
			INSERT INTO graph.entities (id, kind, display_name, aliases, source, machine_id)
			VALUES ($1, 'partner', $2, $3, 'seed:pkg-payment', 'local')
			ON CONFLICT (id) DO UPDATE
			  SET display_name = EXCLUDED.display_name,
			      aliases      = EXCLUDED.aliases,
			      source       = EXCLUDED.source`,
			partnerID, displayName, aliases,
		)
		if execErr != nil {
			return count, fmt.Errorf("entities: upsert partner %s: %w", name, execErr)
		}
		count++
		log.Debug().Str("id", partnerID).Str("name", name).Msg("entities: seeded partner")
	}
	return count, nil
}

// LoadFromCSV reads a 3-column CSV (kind,display_name,aliases) where aliases
// is pipe-separated. Inserts/updates rows. Idempotent.
//
// CSV format:
//
//	kind        ,display_name,aliases
//	partner     ,TripleA     ,TripleA|3A|triple a
//	feature     ,Auto Refund ,auto refund|auto-refund|auto_refund
//	status      ,None        ,none|null|empty
//	currency    ,TRY         ,TRY|try|Turkish Lira
func LoadFromCSV(ctx context.Context, db *pgxpool.Pool, reader io.Reader, log zerolog.Logger) (count int, err error) {
	r := csv.NewReader(reader)
	r.TrimLeadingSpace = true
	r.Comment = '#'

	// Read header.
	header, err := r.Read()
	if err == io.EOF {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("entities: read CSV header: %w", err)
	}
	if len(header) < 3 {
		return 0, fmt.Errorf("entities: CSV must have at least 3 columns (kind,display_name,aliases), got %d", len(header))
	}

	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return count, fmt.Errorf("entities: read CSV row: %w", err)
		}
		if len(rec) < 3 {
			log.Warn().Strs("row", rec).Msg("entities: skip short CSV row")
			continue
		}

		kind := strings.TrimSpace(rec[0])
		displayName := strings.TrimSpace(rec[1])
		rawAliases := strings.TrimSpace(rec[2])

		if kind == "" || displayName == "" {
			log.Warn().Strs("row", rec).Msg("entities: skip row with empty kind or display_name")
			continue
		}

		aliases := splitAliases(rawAliases)
		entityID := buildEntityID(kind, displayName)

		_, execErr := db.Exec(ctx, `
			INSERT INTO graph.entities (id, kind, display_name, aliases, source, machine_id)
			VALUES ($1, $2, $3, $4, 'manual', 'local')
			ON CONFLICT (id) DO UPDATE
			  SET display_name = EXCLUDED.display_name,
			      aliases      = EXCLUDED.aliases`,
			entityID, kind, displayName, aliases,
		)
		if execErr != nil {
			return count, fmt.Errorf("entities: upsert %s %q: %w", kind, displayName, execErr)
		}
		count++
		log.Debug().Str("id", entityID).Str("kind", kind).Msg("entities: loaded from CSV")
	}
	return count, nil
}

// buildEntityID constructs the canonical entity ID for a kind + display_name.
// It delegates to the ids package constructors when possible.
func buildEntityID(kind, displayName string) string {
	switch kind {
	case "partner":
		return ids.Partner(displayName)
	case "feature":
		return ids.Feature(displayName)
	case "status":
		return ids.Status(displayName)
	case "currency":
		return ids.Currency(displayName)
	default:
		slug := strings.ToLower(displayName)
		slug = strings.ReplaceAll(slug, " ", "_")
		return kind + ":" + slug
	}
}

// splitAliases splits a pipe-separated alias string and trims whitespace.
func splitAliases(raw string) []string {
	parts := strings.Split(raw, "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// titleCase title-cases a string where words are separated by common delimiters
// (spaces, hyphens, underscores).
func titleCase(s string) string {
	words := strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || r == '-' || r == '_'
	})
	for i, w := range words {
		if len(w) == 0 {
			continue
		}
		words[i] = strings.ToUpper(w[:1]) + strings.ToLower(w[1:])
	}
	return strings.Join(words, " ")
}

// dedupStrings returns a slice with duplicate strings removed (order preserved).
func dedupStrings(ss []string) []string {
	seen := make(map[string]struct{}, len(ss))
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}
