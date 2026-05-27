package fetchers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/ids"
)

var (
	sentryNodeRe = regexp.MustCompile(`^sentry:([A-Z0-9_\-]+)$`)
	sentryURLRe  = regexp.MustCompile(`\bsentry\.io/[\w-]+/[\w-]+/issues/(\w+)/?`)
)

// sentryFetcher retrieves Sentry issue bodies via the REST API.
type sentryFetcher struct {
	cfg Config
	log zerolog.Logger
}

func newSentryFetcher(cfg Config, log zerolog.Logger) *sentryFetcher {
	return &sentryFetcher{cfg: cfg, log: log}
}

func (f *sentryFetcher) Source() string { return "sentry" }

// Matches returns true for sentry:<id> node IDs or Sentry issue URLs.
func (f *sentryFetcher) Matches(nodeIDorURL string) bool {
	if sentryNodeRe.MatchString(nodeIDorURL) {
		return true
	}
	return sentryURLRe.MatchString(nodeIDorURL)
}

type sentryIssueResponse struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	LastSeen time.Time `json:"lastSeen"`
	Actor    *sentryActor `json:"actor"`
	Permalink string   `json:"permalink"`
}

type sentryActor struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Fetch retrieves the Sentry issue.
func (f *sentryFetcher) Fetch(ctx context.Context, node string) (FetchedBody, error) {
	issueID, err := f.parseNode(node)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("sentry fetcher: %w", err)
	}

	base := strings.TrimRight(f.cfg.SentryBaseURL, "/")
	apiURL := fmt.Sprintf("%s/api/0/issues/%s/", base, issueID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("sentry fetcher: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+f.cfg.SentryAuthToken)

	resp, err := f.cfg.HTTPClient.Do(req)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("sentry fetcher: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return FetchedBody{}, fmt.Errorf("sentry fetcher status %d: %s", resp.StatusCode, string(body))
	}

	rawBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("sentry fetcher: read body: %w", err)
	}

	var issue sentryIssueResponse
	if err := json.Unmarshal(rawBytes, &issue); err != nil {
		return FetchedBody{}, fmt.Errorf("sentry fetcher: decode response: %w", err)
	}

	var author AuthorRef
	if issue.Actor != nil {
		author = AuthorRef{
			Source:      "sentry",
			ExternalID:  issue.Actor.ID,
			DisplayName: issue.Actor.Name,
			Email:       issue.Actor.Email,
		}
	}

	permalink := issue.Permalink
	if permalink == "" {
		permalink = fmt.Sprintf("%s/issues/%s/", base, issueID)
	}

	nodeID, _ := ids.Sentry(issueID)
	return FetchedBody{
		NodeID:      nodeID,
		Type:        ids.TypeSentry,
		URL:         permalink,
		Title:       issue.Title,
		Raw:         rawBytes,
		ContentType: "application/json",
		Author:      author,
		BodyTS:      issue.LastSeen,
	}, nil
}

// parseNode extracts the issue ID from a node ID or Sentry URL.
func (f *sentryFetcher) parseNode(node string) (string, error) {
	if m := sentryNodeRe.FindStringSubmatch(node); m != nil {
		return m[1], nil
	}
	if m := sentryURLRe.FindStringSubmatch(node); m != nil {
		return m[1], nil
	}
	return "", fmt.Errorf("cannot parse sentry node %q", node)
}
