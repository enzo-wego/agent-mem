// Package handlers implements the six graph-worker job handlers.
// Each handler is wired to a specific job type and registered via RegisterAll.
package handlers

import (
	"context"
	"time"

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
	Generate(ctx context.Context, systemPrompt, userMessage string) (string, error)
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
	LiteParse   LiteParseConfig
}

// RegisterAll registers all handlers with the given Registry.
func RegisterAll(reg *jobs.Registry, deps Deps) {
	reg.Register("fetch_body", NewFetchBodyHandler(deps))
	reg.Register("describe_attachment", NewDescribeAttachmentHandler(deps))
	reg.Register("resolve_identity", NewResolveIdentityHandler(deps))
	reg.Register("index_artifact", NewIndexArtifactHandler(deps))
	reg.Register("refresh_slack_groups", NewRefreshSlackGroupsHandler(deps))
	reg.Register("refresh_slack_users", NewRefreshSlackUsersHandler(deps))
	reg.Register("import_bamboohr", NewImportBambooHRHandler(deps))
	reg.Register("recompute_person_distance", jobs.Entry{
		Handler:   RecomputePersonDistance(deps.DB, deps.Logger),
		Systems:   []string{},
		PoolSize:  1,
		Lease:     600 * time.Second,
		Heartbeat: true,
	})
	reg.Register("backfill_slack_channel", NewBackfillSlackChannelHandler(deps))
	reg.Register("summarize_thread", NewSummarizeThreadHandler(deps))
	reg.Register("backfill_slack_thread", NewBackfillSlackThreadHandler(deps))
}
