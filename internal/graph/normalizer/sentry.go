package normalizer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// SentryNormalizer converts a Sentry issue JSON to plain text.
type SentryNormalizer struct{}

// NewSentryNormalizer returns a SentryNormalizer.
func NewSentryNormalizer() *SentryNormalizer { return &SentryNormalizer{} }

func (n *SentryNormalizer) Source() string { return "sentry" }

// Normalize renders a Sentry GET /api/0/issues/:id/ JSON as structured plain text.
func (n *SentryNormalizer) Normalize(_ context.Context, raw []byte, _ map[string]any) (Result, error) {
	if len(raw) == 0 {
		return Result{}, nil
	}

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Result{Text: string(raw)}, nil
	}

	shortID := jsonStr(doc, "shortId")
	title := jsonStr(doc, "title")
	status := jsonStr(doc, "status")
	level := jsonStr(doc, "level")
	firstSeen := jsonStr(doc, "firstSeen")
	lastSeen := jsonStr(doc, "lastSeen")
	count := jsonStr(doc, "count")
	userCount := jsonInt(doc, "userCount")
	permalink := jsonStr(doc, "permalink")

	projectSlug := ""
	if proj, ok := doc["project"].(map[string]any); ok {
		projectSlug = jsonStr(proj, "slug")
	}

	metadataValue := ""
	if meta, ok := doc["metadata"].(map[string]any); ok {
		metadataValue = jsonStr(meta, "value")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Sentry Issue %s: %s\n", shortID, title)
	fmt.Fprintf(&b, "Project: %s   Status: %s   Level: %s\n", projectSlug, status, level)
	fmt.Fprintf(&b, "First seen: %s   Last seen: %s   Count: %s   Users: %d\n", firstSeen, lastSeen, count, userCount)
	if permalink != "" {
		fmt.Fprintf(&b, "URL: %s\n", permalink)
	}
	if metadataValue != "" {
		b.WriteByte('\n')
		b.WriteString(metadataValue)
		b.WriteByte('\n')
	}

	// Latest stack frame.
	if entries, ok := doc["entries"].([]any); ok {
		for _, entry := range entries {
			entryMap, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			if jsonStr(entryMap, "type") != "exception" {
				continue
			}
			data, ok := entryMap["data"].(map[string]any)
			if !ok {
				continue
			}
			values, ok := data["values"].([]any)
			if !ok || len(values) == 0 {
				continue
			}
			exc, ok := values[0].(map[string]any)
			if !ok {
				continue
			}
			st, ok := exc["stacktrace"].(map[string]any)
			if !ok {
				continue
			}
			frames, ok := st["frames"].([]any)
			if !ok || len(frames) == 0 {
				continue
			}
			// Last frame = most recent.
			frame, ok := frames[len(frames)-1].(map[string]any)
			if !ok {
				continue
			}
			filename := jsonStr(frame, "filename")
			fn := jsonStr(frame, "function")
			// Sentry uses "lineNo" in some SDK versions and "lineno" in others.
			lineno := jsonInt(frame, "lineNo")
			if lineno == 0 {
				lineno = jsonInt(frame, "lineno")
			}
			if filename != "" {
				b.WriteString("\nLatest stack frame:\n")
				fmt.Fprintf(&b, "%s:%d in %s\n", filename, lineno, fn)
			}
			break
		}
	}

	return Result{Text: strings.TrimSpace(b.String())}, nil
}
