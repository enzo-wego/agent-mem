// Package handlers implements the six graph-worker job handlers.
// Each handler is wired to a specific job type and registered via RegisterAll.
package handlers

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/extractor"
	"github.com/agent-mem/agent-mem/internal/graph/fetchers"
	"github.com/agent-mem/agent-mem/internal/graph/identity"
	"github.com/agent-mem/agent-mem/internal/graph/jobs"
	"github.com/agent-mem/agent-mem/internal/graph/normalizer"
)

// GeminiClient is the minimal interface handlers need. *gemini.Client satisfies
// it directly — Embed is already defined there. Describe is added via a thin
// adapter (see gemini_adapter.go) because the real client exposes Generate, not
// Describe.
type GeminiClient interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	Describe(ctx context.Context, mime string, data []byte, prompt string) (description string, ocr string, entities []string, err error)
}

// Deps groups the shared dependencies needed by all handlers. Wired once at
// worker boot.
type Deps struct {
	DB          *pgxpool.Pool
	Logger      zerolog.Logger
	MachineID   string
	Fetchers    *fetchers.Registry
	Normalizers *normalizer.Registry
	Extractor   *extractor.Extractor
	Identity    *identity.Service
	Gemini      GeminiClient
}

// RegisterAll registers all six handlers with the given Dispatcher.
func RegisterAll(d *jobs.Dispatcher, deps Deps) {
	d.Register("fetch_body", NewFetchBodyHandler(deps))
	d.Register("describe_attachment", NewDescribeAttachmentHandler(deps))
	d.Register("resolve_identity", NewResolveIdentityHandler(deps))
	d.Register("index_artifact", NewIndexArtifactHandler(deps))
	d.Register("refresh_slack_groups", NewRefreshSlackGroupsHandler(deps))
	d.Register("import_bamboohr", NewImportBambooHRHandler(deps))
}
