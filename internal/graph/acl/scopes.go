// Package acl computes per-asker accessible_scopes snapshots.
package acl

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Builder produces and caches per-asker scope lists.
type Builder struct {
	db    *pgxpool.Pool
	ttl   time.Duration
	mu    sync.Mutex
	cache map[int]*entry
}

type entry struct {
	scopes []string
	at     time.Time
}

// NewBuilder returns a fresh builder. TTL controls how long a snapshot
// is reused.
func NewBuilder(db *pgxpool.Pool, ttl time.Duration) *Builder {
	return &Builder{
		db:    db,
		ttl:   ttl,
		cache: make(map[int]*entry),
	}
}

// For returns the accessible_scopes for the given asker eeid.
func (b *Builder) For(ctx context.Context, askerEEID int) ([]string, error) {
	b.mu.Lock()
	if e, ok := b.cache[askerEEID]; ok && time.Since(e.at) < b.ttl {
		out := append([]string(nil), e.scopes...)
		b.mu.Unlock()
		return out, nil
	}
	b.mu.Unlock()

	scopes, err := b.build(ctx, askerEEID)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.cache[askerEEID] = &entry{scopes: scopes, at: time.Now()}
	b.mu.Unlock()
	return scopes, nil
}

// build computes the scope list from graph.member_scopes — the single,
// per-source membership table populated by the refresh jobs (one row per
// (eeid, scope), e.g. 'slack:C123', 'jira:PROJ', 'github:org/repo').
//
// Slack channel scopes (C/G) require membership evidence refreshed within
// 24 hours, independently of whether the refresh job's housekeeping runs.
// DM and non-Slack scopes are not age-limited. Each reader caches this snapshot
// for its TTL (five minutes in production), so grants and revocations can take
// that long to become visible, including expiry of an aged channel grant.
// graph.slack_groups stores usergroups, not channel membership, and is never
// used to grant channel access.
func (b *Builder) build(ctx context.Context, eeid int) ([]string, error) {
	rows, err := b.db.Query(ctx, `
SELECT DISTINCT scope FROM graph.member_scopes
WHERE eeid = $1
  AND NOT ((scope LIKE 'slack:C%' OR scope LIKE 'slack:G%')
           AND refreshed_at < now() - interval '24 hours')
`, eeid)
	if err != nil {
		return nil, fmt.Errorf("acl member_scopes: %w", err)
	}
	defer rows.Close()
	var scopes []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		scopes = append(scopes, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Returns the asker's real memberships only. Visibility of internal-public
	// ("public") content is handled at the read endpoints (search/resolve), not
	// here, so it applies uniformly even to an asker with zero memberships.
	return scopes, nil
}

// Invalidate drops the cache for the given asker (used by refresh jobs).
func (b *Builder) Invalidate(eeid int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.cache, eeid)
}
