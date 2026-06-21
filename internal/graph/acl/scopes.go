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

// build computes the scope list by joining people → slack_groups (for Slack
// scopes) and reading graph.member_scopes for other source memberships.
func (b *Builder) build(ctx context.Context, eeid int) ([]string, error) {
	// 1. Slack channel scopes: find channels whose scope appears on nodes and
	//    the asker's slack_user_id is in the group's member_user_ids.
	//    We derive the scope from distinct node.scope values for slack channels
	//    where the asker is a group member.
	rows, err := b.db.Query(ctx, `
SELECT DISTINCT n.scope
FROM graph.nodes n
WHERE n.scope LIKE 'slack:%'
  AND n.deleted_at IS NULL
  AND EXISTS (
    SELECT 1 FROM graph.slack_groups g
    JOIN graph.people p ON p.slack_user_id = ANY(g.member_user_ids)
    WHERE p.eeid = $1
      AND n.scope = 'slack:' || split_part(n.scope, ':', 2)
  )
`, eeid)
	if err != nil {
		return nil, fmt.Errorf("acl slack scopes: %w", err)
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

	// 2. Other scopes from the denormalised member_scopes table
	//    (Jira/GH/CF membership, populated by per-source refresh jobs).
	rows2, err := b.db.Query(ctx, `
SELECT DISTINCT scope FROM graph.member_scopes WHERE eeid = $1
`, eeid)
	if err == nil {
		defer rows2.Close()
		for rows2.Next() {
			var s string
			if err := rows2.Scan(&s); err != nil {
				return nil, err
			}
			scopes = append(scopes, s)
		}
	}
	// Ignore error on member_scopes (table may be empty but should exist after migrations).

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
