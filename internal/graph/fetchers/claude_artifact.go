package fetchers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/ids"
)

var (
	// claudeArtifactNodeRe matches "claude_artifact:<id>".
	claudeArtifactNodeRe = regexp.MustCompile(`^claude_artifact:([A-Za-z0-9_-]{8,})$`)
	// claudeArtifactURLRe matches a shared/published Claude artifact URL, e.g.
	// https://claude.ai/public/artifacts/<id> or https://claude.ai/code/artifact/<id>.
	claudeArtifactURLRe = regexp.MustCompile(`\bclaude\.ai/(?:public/artifacts|code/artifact)/([A-Za-z0-9_-]{8,})\b`)
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
	return claudeArtifactNodeRe.MatchString(nodeIDorURL) || claudeArtifactURLRe.MatchString(nodeIDorURL)
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

// parseNode returns the artifact id and the URL to GET. A node ID is mapped to
// the public-share URL; a URL is used as-is.
func (f *claudeArtifactFetcher) parseNode(node string) (id, url string, err error) {
	if m := claudeArtifactURLRe.FindStringSubmatch(node); m != nil {
		url = node
		if !strings.HasPrefix(url, "http") {
			url = "https://" + strings.TrimPrefix(url, "//")
		}
		return m[1], url, nil
	}
	if m := claudeArtifactNodeRe.FindStringSubmatch(node); m != nil {
		return m[1], "https://claude.ai/public/artifacts/" + m[1], nil
	}
	return "", "", fmt.Errorf("cannot parse claude_artifact node %q", node)
}
