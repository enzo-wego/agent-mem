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
	ghNodeRe = regexp.MustCompile(`^gh_pr:([a-zA-Z0-9_.\-]+/[a-zA-Z0-9_.\-]+)#(\d+)$`)
	ghURLRe  = regexp.MustCompile(`\bgithub\.com/([a-zA-Z0-9_.\-]+/[a-zA-Z0-9_.\-]+)/pull/(\d+)\b`)
)

// gitHubFetcher retrieves GitHub PR bodies and comments.
type gitHubFetcher struct {
	cfg Config
	log zerolog.Logger
}

func newGitHubFetcher(cfg Config, log zerolog.Logger) *gitHubFetcher {
	return &gitHubFetcher{cfg: cfg, log: log}
}

func (f *gitHubFetcher) Source() string { return "github" }

// Matches returns true for gh_pr:<repo>#<N> node IDs or GitHub PR URLs.
func (f *gitHubFetcher) Matches(nodeIDorURL string) bool {
	if ghNodeRe.MatchString(nodeIDorURL) {
		return true
	}
	return ghURLRe.MatchString(nodeIDorURL)
}

type ghPR struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	User      ghUser    `json:"user"`
}

type ghUser struct {
	Login string `json:"login"`
}

type ghComment struct {
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	User      ghUser    `json:"user"`
}

// Fetch retrieves the PR body plus review comments and issue comments.
func (f *gitHubFetcher) Fetch(ctx context.Context, node string) (FetchedBody, error) {
	repo, number, err := f.parseNode(node)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("github fetcher: %w", err)
	}

	base := strings.TrimRight(f.cfg.GHBaseURL, "/")
	prURL := fmt.Sprintf("%s/repos/%s/pulls/%s", base, repo, number)

	var pr ghPR
	if err := f.doGet(ctx, prURL, &pr); err != nil {
		return FetchedBody{}, err
	}

	// Fetch PR review comments.
	var reviewComments []ghComment
	reviewURL := fmt.Sprintf("%s/repos/%s/pulls/%s/comments", base, repo, number)
	_ = f.doGet(ctx, reviewURL, &reviewComments) // best-effort

	// Fetch issue comments.
	var issueComments []ghComment
	issueURL := fmt.Sprintf("%s/repos/%s/issues/%s/comments", base, repo, number)
	_ = f.doGet(ctx, issueURL, &issueComments) // best-effort

	// Build combined body.
	var sb strings.Builder
	sb.WriteString(pr.Body)

	allComments := append(reviewComments, issueComments...)
	if len(allComments) > 0 {
		sb.WriteString("\n\n## Comments\n\n")
		for _, c := range allComments {
			sb.WriteString(fmt.Sprintf("### @%s at %s\n\n%s\n\n", c.User.Login, c.CreatedAt.Format(time.RFC3339), c.Body))
		}
	}

	numStr := number
	nodeID, _ := ids.GHPR(repo, atoi(numStr))
	return FetchedBody{
		NodeID:      nodeID,
		Type:        ids.TypeGHPR,
		URL:         pr.HTMLURL,
		Title:       pr.Title,
		Raw:         []byte(sb.String()),
		ContentType: "text/markdown",
		Author: AuthorRef{
			Source:      "github",
			ExternalID:  pr.User.Login,
			DisplayName: pr.User.Login,
		},
		BodyTS:    pr.UpdatedAt,
		CreatedAt: pr.CreatedAt,
	}, nil
}

// parseNode extracts repo and PR number from a node ID or GitHub URL.
func (f *gitHubFetcher) parseNode(node string) (repo, number string, err error) {
	if m := ghNodeRe.FindStringSubmatch(node); m != nil {
		return m[1], m[2], nil
	}
	if m := ghURLRe.FindStringSubmatch(node); m != nil {
		return m[1], m[2], nil
	}
	return "", "", fmt.Errorf("cannot parse github node %q", node)
}

// doGet issues a GET with the bearer token and decodes JSON into dst.
func (f *gitHubFetcher) doGet(ctx context.Context, url string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("github fetcher: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+f.cfg.GHToken)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := f.cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("github fetcher: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("github fetcher status %d: %s", resp.StatusCode, string(body))
	}

	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		return fmt.Errorf("github fetcher: decode response: %w", err)
	}
	return nil
}

// atoi converts a decimal string to int, returning 0 on error.
func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}
