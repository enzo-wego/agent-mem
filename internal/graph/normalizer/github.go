package normalizer

import (
	"context"
	"regexp"
	"strings"
)

// gitHubMentionRe matches @login references in GitHub Markdown.
var gitHubMentionRe = regexp.MustCompile(`@([A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?)`)

// htmlCommentRe strips HTML comments (e.g. CodeRabbit hidden state).
var htmlCommentRe = regexp.MustCompile(`(?s)<!--.*?-->`)

// frontMatterRe strips YAML/TOML front-matter at the very start of the body.
var frontMatterRe = regexp.MustCompile(`(?s)\A---\n.*?\n---\n?`)

// multiNewlineRe collapses 3+ consecutive newlines to 2.
var multiNewlineRe = regexp.MustCompile(`\n{3,}`)

// GitHubNormalizer converts GitHub-Flavored Markdown to plain text.
type GitHubNormalizer struct{}

// NewGitHubNormalizer returns a GitHubNormalizer.
func NewGitHubNormalizer() *GitHubNormalizer { return &GitHubNormalizer{} }

func (n *GitHubNormalizer) Source() string { return "github" }

// Normalize applies light cleanup to GFM. Fenced code blocks are preserved
// intact. @mentions are detected and emitted as Mention entries.
func (n *GitHubNormalizer) Normalize(_ context.Context, raw []byte, _ map[string]any) (Result, error) {
	if len(raw) == 0 {
		return Result{}, nil
	}

	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	// Strip front-matter.
	text = frontMatterRe.ReplaceAllString(text, "")

	// Strip HTML comments.
	text = htmlCommentRe.ReplaceAllString(text, "")

	// Collapse triple+ newlines.
	text = multiNewlineRe.ReplaceAllString(text, "\n\n")

	text = strings.TrimSpace(text)

	// Detect @mentions — skip those inside fenced code blocks.
	var mentions []Mention
	seen := make(map[string]bool)
	outsideFence := splitOutsideFences(text)
	for _, segment := range outsideFence {
		for _, m := range gitHubMentionRe.FindAllStringSubmatch(segment, -1) {
			login := m[1]
			if !seen[login] {
				seen[login] = true
				mentions = append(mentions, Mention{Source: "github", ExternalID: login})
			}
		}
	}

	return Result{Text: text, Mentions: mentions}, nil
}

// splitOutsideFences returns the non-fenced-code-block segments of text.
func splitOutsideFences(text string) []string {
	lines := strings.Split(text, "\n")
	var segments []string
	var cur strings.Builder
	inFence := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if !inFence {
			cur.WriteString(line)
			cur.WriteByte('\n')
		}
	}
	if cur.Len() > 0 {
		segments = append(segments, cur.String())
	}
	return segments
}
