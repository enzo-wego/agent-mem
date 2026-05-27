package fetchers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/ids"
)

var (
	cfNodeRe = regexp.MustCompile(`^cf:(\d+)$`)
	cfURLRe  = regexp.MustCompile(`\batlassian\.net/wiki/[^\s]*?pages/(\d+)\b`)
)

// confluenceFetcher retrieves Confluence page bodies via the v2 REST API.
type confluenceFetcher struct {
	cfg Config
	log zerolog.Logger
}

func newConfluenceFetcher(cfg Config, log zerolog.Logger) *confluenceFetcher {
	return &confluenceFetcher{cfg: cfg, log: log}
}

func (f *confluenceFetcher) Source() string { return "confluence" }

// Matches returns true for cf:<id> node IDs or Confluence page URLs.
func (f *confluenceFetcher) Matches(nodeIDorURL string) bool {
	if cfNodeRe.MatchString(nodeIDorURL) {
		return true
	}
	return cfURLRe.MatchString(nodeIDorURL)
}

type cfPageResponse struct {
	ID      string  `json:"id"`
	Title   string  `json:"title"`
	Body    cfBody  `json:"body"`
	Version cfVersion `json:"version"`
}

type cfBody struct {
	Storage cfStorage `json:"storage"`
}

type cfStorage struct {
	Value string `json:"value"`
}

type cfVersion struct {
	AuthorID  string    `json:"authorId"`
	CreatedAt time.Time `json:"createdAt"`
}

// Fetch retrieves the Confluence page.
func (f *confluenceFetcher) Fetch(ctx context.Context, node string) (FetchedBody, error) {
	pageID, err := f.parseNode(node)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("confluence fetcher: %w", err)
	}

	// Prefer CFBaseURL; fall back to JiraBaseURL + /wiki.
	baseURL := strings.TrimRight(f.cfg.CFBaseURL, "/")
	if baseURL == "" {
		baseURL = strings.TrimRight(f.cfg.JiraBaseURL, "/") + "/wiki"
	}
	apiURL := fmt.Sprintf("%s/api/v2/pages/%s?body-format=storage", baseURL, pageID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("confluence fetcher: build request: %w", err)
	}

	// Use CFToken if set; otherwise fall back to Jira basic auth.
	if f.cfg.CFToken != "" {
		req.SetBasicAuth(f.cfg.JiraEmail, f.cfg.CFToken)
	} else {
		req.SetBasicAuth(f.cfg.JiraEmail, f.cfg.JiraToken)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := f.cfg.HTTPClient.Do(req)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("confluence fetcher: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return FetchedBody{}, fmt.Errorf("confluence fetcher status %d: %s", resp.StatusCode, string(body))
	}

	var page cfPageResponse
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return FetchedBody{}, fmt.Errorf("confluence fetcher: decode response: %w", err)
	}

	id64, _ := strconv.ParseInt(pageID, 10, 64)
	nodeID := ids.CFPage(id64)

	return FetchedBody{
		NodeID:      nodeID,
		Type:        ids.TypeCFPage,
		URL:         fmt.Sprintf("%s/pages/%s", baseURL, pageID),
		Title:       page.Title,
		Raw:         []byte(page.Body.Storage.Value),
		ContentType: "application/xhtml+xml",
		Author: AuthorRef{
			Source:     "confluence",
			ExternalID: page.Version.AuthorID,
		},
		BodyTS: page.Version.CreatedAt,
	}, nil
}

// parseNode extracts the page ID from a node ID or Confluence URL.
func (f *confluenceFetcher) parseNode(node string) (string, error) {
	if m := cfNodeRe.FindStringSubmatch(node); m != nil {
		return m[1], nil
	}
	if m := cfURLRe.FindStringSubmatch(node); m != nil {
		return m[1], nil
	}
	return "", fmt.Errorf("cannot parse confluence node %q", node)
}
