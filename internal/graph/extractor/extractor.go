// Package extractor runs URL-regex, ID-regex, and entity-alias matching over
// a normalised (plain-text) body and emits structured Findings.
package extractor

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/ids"
)

// MatchOrigin describes how a Finding was discovered.
type MatchOrigin string

const (
	OriginURL     MatchOrigin = "url"
	OriginIDRegex MatchOrigin = "id_regex"
	OriginEntity  MatchOrigin = "entity"
)

// Finding is one identified reference inside a body.
type Finding struct {
	NodeID   string       // canonical node id, e.g. "jira:PAY-2128"
	Type     ids.NodeType // type prefix
	Source   string       // raw source string ("PAY-2128", "https://...")
	EdgeKind string       // suggested edge kind: REFERENCES, PART_OF, MENTIONS, TOUCHES
	Match    MatchOrigin  // how it was found
}

// Result is the full set of findings from one extraction pass.
type Result struct {
	Findings []Finding
	// Convenience views:
	URLs     []string
	JiraKeys []string
	GHPRs    []string // "wego/payments#1960"
	Entities []string // canonical entity node ids
}

// entityRow is a cached row from graph.entities.
type entityRow struct {
	id      string
	kind    string
	aliases []string
}

// Extractor runs URL regex + ID regex + entity-alias matching against a body.
type Extractor struct {
	db  *pgxpool.Pool
	log zerolog.Logger

	mu          sync.RWMutex
	aliasMap    map[string]string // lowercase-alias → entity node id
	aliasPatMap map[string]*regexp.Regexp
	cachedAt    time.Time
	cacheTTL    time.Duration
}

// New creates an Extractor backed by db.
func New(db *pgxpool.Pool, log zerolog.Logger) *Extractor {
	return &Extractor{
		db:       db,
		log:      log,
		cacheTTL: 5 * time.Minute,
	}
}

// Refresh reloads the entity alias trie from the database unconditionally.
// If the Extractor was created with a nil pool, Refresh is a no-op.
func (e *Extractor) Refresh(ctx context.Context) error {
	if e.db == nil {
		return nil
	}
	rows, err := e.db.Query(ctx,
		`SELECT id, kind, aliases FROM graph.entities`)
	if err != nil {
		return fmt.Errorf("extractor: load entities: %w", err)
	}
	defer rows.Close()

	aliasMap := make(map[string]string)
	aliasPatMap := make(map[string]*regexp.Regexp)

	for rows.Next() {
		var ent entityRow
		if err := rows.Scan(&ent.id, &ent.kind, &ent.aliases); err != nil {
			return fmt.Errorf("extractor: scan entity: %w", err)
		}
		for _, alias := range ent.aliases {
			lower := strings.ToLower(alias)
			if lower == "" {
				continue
			}
			if _, exists := aliasMap[lower]; !exists {
				aliasMap[lower] = ent.id
				// Build a word-boundary pattern for this alias.
				pat := `(?i)\b` + regexp.QuoteMeta(alias) + `\b`
				re, err := regexp.Compile(pat)
				if err != nil {
					e.log.Warn().Str("alias", alias).Err(err).Msg("extractor: skip bad alias pattern")
					continue
				}
				aliasPatMap[lower] = re
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("extractor: entity rows: %w", err)
	}

	e.mu.Lock()
	e.aliasMap = aliasMap
	e.aliasPatMap = aliasPatMap
	e.cachedAt = time.Now()
	e.mu.Unlock()
	return nil
}

// ensureCache refreshes the entity cache if it's stale.
// No-op when pool is nil (regex-only mode).
func (e *Extractor) ensureCache(ctx context.Context) {
	if e.db == nil {
		return
	}
	e.mu.RLock()
	stale := time.Since(e.cachedAt) > e.cacheTTL
	e.mu.RUnlock()
	if stale {
		if err := e.Refresh(ctx); err != nil {
			e.log.Warn().Err(err).Msg("extractor: entity cache refresh failed; proceeding with stale cache")
		}
	}
}

// -----------------------------------------------------------------------
// Regex rules
// -----------------------------------------------------------------------

type rule struct {
	re    *regexp.Regexp
	build func(matches []string) (Finding, bool)
}

// slackTSToStandard converts a raw Slack ts integer string (16+ digits) to
// the dotted format: insert a dot before the last 6 digits.
// e.g. "1779710863216389" → "1779710863.216389"
func slackTSToStandard(raw string) string {
	if len(raw) <= 6 {
		return raw
	}
	return raw[:len(raw)-6] + "." + raw[len(raw)-6:]
}

var rules = []rule{
	// Slack thread/message archive URL:
	// https://wego.slack.com/archives/C08S954G2LX/p1779710863216389
	{
		re: regexp.MustCompile(`\bwego\.slack\.com/archives/(C\w+)/p(\d+)\b`),
		build: func(m []string) (Finding, bool) {
			channel := m[1]
			ts := slackTSToStandard(m[2])
			nodeID := ids.SlackThread(channel, ts)
			return Finding{
				NodeID:   nodeID,
				Type:     ids.TypeSlackThread,
				Source:   m[0],
				EdgeKind: "REFERENCES",
				Match:    OriginURL,
			}, true
		},
	},
	// Slack file URL:
	// https://wego.slack.com/files/U.../Fxxx/filename
	{
		re: regexp.MustCompile(`\bwego\.slack\.com/files/[^/\s]+/(F\w+)\b`),
		build: func(m []string) (Finding, bool) {
			nodeID := ids.SlackFile(m[1])
			return Finding{
				NodeID:   nodeID,
				Type:     ids.TypeSlackFile,
				Source:   m[0],
				EdgeKind: "REFERENCES",
				Match:    OriginURL,
			}, true
		},
	},
	// Jira URL: https://wegomushi.atlassian.net/browse/PAY-2128
	{
		re: regexp.MustCompile(`\bwegomushi\.atlassian\.net/browse/([A-Z][A-Z0-9]+-\d+)\b`),
		build: func(m []string) (Finding, bool) {
			nodeID, err := ids.Jira(m[1])
			if err != nil {
				return Finding{}, false
			}
			return Finding{
				NodeID:   nodeID,
				Type:     ids.TypeJira,
				Source:   m[0],
				EdgeKind: "REFERENCES",
				Match:    OriginURL,
			}, true
		},
	},
	// GitHub PR URL: https://github.com/wego/payments/pull/1960
	{
		re: regexp.MustCompile(`\bgithub\.com/(wego/[\w-]+)/pull/(\d+)\b`),
		build: func(m []string) (Finding, bool) {
			num, err := strconv.Atoi(m[2])
			if err != nil {
				return Finding{}, false
			}
			nodeID, err := ids.GHPR(m[1], num)
			if err != nil {
				return Finding{}, false
			}
			return Finding{
				NodeID:   nodeID,
				Type:     ids.TypeGHPR,
				Source:   m[0],
				EdgeKind: "REFERENCES",
				Match:    OriginURL,
			}, true
		},
	},
	// PagerDuty: https://wegotravel.pagerduty.com/incidents/P8K3M2N
	{
		re: regexp.MustCompile(`\bwegotravel\.pagerduty\.com/incidents/(\w+)\b`),
		build: func(m []string) (Finding, bool) {
			nodeID, err := ids.PagerDuty(m[1])
			if err != nil {
				return Finding{}, false
			}
			return Finding{
				NodeID:   nodeID,
				Type:     ids.TypePagerDuty,
				Source:   m[0],
				EdgeKind: "REFERENCES",
				Match:    OriginURL,
			}, true
		},
	},
	// Datadog monitor: https://app.datadoghq.com/monitors/133274814
	{
		re: regexp.MustCompile(`\bapp\.datadoghq\.com/monitors/(\d+)\b`),
		build: func(m []string) (Finding, bool) {
			id, err := strconv.ParseInt(m[1], 10, 64)
			if err != nil {
				return Finding{}, false
			}
			nodeID, err := ids.Datadog("monitor", id)
			if err != nil {
				return Finding{}, false
			}
			return Finding{
				NodeID:   nodeID,
				Type:     ids.TypeDatadog,
				Source:   m[0],
				EdgeKind: "REFERENCES",
				Match:    OriginURL,
			}, true
		},
	},
	// Sentry issue: https://sentry.io/wego/payments/issues/4872610293/
	{
		re: regexp.MustCompile(`\bsentry\.io/[\w-]+/[\w-]+/issues/(\w+)/?`),
		build: func(m []string) (Finding, bool) {
			nodeID, err := ids.Sentry(m[1])
			if err != nil {
				return Finding{}, false
			}
			return Finding{
				NodeID:   nodeID,
				Type:     ids.TypeSentry,
				Source:   m[0],
				EdgeKind: "REFERENCES",
				Match:    OriginURL,
			}, true
		},
	},
	// Confluence page: https://wegomushi.atlassian.net/wiki/.../pages/987654321
	{
		re: regexp.MustCompile(`\bwegomushi\.atlassian\.net/wiki/[^\s]*?pages/(\d+)\b`),
		build: func(m []string) (Finding, bool) {
			id, err := strconv.ParseInt(m[1], 10, 64)
			if err != nil {
				return Finding{}, false
			}
			nodeID := ids.CFPage(id)
			return Finding{
				NodeID:   nodeID,
				Type:     ids.TypeCFPage,
				Source:   m[0],
				EdgeKind: "REFERENCES",
				Match:    OriginURL,
			}, true
		},
	},
	// Google Docs: https://docs.google.com/document/d/<id>
	{
		re: regexp.MustCompile(`\bdocs\.google\.com/document/d/([\w-]+)\b`),
		build: func(m []string) (Finding, bool) {
			nodeID := ids.GWSDoc(m[1])
			return Finding{
				NodeID:   nodeID,
				Type:     ids.TypeGWSDoc,
				Source:   m[0],
				EdgeKind: "REFERENCES",
				Match:    OriginURL,
			}, true
		},
	},
	// Google Drive file: https://drive.google.com/file/d/<id>
	{
		re: regexp.MustCompile(`\bdrive\.google\.com/file/d/([\w-]+)\b`),
		build: func(m []string) (Finding, bool) {
			nodeID := ids.GWSDoc(m[1])
			return Finding{
				NodeID:   nodeID,
				Type:     ids.TypeGWSDoc,
				Source:   m[0],
				EdgeKind: "REFERENCES",
				Match:    OriginURL,
			}, true
		},
	},
	// Wego order ref: WF-XXXXXXXX-XXXX[-XXXX...] — one mandatory 8-char segment
	// followed by one or more dash-separated hex groups of 4+ chars each.
	{
		re: regexp.MustCompile(`\bWF-[A-Fa-f0-9]{8}(?:-[A-Fa-f0-9]{4,})+\b`),
		build: func(m []string) (Finding, bool) {
			nodeID := "wego_order:" + m[0]
			return Finding{
				NodeID:   nodeID,
				Type:     ids.NodeType("wego_order"),
				Source:   m[0],
				EdgeKind: "REFERENCES",
				Match:    OriginIDRegex,
			}, true
		},
	},
	// CKO processing channel: pc_<alphanumeric_with_underscores>
	{
		re: regexp.MustCompile(`\bpc_[a-zA-Z0-9_]+\b`),
		build: func(m []string) (Finding, bool) {
			nodeID := "cko_processing_channel:" + m[0]
			return Finding{
				NodeID:   nodeID,
				Type:     ids.NodeType("cko_processing_channel"),
				Source:   m[0],
				EdgeKind: "REFERENCES",
				Match:    OriginIDRegex,
			}, true
		},
	},
	// Payment ref: F.<id> — only at word boundary, not inside file extensions
	// Must be preceded by whitespace or start of line (not inside a word like "F.go" at non-boundary).
	{
		re: regexp.MustCompile(`(?:^|[\s,;:(])F\.([a-z0-9]+)\b`),
		build: func(m []string) (Finding, bool) {
			// m[0] is full match (may include leading whitespace), m[1] is the id suffix
			raw := "F." + m[1]
			nodeID := "payment_ref:" + raw
			return Finding{
				NodeID:   nodeID,
				Type:     ids.NodeType("payment_ref"),
				Source:   raw,
				EdgeKind: "REFERENCES",
				Match:    OriginIDRegex,
			}, true
		},
	},
}

// idRegexRule matches bare Jira-style keys like PAY-2128 (not already caught by URL rule).
// We skip lines that start with 4+ spaces (code blocks) and text inside backtick fences.
var bareJiraRe = regexp.MustCompile(`\b([A-Z]{2,10}-\d+)\b`)
var codeFenceRe = regexp.MustCompile("(?m)^```")

// Extract walks the body once, returns all findings deduplicated by NodeID.
func (e *Extractor) Extract(ctx context.Context, body string) (Result, error) {
	e.ensureCache(ctx)

	seen := make(map[string]struct{})
	var findings []Finding

	addFinding := func(f Finding) {
		if _, ok := seen[f.NodeID]; !ok {
			seen[f.NodeID] = struct{}{}
			findings = append(findings, f)
		}
	}

	// Strip code-fenced regions to avoid matching IDs inside them.
	cleanBody := stripCodeFences(body)

	// Apply all URL/structured-ID rules.
	for _, r := range rules {
		allMatches := r.re.FindAllStringSubmatch(cleanBody, -1)
		for _, m := range allMatches {
			f, ok := r.build(m)
			if ok {
				addFinding(f)
			}
		}
	}

	// Bare Jira key rule — skip indented lines (code blocks).
	bareLines := filterNonCodeLines(cleanBody)
	for _, m := range bareJiraRe.FindAllStringSubmatch(bareLines, -1) {
		key := m[1]
		nodeID, err := ids.Jira(key)
		if err != nil {
			continue
		}
		addFinding(Finding{
			NodeID:   nodeID,
			Type:     ids.TypeJira,
			Source:   key,
			EdgeKind: "REFERENCES",
			Match:    OriginIDRegex,
		})
	}

	// Entity alias matching.
	e.mu.RLock()
	aliasPatMap := e.aliasPatMap
	aliasMap := e.aliasMap
	e.mu.RUnlock()

	// Build the reverse map: nodeID → display for dedup.
	for lower, nodeID := range aliasMap {
		re, ok := aliasPatMap[lower]
		if !ok {
			continue
		}
		if re.MatchString(cleanBody) {
			nodeType, _ := ids.ParseType(nodeID)
			addFinding(Finding{
				NodeID:   nodeID,
				Type:     nodeType,
				Source:   lower,
				EdgeKind: "REFERENCES",
				Match:    OriginEntity,
			})
		}
	}

	// Build convenience views.
	var result Result
	result.Findings = findings
	for _, f := range findings {
		switch f.Match {
		case OriginURL:
			result.URLs = append(result.URLs, f.Source)
		}
		switch f.Type {
		case ids.TypeJira:
			result.JiraKeys = append(result.JiraKeys, f.Source)
		case ids.TypeGHPR:
			result.GHPRs = append(result.GHPRs, f.Source)
		}
		if f.Match == OriginEntity {
			result.Entities = append(result.Entities, f.NodeID)
		}
	}

	return result, nil
}

// stripCodeFences removes text between ``` fences so we don't extract IDs from code blocks.
func stripCodeFences(body string) string {
	parts := codeFenceRe.Split(body, -1)
	if len(parts) <= 1 {
		return body
	}
	var b strings.Builder
	for i, part := range parts {
		// Even-indexed parts are outside fences, odd-indexed are inside.
		if i%2 == 0 {
			b.WriteString(part)
		} else {
			// Replace with same number of newlines to preserve line numbers.
			for _, c := range part {
				if c == '\n' {
					b.WriteByte('\n')
				}
			}
		}
	}
	return b.String()
}

// filterNonCodeLines removes lines that start with 4+ spaces (indented code blocks).
func filterNonCodeLines(body string) string {
	lines := strings.Split(body, "\n")
	var out []string
	for _, line := range lines {
		if !strings.HasPrefix(line, "    ") {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
