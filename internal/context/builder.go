package context

import (
	"context"

	"github.com/agent-mem/agent-mem/internal/config"
	"github.com/agent-mem/agent-mem/internal/database"
)

// Builder constructs context markdown from past observations and summaries.
type Builder struct {
	db  *database.DB
	cfg *config.Config
}

// NewBuilder creates a new context builder.
func NewBuilder(db *database.DB, cfg *config.Config) *Builder {
	return &Builder{db: db, cfg: cfg}
}

// BuildContext queries recent observations and summaries for the project
// and renders them as markdown for injection into Claude's session.
// Returns empty string if no data exists.
func (b *Builder) BuildContext(ctx context.Context, project string) (string, error) {
	observations, err := b.db.GetRecentObservations(ctx, project, b.cfg.ContextObservations)
	if err != nil {
		return "", err
	}

	summaries, err := b.db.GetRecentSummaries(ctx, project, b.cfg.ContextSessionCount)
	if err != nil {
		return "", err
	}

	if len(observations) == 0 && len(summaries) == 0 {
		return "", nil
	}

	return render(observations, summaries, b.cfg.ContextFullCount), nil
}
