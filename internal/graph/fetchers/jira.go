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
	jiraNodeRe = regexp.MustCompile(`^jira:([A-Z][A-Z0-9]+-\d+)$`)
	jiraURLRe  = regexp.MustCompile(`\batlassian\.net/browse/([A-Z][A-Z0-9]+-\d+)\b`)
)

// jiraFetcher retrieves Jira issue bodies via the REST API v3.
type jiraFetcher struct {
	cfg Config
	log zerolog.Logger
}

func newJiraFetcher(cfg Config, log zerolog.Logger) *jiraFetcher {
	return &jiraFetcher{cfg: cfg, log: log}
}

func (f *jiraFetcher) Source() string { return "jira" }

// Matches returns true for jira:<KEY> node IDs or Jira browse URLs.
func (f *jiraFetcher) Matches(nodeIDorURL string) bool {
	if jiraNodeRe.MatchString(nodeIDorURL) {
		return true
	}
	return jiraURLRe.MatchString(nodeIDorURL)
}

// jiraIssueResponse is the minimal shape of a Jira issue API response.
type jiraIssueResponse struct {
	Key    string     `json:"key"`
	Fields jiraFields `json:"fields"`
}

type jiraFields struct {
	Summary     string          `json:"summary"`
	Description json.RawMessage `json:"description"`
	Updated     string          `json:"updated"`
	Reporter    *jiraUser       `json:"reporter"`
	Assignee    *jiraUser       `json:"assignee"`
	Attachment  []jiraAttachment `json:"attachment"`
}

type jiraUser struct {
	AccountID    string `json:"accountId"`
	DisplayName  string `json:"displayName"`
	EmailAddress string `json:"emailAddress"`
}

type jiraAttachment struct {
	ID       string `json:"id"`
	Filename string `json:"filename"`
	MimeType string `json:"mimeType"`
	Size     int64  `json:"size"`
	Content  string `json:"content"`
}

// Fetch retrieves the Jira issue.
func (f *jiraFetcher) Fetch(ctx context.Context, node string) (FetchedBody, error) {
	key, err := f.parseNode(node)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("jira fetcher: %w", err)
	}

	baseURL := strings.TrimRight(f.cfg.JiraBaseURL, "/")
	apiURL := fmt.Sprintf("%s/rest/api/3/issue/%s?fields=summary,description,status,assignee,reporter,creator,labels,updated,attachment", baseURL, key)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("jira fetcher: build request: %w", err)
	}
	req.SetBasicAuth(f.cfg.JiraEmail, f.cfg.JiraToken)
	req.Header.Set("Accept", "application/json")

	resp, err := f.cfg.HTTPClient.Do(req)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("jira fetcher: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return FetchedBody{}, fmt.Errorf("jira fetcher status %d: %s", resp.StatusCode, string(body))
	}

	var issue jiraIssueResponse
	if err := json.NewDecoder(resp.Body).Decode(&issue); err != nil {
		return FetchedBody{}, fmt.Errorf("jira fetcher: decode response: %w", err)
	}

	// Raw = JSON of the ADF description object so the Jira normalizer can walk it.
	raw := []byte(issue.Fields.Description)
	if len(raw) == 0 || string(raw) == "null" {
		raw = []byte("{}")
	}

	bodyTS := time.Time{}
	if issue.Fields.Updated != "" {
		if t, err := time.Parse(time.RFC3339, issue.Fields.Updated); err == nil {
			bodyTS = t
		}
	}

	var author AuthorRef
	if r := issue.Fields.Reporter; r != nil {
		author = AuthorRef{
			Source:      "jira",
			ExternalID:  r.AccountID,
			DisplayName: r.DisplayName,
			Email:       r.EmailAddress,
		}
	}

	var attachments []Attachment
	for _, a := range issue.Fields.Attachment {
		attachments = append(attachments, Attachment{
			NodeID:     fmt.Sprintf("jira_attachment:%s", a.ID),
			MimeType:   a.MimeType,
			Filename:   a.Filename,
			SizeBytes:  a.Size,
			URLPrivate: a.Content,
		})
	}

	nodeID, _ := ids.Jira(key)
	return FetchedBody{
		NodeID:      nodeID,
		Type:        ids.TypeJira,
		URL:         fmt.Sprintf("%s/browse/%s", baseURL, key),
		Title:       issue.Fields.Summary,
		Raw:         raw,
		ContentType: "application/json",
		Author:      author,
		BodyTS:      bodyTS,
		Attachments: attachments,
	}, nil
}

// parseNode extracts the Jira key from a node ID or browse URL.
func (f *jiraFetcher) parseNode(node string) (string, error) {
	if m := jiraNodeRe.FindStringSubmatch(node); m != nil {
		return m[1], nil
	}
	if m := jiraURLRe.FindStringSubmatch(node); m != nil {
		return m[1], nil
	}
	return "", fmt.Errorf("cannot parse jira node %q", node)
}
