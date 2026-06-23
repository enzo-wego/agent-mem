package normalizer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// PagerDutyNormalizer converts a PagerDuty incident JSON to plain text.
type PagerDutyNormalizer struct{}

// NewPagerDutyNormalizer returns a PagerDutyNormalizer.
func NewPagerDutyNormalizer() *PagerDutyNormalizer { return &PagerDutyNormalizer{} }

func (n *PagerDutyNormalizer) Source() string { return "pagerduty" }

// Normalize renders a PagerDuty GET /incidents/:id JSON response as structured
// plain text. Falls back to raw text on malformed JSON.
func (n *PagerDutyNormalizer) Normalize(_ context.Context, raw []byte, _ map[string]any) (Result, error) {
	if len(raw) == 0 {
		return Result{}, nil
	}

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Result{Text: string(raw)}, nil
	}

	// PagerDuty wraps the object under "incident" key, or it may be the
	// object directly.
	incident, ok := doc["incident"].(map[string]any)
	if !ok {
		incident = doc
	}

	number := jsonInt(incident, "incident_number")
	title := jsonStr(incident, "title")
	description := jsonStr(incident, "description")
	status := jsonStr(incident, "status")
	urgency := jsonStr(incident, "urgency")
	createdAt := jsonStr(incident, "created_at")
	resolvedAt := jsonStr(incident, "resolved_at")
	htmlURL := jsonStr(incident, "html_url")

	service := ""
	if svc, ok := incident["service"].(map[string]any); ok {
		service = jsonStr(svc, "summary")
	}

	var assignees []string
	if assignments, ok := incident["assignments"].([]any); ok {
		for _, a := range assignments {
			aMap, ok := a.(map[string]any)
			if !ok {
				continue
			}
			if assignee, ok := aMap["assignee"].(map[string]any); ok {
				if summary := jsonStr(assignee, "summary"); summary != "" {
					assignees = append(assignees, summary)
				}
			}
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "PagerDuty Incident #%d: %s\n", number, title)
	fmt.Fprintf(&b, "Service: %s   Urgency: %s   Status: %s\n", service, urgency, status)
	if len(assignees) > 0 {
		fmt.Fprintf(&b, "Assignees: %s\n", strings.Join(assignees, ", "))
	}
	fmt.Fprintf(&b, "Created: %s", createdAt)
	if resolvedAt != "" {
		fmt.Fprintf(&b, "   Resolved: %s", resolvedAt)
	}
	b.WriteByte('\n')
	if htmlURL != "" {
		fmt.Fprintf(&b, "URL: %s\n", htmlURL)
	}
	if description != "" {
		b.WriteByte('\n')
		b.WriteString(description)
	}

	return Result{Text: strings.TrimSpace(b.String())}, nil
}

// jsonStr extracts a string field from a map[string]any.
func jsonStr(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

// jsonInt extracts an int/float64 field and returns it as int.
func jsonInt(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}
