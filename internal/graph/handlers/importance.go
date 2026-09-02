package handlers

import (
	"context"
	_ "embed"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// importanceJSON is the curated importance config (owner, manager, tier weights,
// and people the org tree can't capture). It's the same file the org map reads —
// single source of truth. Edit internal/graph/handlers/importance.json and rebuild.
//
//go:embed importance.json
var importanceJSON []byte

// importanceConfig mirrors importance.json. Only the fields the notifier needs are
// modelled; tiers/seniority are consumed by the map's SQL, not here.
type importanceConfig struct {
	OwnerEEID   int32 `json:"owner_eeid"`
	ManagerEEID int32 `json:"manager_eeid"`
	Overrides   []struct {
		Name        string  `json:"name"`
		Score       float64 `json:"score"`
		Why         string  `json:"why"`
		Pin         bool    `json:"pin"`
		AlwaysAlert bool    `json:"always_alert"`
	} `json:"overrides"`
}

// loadImportanceConfig parses the embedded config. Returns a zero value (no
// overrides) on parse error so the notifier degrades to org-tree-only.
func loadImportanceConfig() importanceConfig {
	var c importanceConfig
	_ = json.Unmarshal(importanceJSON, &c)
	return c
}

// overrideImportantEeids resolves the config's override people (e.g. a payments
// business owner or a daily collaborator the reporting tree puts far away) to
// their eeids, so a message from one of them counts as "important" and can alert
// on its own. Only applies for the config's owner — these pins are the owner's.
func overrideImportantEeids(ctx context.Context, db *pgxpool.Pool, owner int32) []int32 {
	cfg := loadImportanceConfig()
	if owner == 0 || owner != cfg.OwnerEEID || len(cfg.Overrides) == 0 {
		return nil
	}
	names := make([]string, 0, len(cfg.Overrides))
	for _, o := range cfg.Overrides {
		if n := strings.TrimSpace(o.Name); n != "" {
			names = append(names, strings.ToLower(n))
		}
	}
	return resolveOverrideEeids(ctx, db, names)
}

// alwaysAlertEeids resolves only explicit always-alert overrides. They bypass
// topic relevance for this config's owner, without widening the important set.
func alwaysAlertEeids(ctx context.Context, db *pgxpool.Pool, owner int32) []int32 {
	cfg := loadImportanceConfig()
	if owner == 0 || owner != cfg.OwnerEEID || len(cfg.Overrides) == 0 {
		return nil
	}
	names := make([]string, 0, len(cfg.Overrides))
	for _, o := range cfg.Overrides {
		if !o.AlwaysAlert {
			continue
		}
		if n := strings.TrimSpace(o.Name); n != "" {
			names = append(names, strings.ToLower(n))
		}
	}
	return resolveOverrideEeids(ctx, db, names)
}

// resolveOverrideEeids matches display names case-insensitively against active,
// unmerged people with an org eeid.
func resolveOverrideEeids(ctx context.Context, db *pgxpool.Pool, names []string) []int32 {
	if len(names) == 0 {
		return nil
	}
	rows, err := db.Query(ctx,
		`SELECT eeid FROM graph.people
		 WHERE eeid IS NOT NULL AND merged_into IS NULL AND lower(display_name) = ANY($1)`, names)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int32
	for rows.Next() {
		var e int32
		if rows.Scan(&e) == nil {
			out = append(out, e)
		}
	}
	return out
}
