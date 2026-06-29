// Package fetchers provides per-source HTTP fetchers that retrieve a raw
// artifact body by canonical node ID or URL and package it as a FetchedBody
// for downstream normalisation and ingestion.
package fetchers

import (
	"context"
	"net/http"
	"time"

	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/ids"
)

// Fetcher retrieves the raw body for a single artifact and reports any
// inline attachments / files that come back with it.
type Fetcher interface {
	Source() string
	// Matches returns true if this fetcher claims responsibility for the
	// given canonical node id or URL. Used by the dispatcher to route.
	Matches(nodeIDorURL string) bool
	// Fetch retrieves the artifact. node identifies the target by either
	// its canonical node_id (preferred) or a raw URL.
	Fetch(ctx context.Context, node string) (FetchedBody, error)
}

// FetchedBody is the raw response packaged for downstream processing.
type FetchedBody struct {
	NodeID      string         // canonical id of the fetched artifact
	Type        ids.NodeType
	URL         string         // canonical permalink to the artifact
	Title       string
	Raw         []byte         // raw body (JSON or markdown or XML; the normalizer knows what to do)
	ContentType string         // "application/json","text/markdown",...
	Author      AuthorRef
	BodyTS      time.Time      // source-reported updated_at; used by ingest tiebreaker
	CreatedAt   time.Time      // source-reported created-at; populates graph.nodes.created_at (zero → BodyTS)
	Attachments []Attachment
	Metadata    map[string]any
}

// AuthorRef identifies the person who authored the artifact.
type AuthorRef struct {
	Source      string
	ExternalID  string
	DisplayName string
	Email       string
	IsBot       bool
}

// Attachment is a file attached to an artifact.
type Attachment struct {
	NodeID     string // e.g. slack_file:F0B5T0WD39P
	MimeType   string
	Filename   string
	SizeBytes  int64
	URLPrivate string // requires auth header to fetch bytes
	ThumbURL   string
}

// Config bundles the env-derived tokens and base URLs for all sources.
type Config struct {
	SlackBotToken string

	JiraEmail   string
	JiraToken   string
	JiraBaseURL string // https://wegomushi.atlassian.net

	GHToken   string
	GHBaseURL string // default https://api.github.com

	CFToken   string // for v1 we reuse JiraToken+JiraEmail; CFToken kept for future
	CFBaseURL string // https://wegomushi.atlassian.net/wiki

	PagerDutyToken   string
	PagerDutyBaseURL string // default https://api.pagerduty.com

	DatadogAPIKey  string
	DatadogAppKey  string
	DatadogBaseURL string // default https://api.datadoghq.com

	SentryAuthToken string
	SentryBaseURL   string // default https://sentry.io
	SentryOrg       string // org slug for project-scoped issue URLs

	GWSServiceKeyPath string // path to a Google service-account JSON

	WegoHubToken   string // deploy/Bearer token for the Wego Hub read API
	WegoHubBaseURL string // default https://internal.wego.com/hub

	HTTPClient *http.Client // optional override for tests; defaults to a client with 15s timeout
}

// defaultHTTPClient returns an HTTP client with a 15-second timeout.
func defaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Second}
}

// Registry binds Config to fetcher instances.
type Registry struct {
	fetchers []Fetcher
	cfg      Config // retained for source-tree reads (Confluence descendants, repo markdown)
}

// NewRegistry creates a Registry with all eight fetchers registered.
func NewRegistry(cfg Config, log zerolog.Logger) *Registry {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = defaultHTTPClient()
	}
	if cfg.GHBaseURL == "" {
		cfg.GHBaseURL = "https://api.github.com"
	}
	if cfg.PagerDutyBaseURL == "" {
		cfg.PagerDutyBaseURL = "https://api.pagerduty.com"
	}
	if cfg.DatadogBaseURL == "" {
		cfg.DatadogBaseURL = "https://api.datadoghq.com"
	}
	if cfg.SentryBaseURL == "" {
		cfg.SentryBaseURL = "https://sentry.io"
	}
	if cfg.WegoHubBaseURL == "" {
		cfg.WegoHubBaseURL = "https://internal.wego.com/hub"
	}

	r := &Registry{cfg: cfg}
	r.fetchers = []Fetcher{
		newSlackFetcher(cfg, log),
		newJiraFetcher(cfg, log),
		newGitHubFetcher(cfg, log),
		newConfluenceFetcher(cfg, log),
		newPagerDutyFetcher(cfg, log),
		newDatadogFetcher(cfg, log),
		newSentryFetcher(cfg, log),
		newGWSFetcher(cfg, log),
		newWegoHubFetcher(cfg, log),
		newClaudeArtifactFetcher(cfg, log),
	}
	return r
}

// For returns the Fetcher that claims responsibility for nodeIDorURL.
func (r *Registry) For(nodeIDorURL string) (Fetcher, bool) {
	for _, f := range r.fetchers {
		if f.Matches(nodeIDorURL) {
			return f, true
		}
	}
	return nil, false
}
