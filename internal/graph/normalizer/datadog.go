package normalizer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// DatadogNormalizer converts a Datadog monitor JSON to plain text.
type DatadogNormalizer struct{}

// NewDatadogNormalizer returns a DatadogNormalizer.
func NewDatadogNormalizer() *DatadogNormalizer { return &DatadogNormalizer{} }

func (n *DatadogNormalizer) Source() string { return "datadog" }

// Normalize renders a Datadog GET /api/v1/monitor/:id JSON as structured plain text.
func (n *DatadogNormalizer) Normalize(_ context.Context, raw []byte, _ map[string]any) (Result, error) {
	if len(raw) == 0 {
		return Result{}, nil
	}

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Result{Text: string(raw)}, nil
	}

	id := jsonInt(doc, "id")
	name := jsonStr(doc, "name")
	monType := jsonStr(doc, "type")
	overallState := jsonStr(doc, "overall_state")
	query := jsonStr(doc, "query")
	message := jsonStr(doc, "message")

	creatorName := ""
	if creator, ok := doc["creator"].(map[string]any); ok {
		creatorName = jsonStr(creator, "name")
	}

	var tags []string
	if tagList, ok := doc["tags"].([]any); ok {
		for _, t := range tagList {
			if s, ok := t.(string); ok {
				tags = append(tags, s)
			}
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Datadog Monitor #%d: %s\n", id, name)
	fmt.Fprintf(&b, "Type: %s   Status: %s   Creator: %s\n", monType, overallState, creatorName)
	if query != "" {
		fmt.Fprintf(&b, "Query: %s\n", query)
	}
	if message != "" {
		fmt.Fprintf(&b, "Message:\n%s\n", message)
	}
	if len(tags) > 0 {
		fmt.Fprintf(&b, "\nTags: %s", strings.Join(tags, ", "))
	}

	return Result{Text: strings.TrimSpace(b.String())}, nil
}
