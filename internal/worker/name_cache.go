package worker

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// dbNameCache resolves Slack ids to display names from graph.slack_users and
// graph.slack_groups so the normalizer can turn <@U…>/<!subteam^S…> into names.
// Implements normalizer.Cache. Results (hits and misses) are memoized in-process
// to avoid a DB round-trip per mention during a backfill.
type dbNameCache struct {
	db   *pgxpool.Pool
	mu   sync.RWMutex
	memo map[string]string // "source|id" -> name; "" means looked-up-and-missing
}

func newDBNameCache(db *pgxpool.Pool) *dbNameCache {
	return &dbNameCache{db: db, memo: map[string]string{}}
}

func (c *dbNameCache) DisplayName(ctx context.Context, source, externalID string) (string, bool) {
	if externalID == "" {
		return "", false
	}
	key := source + "|" + externalID
	c.mu.RLock()
	v, ok := c.memo[key]
	c.mu.RUnlock()
	if ok {
		return v, v != ""
	}

	var name string
	switch source {
	case "slack":
		_ = c.db.QueryRow(ctx, `SELECT display_name FROM graph.slack_users WHERE slack_user_id=$1`, externalID).Scan(&name)
	case "slack_group":
		_ = c.db.QueryRow(ctx, `SELECT COALESCE(NULLIF(name,''), handle) FROM graph.slack_groups WHERE id=$1`, externalID).Scan(&name)
	}

	c.mu.Lock()
	c.memo[key] = name
	c.mu.Unlock()
	return name, name != ""
}
