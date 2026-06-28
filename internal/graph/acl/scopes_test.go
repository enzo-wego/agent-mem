package acl_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/acl"
)

func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("DB ping: %v", err)
	}
	t.Cleanup(func() {
		cleanupACLTables(t, pool)
		pool.Close()
	})
	cleanupACLTables(t, pool)
	return pool
}

func cleanupACLTables(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, tbl := range []string{
		"graph.member_scopes",
		"graph.nodes",
		"graph.slack_groups",
		"graph.people",
	} {
		if _, err := pool.Exec(ctx, "DELETE FROM "+tbl); err != nil {
			t.Logf("cleanup %s: %v", tbl, err)
		}
	}
}

func seedAsker(t *testing.T, pool *pgxpool.Pool, eeid int, slackUID string, _ []string) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
INSERT INTO graph.people (eeid, display_name, slack_user_id, machine_id)
VALUES ($1, 'Test User', $2, 'test')
ON CONFLICT (eeid) DO NOTHING`, eeid, slackUID)
	if err != nil {
		t.Fatalf("seedAsker: %v", err)
	}
}

func seedSlackGroupMembers(t *testing.T, pool *pgxpool.Pool, groupID string, memberUIDs []string) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
INSERT INTO graph.slack_groups (id, handle, name, member_user_ids, machine_id)
VALUES ($1, $1, $1, $2, 'test')
ON CONFLICT (id) DO UPDATE SET member_user_ids = EXCLUDED.member_user_ids`,
		groupID, memberUIDs)
	if err != nil {
		t.Fatalf("seedSlackGroupMembers: %v", err)
	}
}

func seedSlackChannelNode(t *testing.T, pool *pgxpool.Pool, channelScopes []string) {
	t.Helper()
	ctx := context.Background()
	for _, scope := range channelScopes {
		_, err := pool.Exec(ctx, `
INSERT INTO graph.nodes (id, type, natural_key, scope, machine_id)
VALUES ($1, 'slack_thread', $1, $2, 'test')
ON CONFLICT (id) DO NOTHING`, scope+":1", scope)
		if err != nil {
			t.Fatalf("seedSlackChannelNode: %v", err)
		}
	}
}

func seedMemberScope(t *testing.T, pool *pgxpool.Pool, eeid int, scopes ...string) {
	t.Helper()
	ctx := context.Background()
	for _, s := range scopes {
		if _, err := pool.Exec(ctx, `
INSERT INTO graph.member_scopes (eeid, scope) VALUES ($1, $2)
ON CONFLICT (eeid, scope) DO NOTHING`, eeid, s); err != nil {
			t.Fatalf("seedMemberScope: %v", err)
		}
	}
}

func TestBuilder_ReturnsAccessibleScopes(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	seedAsker(t, pool, 982, "U07UAC0J7T3", nil)
	// Memberships come from member_scopes (populated by the per-source refresh
	// jobs), regardless of source.
	seedMemberScope(t, pool, 982, "slack:C05RNSE8TBR", "jira:PROJ")

	b := acl.NewBuilder(pool, 5*time.Minute)
	scopes, err := b.For(ctx, 982)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"slack:C05RNSE8TBR": true,
		"jira:PROJ":         true,
	}
	for w := range want {
		found := false
		for _, s := range scopes {
			if s == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing scope %q in %v", w, scopes)
		}
	}
}

// Regression for the cross-channel leak (issue agent-mem-7h1): being in a Slack
// *usergroup* must NOT grant any slack:CHANNEL scope. Channel access is only
// conferred by an explicit member_scopes row.
func TestBuilder_SlackUsergroupDoesNotGrantChannelScope(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	seedAsker(t, pool, 555, "U_INGROUP", nil)
	seedSlackGroupMembers(t, pool, "S01TMG8Q65R", []string{"U_INGROUP"})
	seedSlackChannelNode(t, pool, []string{"slack:C_PRIVATE"})
	// Note: no member_scopes row for eeid 555.

	b := acl.NewBuilder(pool, 5*time.Minute)
	scopes, err := b.For(ctx, 555)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range scopes {
		if s == "slack:C_PRIVATE" {
			t.Fatalf("usergroup membership leaked channel scope %q (scopes=%v)", s, scopes)
		}
	}
}

// An asker with no derivable scopes must return an EMPTY set, not ["public"].
// An empty set means "no filter" (admin/anonymous sees everything); adding
// "public" there would wrongly narrow visibility to public-only.
func TestBuilder_NoScopesStaysEmpty(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	seedAsker(t, pool, 777, "U_NOGROUPS", nil)

	b := acl.NewBuilder(pool, 5*time.Minute)
	scopes, err := b.For(ctx, 777)
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 0 {
		t.Errorf("expected empty scopes for asker with no memberships, got %v", scopes)
	}
}

func TestBuilder_CachesSnapshot(t *testing.T) {
	ctx := context.Background()
	pool := testDB(t)
	seedAsker(t, pool, 982, "U07UAC0J7T3", nil)

	b := acl.NewBuilder(pool, 1*time.Minute)
	s1, err := b.For(ctx, 982)
	if err != nil {
		t.Fatal(err)
	}
	// Mutate underlying memberships after the first call; the cached snapshot
	// (1-minute TTL) must not pick this up until it expires or is invalidated.
	seedMemberScope(t, pool, 982, "slack:C_NEW")

	s2, _ := b.For(ctx, 982)
	if len(s1) != len(s2) {
		t.Errorf("cache invalidated prematurely: %d vs %d", len(s1), len(s2))
	}
}
