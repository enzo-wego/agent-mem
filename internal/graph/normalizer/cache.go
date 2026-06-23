package normalizer

import "context"

// Cache resolves source-native IDs to human-readable display names. Used by
// Slack to rewrite <@U…> into @Name. Other normalizers may use it for
// mentions in bodies (e.g. Jira @mentions).
type Cache interface {
	DisplayName(ctx context.Context, source, externalID string) (name string, ok bool)
}

// MemoryCache is a tiny in-memory implementation used in unit tests.
// The seed map key format is "<source>/<externalID>".
type MemoryCache struct {
	data map[string]string
}

// NewMemoryCache creates a MemoryCache pre-populated with seed. Seed keys
// must be in the form "source/externalID", e.g. "slack/U02FKR154T1".
func NewMemoryCache(seed map[string]string) *MemoryCache {
	d := make(map[string]string, len(seed))
	for k, v := range seed {
		d[k] = v
	}
	return &MemoryCache{data: d}
}

// DisplayName looks up source+externalID in the in-memory map.
func (c *MemoryCache) DisplayName(_ context.Context, source, externalID string) (string, bool) {
	v, ok := c.data[source+"/"+externalID]
	return v, ok
}

// noopCache always returns ("", false).
type noopCache struct{}

func (noopCache) DisplayName(_ context.Context, _, _ string) (string, bool) { return "", false }
