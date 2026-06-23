package normalizer

import (
	"context"
	"regexp"
	"strings"
)

// slackTokenRe matches all Slack mrkdwn tokens of the form <...>.
// It captures everything inside the angle brackets.
var slackTokenRe = regexp.MustCompile(`<([^>]*)>`)

// SlackNormalizer converts a Slack message.text (mrkdwn) to plain UTF-8 text.
type SlackNormalizer struct {
	cache Cache
}

// NewSlackNormalizer returns a SlackNormalizer backed by cache.
func NewSlackNormalizer(cache Cache) *SlackNormalizer {
	return &SlackNormalizer{cache: cache}
}

func (n *SlackNormalizer) Source() string { return "slack" }

// Normalize applies Slack mrkdwn → plain-text transformation.
func (n *SlackNormalizer) Normalize(ctx context.Context, raw []byte, _ map[string]any) (Result, error) {
	if len(raw) == 0 {
		return Result{}, nil
	}
	text := string(raw)
	var mentions []Mention

	// Replace all <...> Slack tokens before any HTML entity unescaping.
	text = slackTokenRe.ReplaceAllStringFunc(text, func(match string) string {
		inner := match[1 : len(match)-1] // strip < and >

		switch {
		// User mention: <@Uxxxxxxx> or <@Uxxxxxxx|name>
		case strings.HasPrefix(inner, "@U") || strings.HasPrefix(inner, "@W"):
			id, _, _ := strings.Cut(inner[1:], "|") // strip leading @
			displayName, _ := n.cache.DisplayName(ctx, "slack", id)
			if displayName == "" {
				displayName = id
			}
			mentions = append(mentions, Mention{Source: "slack", ExternalID: id, DisplayName: displayName})
			return "@" + displayName

		// Channel: <#Cxxxxxxx> or <#Cxxxxxxx|name>
		case strings.HasPrefix(inner, "#"):
			id, label, hasLabel := strings.Cut(inner[1:], "|")
			if hasLabel && label != "" {
				return "#" + label
			}
			if name, ok := n.cache.DisplayName(ctx, "slack_channel", id); ok {
				return "#" + name
			}
			return "#" + id

		// Subteam: <!subteam^SID> or <!subteam^SID|handle>
		case strings.HasPrefix(inner, "!subteam^"):
			rest := inner[len("!subteam^"):]
			id, label, hasLabel := strings.Cut(rest, "|")
			if hasLabel && label != "" {
				// Strip a leading @ from the alias if present (Slack includes it).
				display := strings.TrimPrefix(label, "@")
				mentions = append(mentions, Mention{Source: "slack_group", ExternalID: id, DisplayName: display})
				return "@" + display
			}
			if name, ok := n.cache.DisplayName(ctx, "slack_group", id); ok {
				mentions = append(mentions, Mention{Source: "slack_group", ExternalID: id, DisplayName: name})
				return "@" + name
			}
			mentions = append(mentions, Mention{Source: "slack_group", ExternalID: id})
			return "@" + id

		// Broadcast: <!here>, <!channel>, <!everyone>
		case inner == "!here":
			return "@here"
		case inner == "!channel":
			return "@channel"
		case inner == "!everyone":
			return "@everyone"

		// URL: <http://...> or <http://...|label>
		// The label may itself contain | — use the LAST | as separator.
		case strings.HasPrefix(inner, "http://") || strings.HasPrefix(inner, "https://"):
			lastPipe := strings.LastIndex(inner, "|")
			if lastPipe != -1 {
				url := inner[:lastPipe]
				label := inner[lastPipe+1:]
				return label + " (" + url + ")"
			}
			return inner

		default:
			// Unknown token — emit inner content as-is.
			return inner
		}
	})

	// Strip Slack formatting wrappers: *bold*, _italic_, ~strike~.
	// Only strip when the wrapper chars are at word boundaries (simple approach:
	// replace *x* → x, _x_ → x, ~x~ → x using regexp).
	text = stripSlackFormatting(text)

	// HTML entity unescape — LAST so encoded brackets in text content
	// come out as literal < > rather than being re-parsed as tokens.
	text = strings.ReplaceAll(text, "&amp;", "&")
	text = strings.ReplaceAll(text, "&lt;", "<")
	text = strings.ReplaceAll(text, "&gt;", ">")

	return Result{Text: text, Mentions: mentions}, nil
}

var (
	boldRe = regexp.MustCompile(`\*([^*]+)\*`)
	// italicRe only matches _word_ when surrounded by word boundaries (space,
	// start/end of string, or punctuation), to avoid mangling URL query params
	// like thread_ts=... where _ is not a formatting delimiter.
	italicRe = regexp.MustCompile(`(?:^|[\s(,])_([^_\s][^_]*)_(?:$|[\s),.:;!?])`)
	strikeRe = regexp.MustCompile(`~([^~]+)~`)
)

func stripSlackFormatting(s string) string {
	s = boldRe.ReplaceAllString(s, "$1")
	// italicRe has a surrounding-context capture group at index 1 and the
	// italic content at index 2; preserve the leading context char.
	s = italicRe.ReplaceAllStringFunc(s, func(match string) string {
		groups := italicRe.FindStringSubmatch(match)
		if len(groups) < 2 {
			return match
		}
		// groups[0] = full match (may include leading space/punct)
		// groups[1] = inner text
		// Re-insert the leading context character if present.
		leading := ""
		if len(match) > 0 && match[0] != '_' {
			leading = string(match[0])
		}
		trailing := ""
		if len(match) > 0 && match[len(match)-1] != '_' {
			trailing = string(match[len(match)-1])
		}
		return leading + groups[1] + trailing
	})
	s = strikeRe.ReplaceAllString(s, "$1")
	return s
}
