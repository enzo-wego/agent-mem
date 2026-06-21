package fetchers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/ids"
)

// claudeArtifactHost is the only host this fetcher will ever request. Anchoring
// validation on an exact host (not a substring match) prevents SSRF via inputs
// like https://evil.com/claude.ai/public/artifacts/<id>.
const claudeArtifactHost = "claude.ai"

var (
	// claudeArtifactNodeRe matches "claude_artifact:<id>".
	claudeArtifactNodeRe = regexp.MustCompile(`^claude_artifact:([A-Za-z0-9_-]{8,})$`)
	// claudeArtifactPathRe matches the path of a shared/published artifact URL,
	// anchored end-to-end: /public/artifacts/<id> or /code/artifact/<id>.
	claudeArtifactPathRe = regexp.MustCompile(`^/(?:public/artifacts|code/artifact)/([A-Za-z0-9_-]{8,})/?$`)
	// htmlTitleRe extracts the <title> for a human-readable node title.
	htmlTitleRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
)

// claudeArtifactFetcher retrieves a shared Claude artifact (self-contained
// HTML at a public claude.ai URL). No auth: only shared/public artifacts are
// reachable server-side; private ones sit behind the owner's session and 404.
type claudeArtifactFetcher struct {
	cfg Config
	log zerolog.Logger
}

func newClaudeArtifactFetcher(cfg Config, log zerolog.Logger) *claudeArtifactFetcher {
	return &claudeArtifactFetcher{cfg: cfg, log: log}
}

func (f *claudeArtifactFetcher) Source() string { return "claude_artifact" }

func (f *claudeArtifactFetcher) Matches(nodeIDorURL string) bool {
	_, _, err := f.parseNode(nodeIDorURL)
	return err == nil
}

func (f *claudeArtifactFetcher) Fetch(ctx context.Context, node string) (FetchedBody, error) {
	id, fetchURL, err := f.parseNode(node)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("claude_artifact fetcher: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fetchURL, nil)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("claude_artifact fetcher: build request: %w", err)
	}
	req.Header.Set("Accept", "text/html")

	resp, err := f.cfg.HTTPClient.Do(req)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("claude_artifact fetcher: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return FetchedBody{}, fmt.Errorf("claude_artifact fetcher status %d (artifact may be private/unshared): %s", resp.StatusCode, string(body))
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return FetchedBody{}, fmt.Errorf("claude_artifact fetcher: read body: %w", err)
	}

	title := id
	if m := htmlTitleRe.FindSubmatch(raw); m != nil {
		if t := strings.TrimSpace(string(m[1])); t != "" {
			title = t
		}
	}

	nodeID, err := ids.ClaudeArtifact(id)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("claude_artifact fetcher: %w", err)
	}

	return FetchedBody{
		NodeID:      nodeID,
		Type:        ids.TypeClaudeArtifact,
		URL:         fetchURL,
		Title:       title,
		Raw:         raw,
		ContentType: "text/html",
		Metadata:    map[string]any{"artifact_id": id},
	}, nil
}

// parseNode returns the artifact id and the URL to GET. The returned URL is
// always reconstructed from a verified scheme+host+path (never echoed from the
// input) so an attacker cannot point the fetch at an arbitrary host (SSRF).
func (f *claudeArtifactFetcher) parseNode(node string) (id, fetchURL string, err error) {
	// Canonical node ID form → reconstruct the public-share URL.
	if m := claudeArtifactNodeRe.FindStringSubmatch(node); m != nil {
		return m[1], "https://" + claudeArtifactHost + "/public/artifacts/" + m[1], nil
	}

	// URL form → validate scheme + exact host + anchored path.
	u, perr := url.Parse(node)
	if perr != nil {
		return "", "", fmt.Errorf("claude_artifact: invalid url %q: %w", node, perr)
	}
	if u.Scheme != "https" {
		return "", "", fmt.Errorf("claude_artifact: scheme must be https, got %q", u.Scheme)
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host != claudeArtifactHost {
		return "", "", fmt.Errorf("claude_artifact: host must be %s, got %q", claudeArtifactHost, host)
	}
	m := claudeArtifactPathRe.FindStringSubmatch(u.Path)
	if m == nil {
		return "", "", fmt.Errorf("claude_artifact: unexpected path %q", u.Path)
	}
	// Rebuild from verified parts; drop any query/fragment/userinfo/port.
	return m[1], (&url.URL{Scheme: "https", Host: claudeArtifactHost, Path: u.Path}).String(), nil
}
