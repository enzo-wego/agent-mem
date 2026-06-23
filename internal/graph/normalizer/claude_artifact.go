package normalizer

import "context"

// ClaudeArtifactNormalizer converts a shared Claude artifact (self-contained
// HTML) to plain text via the shared tag-strip path.
type ClaudeArtifactNormalizer struct{}

// NewClaudeArtifactNormalizer returns a ClaudeArtifactNormalizer.
func NewClaudeArtifactNormalizer() *ClaudeArtifactNormalizer { return &ClaudeArtifactNormalizer{} }

func (n *ClaudeArtifactNormalizer) Source() string { return "claude_artifact" }

func (n *ClaudeArtifactNormalizer) Normalize(_ context.Context, raw []byte, _ map[string]any) (Result, error) {
	if len(raw) == 0 {
		return Result{}, nil
	}
	return Result{Text: confluenceFallback(raw)}, nil
}
