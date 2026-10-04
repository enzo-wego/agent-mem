package normalizer

import (
	"context"
	"encoding/json"
	"strings"
)

// JiraNormalizer converts Atlassian Document Format (ADF) JSON to plain text.
type JiraNormalizer struct{}

// NewJiraNormalizer returns a JiraNormalizer.
func NewJiraNormalizer() *JiraNormalizer { return &JiraNormalizer{} }

func (n *JiraNormalizer) Source() string { return "jira" }

// Normalize accepts either a bare ADF document (the issue description) or a
// full REST issue payload (`{"key":…,"fields":{…}}`). For the latter it walks
// `fields.description` as ADF and lifts the fields later rounds filter on into
// Result.Metadata (see jiraFieldMetadata). If the input is not valid JSON, it
// falls back to returning the raw bytes as text.
func (n *JiraNormalizer) Normalize(_ context.Context, raw []byte, _ map[string]any) (Result, error) {
	if len(raw) == 0 {
		return Result{}, nil
	}

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		// Fallback: return raw as text.
		return Result{Text: string(raw)}, nil
	}

	var meta map[string]any
	if fields, ok := doc["fields"].(map[string]any); ok {
		meta = jiraFieldMetadata(fields)
		doc, _ = fields["description"].(map[string]any)
		if doc == nil {
			return Result{Metadata: meta}, nil
		}
	}

	var b strings.Builder
	var mentions []Mention
	walkADF(doc, &b, &mentions)

	return Result{Text: strings.TrimSpace(b.String()), Mentions: mentions, Metadata: meta}, nil
}

// jiraFieldMetadata lifts the issue fields the graph keeps on the node. Every
// key is always present (nil when the issue lacks the value) so a jsonb merge
// clears a resolution/parent/assignee that was removed at the source instead
// of keeping the stale value.
func jiraFieldMetadata(fields map[string]any) map[string]any {
	name := func(key string) any {
		obj, _ := fields[key].(map[string]any)
		if s, ok := obj["name"].(string); ok && s != "" {
			return s
		}
		return nil
	}
	str := func(key string) any {
		if s, ok := fields[key].(string); ok && s != "" {
			return s
		}
		return nil
	}
	labels := []string{}
	if raw, ok := fields["labels"].([]any); ok {
		for _, l := range raw {
			if s, ok := l.(string); ok && s != "" {
				labels = append(labels, s)
			}
		}
	}
	var assignee any
	if a, ok := fields["assignee"].(map[string]any); ok {
		if id, ok := a["accountId"].(string); ok && id != "" {
			assignee = id
		}
	}
	var parent any
	if p, ok := fields["parent"].(map[string]any); ok {
		if k, ok := p["key"].(string); ok && k != "" {
			parent = k
		}
	}
	return map[string]any{
		"status":              name("status"),
		"issuetype":           name("issuetype"),
		"created":             str("created"),
		"updated":             str("updated"),
		"resolutiondate":      str("resolutiondate"),
		"labels":              labels,
		"assignee_account_id": assignee,
		"parent_key":          parent,
	}
}

// walkADF recursively walks an ADF node represented as map[string]any.
func walkADF(node map[string]any, b *strings.Builder, mentions *[]Mention) {
	nodeType, _ := node["type"].(string)

	switch nodeType {
	case "text":
		text, _ := node["text"].(string)
		marks, _ := node["marks"].([]any)
		if len(marks) == 0 {
			b.WriteString(text)
			return
		}
		// Process marks. For link marks, wrap text with URL.
		linkURL := ""
		for _, m := range marks {
			mark, ok := m.(map[string]any)
			if !ok {
				continue
			}
			markType, _ := mark["type"].(string)
			if markType == "link" {
				attrs, _ := mark["attrs"].(map[string]any)
				linkURL, _ = attrs["href"].(string)
			}
			// code, strong, em: strip wrapper, keep content (handled by just writing text).
		}
		if linkURL != "" {
			b.WriteString(text)
			b.WriteString(" (")
			b.WriteString(linkURL)
			b.WriteString(")")
		} else {
			b.WriteString(text)
		}

	case "mention":
		attrs, _ := node["attrs"].(map[string]any)
		id, _ := attrs["id"].(string)
		displayText, _ := attrs["text"].(string)
		b.WriteString("@")
		b.WriteString(displayText)
		*mentions = append(*mentions, Mention{Source: "jira", ExternalID: id, DisplayName: displayText})

	case "inlineCard", "mediaInline":
		attrs, _ := node["attrs"].(map[string]any)
		url, _ := attrs["url"].(string)
		b.WriteString(url)

	case "blockCard", "embedCard":
		attrs, _ := node["attrs"].(map[string]any)
		url, _ := attrs["url"].(string)
		b.WriteString(url)
		b.WriteString("\n\n")

	case "paragraph":
		walkChildren(node, b, mentions)
		b.WriteString("\n\n")

	case "heading":
		walkChildren(node, b, mentions)
		b.WriteString("\n\n")

	case "bulletList":
		walkListItems(node, b, mentions, false)

	case "orderedList":
		walkListItems(node, b, mentions, true)

	case "listItem":
		walkChildren(node, b, mentions)

	case "codeBlock":
		b.WriteString("```")
		walkChildren(node, b, mentions)
		b.WriteString("```\n\n")

	default:
		// doc, blockquote, table, etc. — just recurse into children.
		walkChildren(node, b, mentions)
	}
}

func walkChildren(node map[string]any, b *strings.Builder, mentions *[]Mention) {
	content, _ := node["content"].([]any)
	for _, child := range content {
		childMap, ok := child.(map[string]any)
		if !ok {
			continue
		}
		walkADF(childMap, b, mentions)
	}
}

func walkListItems(node map[string]any, b *strings.Builder, mentions *[]Mention, ordered bool) {
	content, _ := node["content"].([]any)
	for i, child := range content {
		childMap, ok := child.(map[string]any)
		if !ok {
			continue
		}
		if ordered {
			b.WriteString(strings.Repeat("", 0)) // noop, prefix below
			var item strings.Builder
			walkADF(childMap, &item, mentions)
			itemText := strings.TrimRight(item.String(), "\n ")
			b.WriteString(strings.Repeat("", 0))
			b.WriteString(itoa(i+1))
			b.WriteString(". ")
			b.WriteString(itemText)
			b.WriteString("\n")
		} else {
			var item strings.Builder
			walkADF(childMap, &item, mentions)
			itemText := strings.TrimRight(item.String(), "\n ")
			b.WriteString("- ")
			b.WriteString(itemText)
			b.WriteString("\n")
		}
	}
}

func itoa(i int) string {
	// Simple int-to-string without importing strconv — just use fmt via sprintf.
	// Actually strconv is stdlib, but let's keep it simple with a tiny loop.
	if i == 0 {
		return "0"
	}
	digits := []byte{}
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}
