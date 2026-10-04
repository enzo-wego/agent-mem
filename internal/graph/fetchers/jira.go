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
	Summary     string           `json:"summary"`
	Description json.RawMessage  `json:"description"`
	Created     string           `json:"created"`
	Updated     string           `json:"updated"`
	Reporter    *jiraUser        `json:"reporter"`
	Assignee    *jiraUser        `json:"assignee"`
	Attachment  []jiraAttachment `json:"attachment"`
	IssueLinks  []jiraIssueLink  `json:"issuelinks"`
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

type jiraIssueLink struct {
	OutwardIssue struct {
		Key string `json:"key"`
	} `json:"outwardIssue"`
	InwardIssue struct {
		Key string `json:"key"`
	} `json:"inwardIssue"`
}

type jiraComment struct {
	Author  *jiraUser       `json:"author"`
	Created string          `json:"created"`
	Body    json.RawMessage `json:"body"`
}

type jiraCommentPage struct {
	Total    int           `json:"total"`
	Comments []jiraComment `json:"comments"`
}

type jiraRemoteLink struct {
	Object struct {
		URL   string `json:"url"`
		Title string `json:"title"`
	} `json:"object"`
}

// getJSON keeps authentication and failure handling identical for every section.
func (f *jiraFetcher) getJSON(ctx context.Context, apiURL string, dest any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.SetBasicAuth(f.cfg.JiraEmail, f.cfg.JiraToken)
	req.Header.Set("Accept", "application/json")
	resp, err := f.cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// A deleted or moved issue never comes back through retry. 403 stays
		// retryable: a permission change can fix it.
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
			return &PermanentError{Code: fmt.Sprintf("jira_http_%d", resp.StatusCode)}
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}
	if err := json.NewDecoder(resp.Body).Decode(dest); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return ctx.Err()
}

// jiraADFContent accepts only a document object carrying a content array.
func jiraADFContent(raw json.RawMessage) []json.RawMessage {
	var doc struct {
		Content []json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	return doc.Content
}

func jiraADFParagraph(text string) json.RawMessage {
	raw, _ := json.Marshal(struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}{
		Type: "paragraph",
		Content: []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{{Type: "text", Text: text}},
	})
	return raw
}

// Fetch combines the description, oldest 500 comments, issue keys and remote
// links into one ADF document. Every section must succeed before returning a
// body, since downstream ingestion prunes references absent from that body.
func (f *jiraFetcher) Fetch(ctx context.Context, node string) (FetchedBody, error) {
	key, err := f.parseNode(node)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("jira fetcher: %w", err)
	}

	baseURL := strings.TrimRight(f.cfg.JiraBaseURL, "/")
	apiURL := fmt.Sprintf("%s/rest/api/3/issue/%s?fields=summary,description,status,issuetype,assignee,reporter,creator,labels,created,updated,resolutiondate,parent,attachment,issuelinks", baseURL, key)
	var issueRaw json.RawMessage
	if err := f.getJSON(ctx, apiURL, &issueRaw); err != nil {
		return FetchedBody{}, fmt.Errorf("jira fetcher: %w", err)
	}

	var issue jiraIssueResponse
	if err := json.Unmarshal(issueRaw, &issue); err != nil {
		return FetchedBody{}, fmt.Errorf("jira fetcher: decode response: %w", err)
	}
	content := jiraADFContent(issue.Fields.Description)
	held := 0
	for start := 0; ; {
		var page jiraCommentPage
		apiURL := fmt.Sprintf("%s/rest/api/3/issue/%s/comment?orderBy=created&startAt=%d&maxResults=100", baseURL, key, start)
		if err := f.getJSON(ctx, apiURL, &page); err != nil {
			return FetchedBody{}, fmt.Errorf("jira comments: %w", err)
		}
		if len(page.Comments) == 0 && start < page.Total {
			return FetchedBody{}, fmt.Errorf("jira comments: empty page at startAt=%d of total=%d", start, page.Total)
		}
		comments := page.Comments
		if len(comments) > 500-held {
			comments = comments[:500-held]
		}
		for _, comment := range comments {
			name := "unknown"
			if comment.Author != nil && comment.Author.DisplayName != "" {
				name = comment.Author.DisplayName
			}
			content = append(content, jiraADFParagraph(fmt.Sprintf("--- comment by %s @ %s ---", name, comment.Created)))
			content = append(content, jiraADFContent(comment.Body)...)
		}
		held += len(comments)
		start += len(page.Comments)
		if held == 500 {
			f.log.Warn().Str("key", key).Int("total", page.Total).Msg("jira comments capped at 500")
			break
		}
		if start >= page.Total {
			break
		}
	}

	var remoteLinks []jiraRemoteLink
	apiURL = fmt.Sprintf("%s/rest/api/3/issue/%s/remotelink", baseURL, key)
	if err := f.getJSON(ctx, apiURL, &remoteLinks); err != nil {
		return FetchedBody{}, fmt.Errorf("jira remote links: %w", err)
	}

	var linkedKeys []string
	seen := make(map[string]bool)
	for _, link := range issue.Fields.IssueLinks {
		linkedKey := link.OutwardIssue.Key
		if linkedKey == "" {
			linkedKey = link.InwardIssue.Key
		}
		if linkedKey != "" && !seen[linkedKey] {
			seen[linkedKey] = true
			linkedKeys = append(linkedKeys, linkedKey)
		}
	}
	if len(linkedKeys) > 0 {
		content = append(content, jiraADFParagraph("Linked issues: "+strings.Join(linkedKeys, ", ")))
	}
	for _, link := range remoteLinks {
		content = append(content, jiraADFParagraph(fmt.Sprintf("Link: %s (%s)", link.Object.Title, link.Object.URL)))
	}
	if content == nil {
		content = []json.RawMessage{}
	}
	doc, err := json.Marshal(struct {
		Type    string            `json:"type"`
		Version int               `json:"version"`
		Content []json.RawMessage `json:"content"`
	}{Type: "doc", Version: 1, Content: content})
	if err != nil {
		return FetchedBody{}, fmt.Errorf("jira fetcher: encode ADF: %w", err)
	}

	// Raw keeps the full issue shape ({"key", "fields": {...}}) so the normalizer
	// lifts metadata from fields.*; only fields.description is replaced by the
	// composite document (description + comments + links).
	var envelope struct {
		Fields map[string]json.RawMessage `json:"fields"`
	}
	if err := json.Unmarshal(issueRaw, &envelope); err != nil {
		return FetchedBody{}, fmt.Errorf("jira fetcher: decode response: %w", err)
	}
	if envelope.Fields == nil {
		envelope.Fields = map[string]json.RawMessage{}
	}
	envelope.Fields["description"] = doc
	raw, err := json.Marshal(struct {
		Key    string                     `json:"key"`
		Fields map[string]json.RawMessage `json:"fields"`
	}{Key: issue.Key, Fields: envelope.Fields})
	if err != nil {
		return FetchedBody{}, fmt.Errorf("jira fetcher: encode raw: %w", err)
	}

	bodyTS := ParseJiraTime(issue.Fields.Updated)
	createdAt := ParseJiraTime(issue.Fields.Created)

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
			NodeID:     fmt.Sprintf("%s:%s", ids.TypeJiraAttachment, a.ID),
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
		CreatedAt:   createdAt,
		Attachments: attachments,
	}, nil
}

// ParseJiraTime parses Jira's timestamps. Jira returns "2006-01-02T15:04:05.000-0700"
// (numeric tz offset, no colon), which isn't quite RFC3339; try both. Zero on failure.
func ParseJiraTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{"2006-01-02T15:04:05.000-0700", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
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
