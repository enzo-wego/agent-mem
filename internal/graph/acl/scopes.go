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
// NOTE: there is no separate Slack path. graph.slack_groups stores Slack
// *usergroups* (@team-x handles), not channel membership, so it cannot answer
// "is this asker in channel C?"; the previous Slack query joined slack_groups to
// itself and effectively granted every slack:* scope to anyone in any usergroup
// — a cross-channel leak. Until a channel-membership source exists (a
// conversations.members refresh job writing slack:CHANNEL rows into
// member_scopes — see issue agent-mem-7h1), Slack ACL fails closed: a real asker
// gets no slack:* scope and so sees only public/unscoped Slack content. The
// trusted unfiltered view (eeid 0) is unaffected.
func (b *Builder) build(ctx context.Context, eeid int) ([]string, error) {
	rows, err := b.db.Query(ctx, `
SELECT DISTINCT scope FROM graph.member_scopes WHERE eeid = $1
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
