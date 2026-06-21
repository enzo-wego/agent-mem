// Package normalizer converts source-native bodies into plain UTF-8 text
// suitable for embedding, URL/ID extraction, and LLM context.
package normalizer

import "context"

// Normalizer converts a source-native body into plain UTF-8 text plus a
// structured list of mentions when the source carries them inline.
type Normalizer interface {
	// Source returns the canonical source name (e.g. "slack", "jira").
	Source() string
	// Normalize transforms the raw body. Returns the plain text and any
	// inline-mention references the normalizer recognised (slack uids,
	// jira account ids, github logins, etc.). Mentions are returned as
	// canonical Refs with display_name when known.
	Normalize(ctx context.Context, raw []byte, meta map[string]any) (Result, error)
}

// Result holds the output of a normalizer.
type Result struct {
	Text     string
	Mentions []Mention
}

// Mention is a structured reference to a person or entity found inline.
type Mention struct {
	Source      string // "slack","jira","github","confluence","pagerduty","datadog","sentry","gws"
	ExternalID  string
	DisplayName string
}

// Registry maps source name to its Normalizer.
type Registry struct {
	m map[string]Normalizer
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{m: make(map[string]Normalizer)}
}

// Register adds (or replaces) a normalizer in the registry.
func (r *Registry) Register(n Normalizer) {
	r.m[n.Source()] = n
}

// For returns the Normalizer for the given source name, if registered.
func (r *Registry) For(source string) (Normalizer, bool) {
	n, ok := r.m[source]
	return n, ok
}

// NewDefault returns a registry preloaded with all eight normalizers.
// cache is used by the Slack normalizer to resolve UIDs to display names;
// pass nil to use a no-op cache.
func NewDefault(cache Cache) *Registry {
	if cache == nil {
		cache = noopCache{}
	}
	r := NewRegistry()
	r.Register(NewSlackNormalizer(cache))
	r.Register(NewJiraNormalizer())
	r.Register(NewGitHubNormalizer())
	r.Register(NewConfluenceNormalizer())
	r.Register(NewPagerDutyNormalizer())
	r.Register(NewDatadogNormalizer())
	r.Register(NewSentryNormalizer())
	r.Register(NewGWSNormalizer())
	r.Register(NewWegoHubNormalizer())
	return r
}
