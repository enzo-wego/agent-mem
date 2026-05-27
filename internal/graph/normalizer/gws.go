package normalizer

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
)

// GWSNormalizer converts Google Workspace (Drive/Docs) bodies to plain text.
type GWSNormalizer struct{}

// NewGWSNormalizer returns a GWSNormalizer.
func NewGWSNormalizer() *GWSNormalizer { return &GWSNormalizer{} }

func (n *GWSNormalizer) Source() string { return "gws" }

// Normalize handles two cases:
//  1. Google Docs JSON (documents.get response): walks body.content paragraphs.
//  2. Raw HTML (Drive export of a Doc as text/html) or unrecognised mimeType:
//     strip tags + entity-unescape.
func (n *GWSNormalizer) Normalize(_ context.Context, raw []byte, meta map[string]any) (Result, error) {
	if len(raw) == 0 {
		return Result{}, nil
	}

	// Try to detect if the input looks like a Docs JSON.
	if looksLikeDocsJSON(raw) {
		return parseDocsJSON(raw)
	}

	// Fall back to HTML stripping.
	return Result{Text: stripHTML(string(raw))}, nil
}

// looksLikeDocsJSON checks cheaply whether raw is a Docs API JSON response.
func looksLikeDocsJSON(raw []byte) bool {
	// A Docs document always has a "body" key near the top.
	return strings.Contains(string(raw[:min(512, len(raw))]), `"body"`)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func parseDocsJSON(raw []byte) (Result, error) {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Result{Text: stripHTML(string(raw))}, nil
	}

	body, ok := doc["body"].(map[string]any)
	if !ok {
		return Result{Text: stripHTML(string(raw))}, nil
	}

	content, ok := body["content"].([]any)
	if !ok {
		return Result{}, nil
	}

	var b strings.Builder
	var mentions []Mention

	for _, elem := range content {
		elemMap, ok := elem.(map[string]any)
		if !ok {
			continue
		}
		para, ok := elemMap["paragraph"].(map[string]any)
		if !ok {
			continue
		}
		elements, ok := para["elements"].([]any)
		if !ok {
			continue
		}
		for _, e := range elements {
			eMap, ok := e.(map[string]any)
			if !ok {
				continue
			}
			tr, ok := eMap["textRun"].(map[string]any)
			if !ok {
				continue
			}
			content, _ := tr["content"].(string)
			b.WriteString(content)

			// Check for person properties (smart chips / rich links).
			if pp, ok := tr["personProperties"].(map[string]any); ok {
				if email, ok := pp["email"].(string); ok && email != "" {
					mentions = append(mentions, Mention{Source: "gws", ExternalID: email})
				}
			}
			if rl, ok := tr["richLink"].(map[string]any); ok {
				if rlProps, ok := rl["richLinkProperties"].(map[string]any); ok {
					if uri, ok := rlProps["uri"].(string); ok && strings.Contains(uri, "@") {
						mentions = append(mentions, Mention{Source: "gws", ExternalID: uri})
					}
				}
			}
		}
		b.WriteString("\n")
	}

	return Result{Text: strings.TrimSpace(b.String()), Mentions: mentions}, nil
}

var (
	htmlTagRe    = regexp.MustCompile(`<[^>]+>`)
	multiSpaceRe = regexp.MustCompile(`[ \t]+`)
	multiNLRe    = regexp.MustCompile(`\n{3,}`)
)

func stripHTML(s string) string {
	s = htmlTagRe.ReplaceAllString(s, " ")
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&quot;", "\"")
	s = strings.ReplaceAll(s, "&apos;", "'")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	s = multiSpaceRe.ReplaceAllString(s, " ")
	s = multiNLRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
