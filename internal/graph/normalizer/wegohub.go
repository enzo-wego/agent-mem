package normalizer

import "context"

// WegoHubNormalizer converts a published Wego Hub file (HTML) to plain text.
// Wego Hub artifacts are self-contained HTML reports/dashboards, so the
// tag-strip + entity-unescape path (shared with Confluence) is sufficient.
type WegoHubNormalizer struct{}

// NewWegoHubNormalizer returns a WegoHubNormalizer.
func NewWegoHubNormalizer() *WegoHubNormalizer { return &WegoHubNormalizer{} }

func (n *WegoHubNormalizer) Source() string { return "wegohub" }

// Normalize strips HTML to plain text. Reuses confluenceFallback (same package).
func (n *WegoHubNormalizer) Normalize(_ context.Context, raw []byte, _ map[string]any) (Result, error) {
	if len(raw) == 0 {
		return Result{}, nil
	}
	return Result{Text: confluenceFallback(raw)}, nil
}
