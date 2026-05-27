package fetchers

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// noLogger returns a zerolog logger that discards output.
func noLogger() zerolog.Logger {
	return zerolog.Nop()
}

// rewriteClient returns an HTTP client that rewrites all request hosts to targetBase.
// This allows fetchers that hardcode their API URLs to be tested against a local httptest server.
func newRewriteClient(targetBase string, inner *http.Client) *http.Client {
	target, _ := url.Parse(targetBase)
	inner.Transport = &rewriteTransport{target: target, wrapped: http.DefaultTransport}
	return inner
}

// newRewriteClientForHost rewrites requests whose Host matches hostToRewrite.
func newRewriteClientForHost(hostToRewrite, targetBase string, inner *http.Client) *http.Client {
	target, _ := url.Parse(targetBase)
	inner.Transport = &hostRewriteTransport{
		hostMap: map[string]string{hostToRewrite: target.Host},
		scheme:  target.Scheme,
		wrapped: http.DefaultTransport,
	}
	return inner
}

// newRewriteClientForHosts rewrites based on a host map.
func newRewriteClientForHosts(hostMap map[string]string, inner *http.Client) *http.Client {
	// Determine the target scheme from any value.
	scheme := "http"
	resolvedMap := make(map[string]string)
	for k, v := range hostMap {
		t, _ := url.Parse(v)
		resolvedMap[k] = t.Host
		scheme = t.Scheme
	}
	inner.Transport = &hostRewriteTransport{
		hostMap: resolvedMap,
		scheme:  scheme,
		wrapped: http.DefaultTransport,
	}
	return inner
}

// rewriteTransport rewrites ALL request hosts to a single target.
type rewriteTransport struct {
	target  *url.URL
	wrapped http.RoundTripper
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = t.target.Scheme
	clone.URL.Host = t.target.Host
	return t.wrapped.RoundTrip(clone)
}

// hostRewriteTransport rewrites specific hosts.
type hostRewriteTransport struct {
	hostMap map[string]string
	scheme  string
	wrapped http.RoundTripper
}

func (t *hostRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	if newHost, ok := t.hostMap[clone.URL.Hostname()]; ok {
		clone.URL.Host = newHost
		clone.URL.Scheme = t.scheme
	}
	return t.wrapped.RoundTrip(clone)
}

// TestRegistry_For verifies the registry routes correctly.
func TestRegistry_For(t *testing.T) {
	cfg := Config{
		SlackBotToken:    "s",
		JiraEmail:        "e",
		JiraToken:        "j",
		JiraBaseURL:      "https://wegomushi.atlassian.net",
		CFBaseURL:        "https://wegomushi.atlassian.net/wiki",
		GHToken:          "g",
		PagerDutyToken:   "pd",
		DatadogAPIKey:    "dd-api",
		DatadogAppKey:    "dd-app",
		SentryAuthToken:  "sn",
		GWSServiceKeyPath: "/fake",
	}
	reg := NewRegistry(cfg, noLogger())

	cases := []struct {
		input      string
		wantSource string
		wantFound  bool
	}{
		{"slack:C08S954G2LX:1779710863.216389", "slack", true},
		{"jira:PAY-2128", "jira", true},
		{"gh_pr:wego/payments#1960", "github", true},
		{"cf:987654321", "confluence", true},
		{"pagerduty:P8K3M2N", "pagerduty", true},
		{"datadog:monitor:133274814", "datadog", true},
		{"sentry:4872610293", "sentry", true},
		{"gws_doc:1BxiMVs0XRA5nFMdKvBdBZjgmUUqptlbs74OgVE2upms", "gws", true},
		{"unknown:foo", "", false},
		{"https://example.com/unknown", "", false},
		// URL-based routing.
		{"https://wego.slack.com/archives/C08S954G2LX/p1779710863216389", "slack", true},
		{"https://wegomushi.atlassian.net/browse/PAY-2128", "jira", true},
		{"https://github.com/wego/payments/pull/1960", "github", true},
		{"https://wegomushi.atlassian.net/wiki/spaces/ENG/pages/987654321/Title", "confluence", true},
		{"https://wegotravel.pagerduty.com/incidents/P8K3M2N", "pagerduty", true},
		{"https://app.datadoghq.com/monitors/133274814", "datadog", true},
		{"https://sentry.io/wego/payments/issues/4872610293/", "sentry", true},
		{"https://docs.google.com/document/d/1BxiMVs0XRA5nFMdKvBdBZjgmUUqptlbs74OgVE2upms/edit", "gws", true},
		{"https://drive.google.com/file/d/1BxiMVs0XRA5nFMdKvBdBZjgmUUqptlbs74OgVE2upms/view", "gws", true},
	}

	for _, tc := range cases {
		f, ok := reg.For(tc.input)
		if ok != tc.wantFound {
			t.Errorf("For(%q) found=%v, want %v", tc.input, ok, tc.wantFound)
			continue
		}
		if !ok {
			continue
		}
		if f.Source() != tc.wantSource {
			t.Errorf("For(%q) source=%q, want %q", tc.input, f.Source(), tc.wantSource)
		}
	}
}

// TestRegistry_DefaultsApplied checks that base URLs are set to defaults.
func TestRegistry_DefaultsApplied(t *testing.T) {
	reg := NewRegistry(Config{}, noLogger())
	// Registry should be created without panic.
	if reg == nil {
		t.Fatal("registry is nil")
	}
	// GitHub fetcher should use default base URL.
	f, ok := reg.For("gh_pr:wego/repo#1")
	if !ok {
		t.Fatal("github fetcher not found")
	}
	gf := f.(*gitHubFetcher)
	if !strings.Contains(gf.cfg.GHBaseURL, "api.github.com") {
		t.Errorf("GHBaseURL = %q, want api.github.com", gf.cfg.GHBaseURL)
	}
}
