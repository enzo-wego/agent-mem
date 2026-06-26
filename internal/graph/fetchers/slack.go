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
	slackNodeRe = regexp.MustCompile(`^slack:(C\w+):([\d.]+)$`)
	slackURLRe  = regexp.MustCompile(`\bwego\.slack\.com/archives/(C\w+)/p(\d+)\b`)
)

// slackFetcher fetches Slack thread or message bodies via the Web API.
type slackFetcher struct {
	cfg Config
	log zerolog.Logger
}

func newSlackFetcher(cfg Config, log zerolog.Logger) *slackFetcher {
	return &slackFetcher{cfg: cfg, log: log}
}

func (f *slackFetcher) Source() string { return "slack" }

// Matches returns true for slack:<channel>:<ts> node IDs or slack archive URLs.
func (f *slackFetcher) Matches(nodeIDorURL string) bool {
	if slackNodeRe.MatchString(nodeIDorURL) {
		return true
	}
	return slackURLRe.MatchString(nodeIDorURL)
}

// slackAPIResponse is the minimal shape of a Slack conversations.* response.
type slackAPIResponse struct {
	OK       bool             `json:"ok"`
	Error    string           `json:"error"`
	Messages []slackMessage   `json:"messages"`
}

type slackMessage struct {
	Type    string       `json:"type"`
	User    string       `json:"user"`
	BotID   string       `json:"bot_id"`
	Text    string       `json:"text"`
	Ts      string       `json:"ts"`
	ThreadTs string      `json:"thread_ts"`
	Files   []slackFile  `json:"files"`
	// Attachments holds shared/forwarded messages and link unfurls. A forwarded
	// message keeps its real content here (author_name, text, nested files), not in
	// the top-level Text — so without this a "FYI @x" share shows nothing.
	Attachments []slackAttachment `json:"attachments"`
}

type slackAttachment struct {
	AuthorName string      `json:"author_name"`
	Title      string      `json:"title"`
	Text       string      `json:"text"`
	Fallback   string      `json:"fallback"`
	Files      []slackFile `json:"files"`
}

type slackFile struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Mimetype string `json:"mimetype"`
	Size     int64  `json:"size"`
	URLPrivate string `json:"url_private"`
	Thumb360 string `json:"thumb_360"`
}

// Fetch retrieves messages for a slack node ID or archive URL.
func (f *slackFetcher) Fetch(ctx context.Context, node string) (FetchedBody, error) {
	channel, ts, err := f.parseNode(node)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("slack fetcher: %w", err)
	}

	msgs, err := f.fetchMessages(ctx, channel, ts)
	if err != nil {
		return FetchedBody{}, err
	}
	if len(msgs) == 0 {
		return FetchedBody{}, fmt.Errorf("slack fetcher: no messages returned for %s", node)
	}

	// Build body text: parent + replies, each including any shared/forwarded content.
	var sb strings.Builder
	appendMsg := func(m slackMessage) {
		sb.WriteString(m.Text)
		for _, at := range m.Attachments {
			text := at.Text
			if text == "" {
				text = at.Fallback
			}
			if at.AuthorName == "" && at.Title == "" && text == "" {
				continue
			}
			sb.WriteString("\n\n--- shared")
			if at.AuthorName != "" {
				sb.WriteString(" from " + at.AuthorName)
			}
			sb.WriteString(" ---\n")
			if at.Title != "" {
				sb.WriteString(at.Title + "\n")
			}
			sb.WriteString(text)
		}
	}
	parent := msgs[0]
	appendMsg(parent)

	for _, msg := range msgs[1:] {
		sb.WriteString(fmt.Sprintf("\n\n--- reply by %s @ %s ---\n\n", msg.User, msg.Ts))
		appendMsg(msg)
	}
	bodyText := sb.String()

	// Title = first 80 chars of parent.
	title := parent.Text
	if len(title) > 80 {
		title = title[:80]
	}

	// Collect file attachments — both directly attached files and files inside
	// shared/forwarded messages (e.g. a PDF in a forwarded message).
	var attachments []Attachment
	addFile := func(fi slackFile) {
		attachments = append(attachments, Attachment{
			NodeID:     ids.SlackFile(fi.ID),
			MimeType:   fi.Mimetype,
			Filename:   fi.Name,
			SizeBytes:  fi.Size,
			URLPrivate: fi.URLPrivate,
			ThumbURL:   fi.Thumb360,
		})
	}
	for _, msg := range msgs {
		for _, fi := range msg.Files {
			addFile(fi)
		}
		for _, at := range msg.Attachments {
			for _, fi := range at.Files {
				addFile(fi)
			}
		}
	}

	// Parse BodyTS from the parent message ts.
	bodyTS := parseSlackTS(parent.Ts)

	nodeID := ids.SlackThread(channel, ts)
	return FetchedBody{
		NodeID:      nodeID,
		Type:        ids.TypeSlackThread,
		URL:         fmt.Sprintf("https://wego.slack.com/archives/%s/p%s", channel, strings.ReplaceAll(ts, ".", "")),
		Title:       title,
		Raw:         []byte(bodyText),
		ContentType: "text/plain",
		Author: AuthorRef{
			Source:     "slack",
			ExternalID: parent.User,
			IsBot:      parent.BotID != "",
		},
		BodyTS:      bodyTS,
		Attachments: attachments,
	}, nil
}

// parseNode extracts channel and ts from a node ID or Slack archive URL.
func (f *slackFetcher) parseNode(node string) (channel, ts string, err error) {
	if m := slackNodeRe.FindStringSubmatch(node); m != nil {
		return m[1], m[2], nil
	}
	if m := slackURLRe.FindStringSubmatch(node); m != nil {
		return m[1], slackTSFromRaw(m[2]), nil
	}
	return "", "", fmt.Errorf("cannot parse slack node %q", node)
}

// slackTSFromRaw converts a raw p-timestamp (no dot) to dotted format.
func slackTSFromRaw(raw string) string {
	if len(raw) <= 6 {
		return raw
	}
	return raw[:len(raw)-6] + "." + raw[len(raw)-6:]
}

// fetchMessages calls conversations.replies or conversations.history.
func (f *slackFetcher) fetchMessages(ctx context.Context, channel, ts string) ([]slackMessage, error) {
	// Always try replies first; if only one message comes back it's a single-message fetch.
	apiURL := fmt.Sprintf("https://slack.com/api/conversations.replies?channel=%s&ts=%s&limit=200", channel, ts)
	var resp slackAPIResponse
	if err := f.doGet(ctx, apiURL, &resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		// Fall back to conversations.history for a single message.
		histURL := fmt.Sprintf("https://slack.com/api/conversations.history?channel=%s&latest=%s&inclusive=true&limit=1", channel, ts)
		if err := f.doGet(ctx, histURL, &resp); err != nil {
			return nil, err
		}
		if !resp.OK {
			return nil, fmt.Errorf("slack fetcher: API error: %s", resp.Error)
		}
	}
	return resp.Messages, nil
}

// doGet issues a GET with the bot token and decodes JSON into dst.
func (f *slackFetcher) doGet(ctx context.Context, url string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("slack fetcher: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+f.cfg.SlackBotToken)

	resp, err := f.cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("slack fetcher: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("slack fetcher status %d: %s", resp.StatusCode, string(body))
	}

	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		return fmt.Errorf("slack fetcher: decode response: %w", err)
	}
	return nil
}

// parseSlackTS converts a dotted Slack ts (e.g. "1779710863.216389") to time.Time.
func parseSlackTS(ts string) time.Time {
	// ts format: "<unix_seconds>.<microseconds>"
	dot := strings.Index(ts, ".")
	if dot < 0 {
		return time.Time{}
	}
	sec := ts[:dot]
	var unixSec int64
	for _, c := range sec {
		if c < '0' || c > '9' {
			return time.Time{}
		}
		unixSec = unixSec*10 + int64(c-'0')
	}
	return time.Unix(unixSec, 0).UTC()
}
