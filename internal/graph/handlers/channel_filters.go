package handlers

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Per-channel ingest filtering, applied at the pre-LLM chokepoint in
// ingest_content.go so muted/filtered messages never reach the extractor,
// embeddings, or summarize jobs — this is a cost lever, not just noise control.
//
// Config lives in the settings key "graph.channel_filters" (one JSON blob,
// mirroring graph_continents), editable live via SQL — no redeploy to change
// which channels are filtered:
//
//	{
//	  "ignore": ["C01T60D80JV"],
//	  "incident_only": {"C08S954G2LX": ["PagerDuty"]},
//	  "keep_regex": {"CPP5EH3A8": "(?i)pending.?payment|process[- ]?taxes"},
//	  "drop_regex": {"CPP5EH3A8": "(?i)white_check_mark[\\s\\S]*->\\s*200"}
//	}
//
// Rules per channel id:
//   - ignore:        drop every message.
//   - incident_only: keep only if the resolved author's display_name is in the
//     allow list (e.g. "PagerDuty"); drop all other (bot) noise.
//   - keep_regex:    keep only messages whose body matches; drop the rest.
//   - drop_regex:    drop messages whose body matches (evaluated after keep_regex,
//     so keep+drop together means "keep topic X but not its routine successes").
const channelFiltersKey = "graph.channel_filters"

type channelFiltersConfig struct {
	Ignore       []string            `json:"ignore"`
	IncidentOnly map[string][]string `json:"incident_only"`
	KeepRegex    map[string]string   `json:"keep_regex"`
	DropRegex    map[string]string   `json:"drop_regex"`
}

// compiledChannelFilters is the parsed+compiled form cached in-process.
type compiledChannelFilters struct {
	ignore       map[string]bool
	incidentOnly map[string][]string
	keepRe       map[string]*regexp.Regexp
	dropRe       map[string]*regexp.Regexp
}

var (
	cfMu       sync.Mutex
	cfCache    *compiledChannelFilters
	cfLoadedAt time.Time
)

const channelFiltersTTL = 60 * time.Second

// loadChannelFilters reads and compiles the config, cached for channelFiltersTTL
// so a bad/invalid regex is compiled once, not per ingest. An unset key or parse
// error yields an empty (no-op) filter set.
func loadChannelFilters(ctx context.Context, db *pgxpool.Pool) *compiledChannelFilters {
	cfMu.Lock()
	defer cfMu.Unlock()
	if cfCache != nil && time.Since(cfLoadedAt) < channelFiltersTTL {
		return cfCache
	}

	var raw string
	if db != nil {
		_ = db.QueryRow(ctx, `SELECT value FROM settings WHERE key=$1`, channelFiltersKey).Scan(&raw)
	}
	cfCache = compileChannelFilters(raw)
	cfLoadedAt = time.Now()
	return cfCache
}

// compileChannelFilters parses the JSON blob and compiles regexes. Invalid JSON
// or an uncompilable regex is skipped (that channel simply gets no such rule)
// rather than failing the whole config — a config typo must never wedge ingest.
func compileChannelFilters(raw string) *compiledChannelFilters {
	out := &compiledChannelFilters{
		ignore:       map[string]bool{},
		incidentOnly: map[string][]string{},
		keepRe:       map[string]*regexp.Regexp{},
		dropRe:       map[string]*regexp.Regexp{},
	}
	if raw == "" {
		return out
	}
	var cfg channelFiltersConfig
	if json.Unmarshal([]byte(raw), &cfg) != nil {
		return out
	}
	for _, id := range cfg.Ignore {
		if id != "" {
			out.ignore[id] = true
		}
	}
	for id, authors := range cfg.IncidentOnly {
		if id != "" {
			out.incidentOnly[id] = authors
		}
	}
	for id, pat := range cfg.KeepRegex {
		if re, err := regexp.Compile(pat); err == nil {
			out.keepRe[id] = re
		}
	}
	for id, pat := range cfg.DropRegex {
		if re, err := regexp.Compile(pat); err == nil {
			out.dropRe[id] = re
		}
	}
	return out
}

// channelContentSkip applies the channel-only rules (ignore + keep/drop regex)
// that need no author resolution, so they can run at the earliest chokepoint.
// Returns (skip, outcome) where outcome names why for the ingest response.
func channelContentSkip(ctx context.Context, deps Deps, channelID, body string) (bool, string) {
	if channelID == "" {
		return false, ""
	}
	return loadChannelFilters(ctx, deps.DB).contentSkip(channelID, body)
}

// contentSkip is the pure decision (no DB): ignore-list wins, then a keep_regex
// that doesn't match drops the message, then a drop_regex that matches drops it.
// keep+drop together = "keep this topic but not its routine successes".
func (f *compiledChannelFilters) contentSkip(channelID, body string) (bool, string) {
	if f.ignore[channelID] {
		return true, "skipped_ignored_channel"
	}
	if re, ok := f.keepRe[channelID]; ok && !re.MatchString(body) {
		return true, "skipped_off_topic"
	}
	if re, ok := f.dropRe[channelID]; ok && re.MatchString(body) {
		return true, "skipped_off_topic"
	}
	return false, ""
}

// incidentOnlyAuthors returns the allow list of author display names for an
// incident-only channel, or nil if the channel isn't incident-only.
func incidentOnlyAuthors(ctx context.Context, deps Deps, channelID string) []string {
	if channelID == "" {
		return nil
	}
	f := loadChannelFilters(ctx, deps.DB)
	authors, ok := f.incidentOnly[channelID]
	if !ok {
		return nil
	}
	return authors
}

// authorAllowed reports whether the resolved author (by person id) has a
// display_name in the allow list. A nil/unresolved author is never allowed —
// an incident-only channel keeps only messages from a known allowed sender.
func authorAllowed(ctx context.Context, deps Deps, authorPersonID *int64, allowed []string) bool {
	if authorPersonID == nil || deps.DB == nil {
		return false
	}
	var name string
	if err := deps.DB.QueryRow(ctx,
		`SELECT COALESCE(display_name,'') FROM graph.people WHERE id=$1`, *authorPersonID,
	).Scan(&name); err != nil {
		return false
	}
	return slices.Contains(allowed, name)
}
