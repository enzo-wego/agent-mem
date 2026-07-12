package sync

import (
	"context"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/database"
)

// cleanDerivedTestRows deletes only the rows created by these tests.
func cleanDerivedTestRows(t *testing.T, db *database.DB) {
	t.Helper()
	ctx := context.Background()
	db.Pool.Exec(ctx, `DELETE FROM graph.thread_summaries WHERE channel_id = 'C_TEST_DRV'`)
	db.Pool.Exec(ctx, `DELETE FROM graph.slack_users WHERE slack_user_id = 'U_TEST_DRV'`)
	db.Pool.Exec(ctx, `DELETE FROM graph.slack_channels WHERE slack_channel_id = 'C_TEST_DRV'`)
}

func TestDerivedSync_ThreadSummaryUpsert(t *testing.T) {
	pool := openTestPool(t)
	db := database.NewDB(pool)
	cleanDerivedTestRows(t, db)
	t.Cleanup(func() { cleanDerivedTestRows(t, db) })

	ctx := context.Background()
	t1 := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Millisecond)
	t2 := t1.Add(time.Minute)

	// Import v1.
	v1 := &database.SyncableThreadSummary{
		ChannelID: "C_TEST_DRV", ThreadTS: "1234.5678",
		Signature: "sig1", Summary: "first version", UpdatedAt: t1,
	}
	if err := db.UpsertThreadSummaryFromSync(ctx, v1); err != nil {
		t.Fatalf("upsert v1: %v", err)
	}

	// Newer version wins.
	v2 := &database.SyncableThreadSummary{
		ChannelID: "C_TEST_DRV", ThreadTS: "1234.5678",
		Signature: "sig2", Summary: "second version", UpdatedAt: t2,
	}
	if err := db.UpsertThreadSummaryFromSync(ctx, v2); err != nil {
		t.Fatalf("upsert v2: %v", err)
	}

	// Replaying the older version must NOT downgrade the row.
	if err := db.UpsertThreadSummaryFromSync(ctx, v1); err != nil {
		t.Fatalf("replay v1: %v", err)
	}

	var summary string
	if err := pool.QueryRow(ctx, `
		SELECT summary FROM graph.thread_summaries
		WHERE channel_id = 'C_TEST_DRV' AND thread_ts = '1234.5678'`).Scan(&summary); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if summary != "second version" {
		t.Fatalf("expected newest version to win, got %q", summary)
	}

	// Since-query returns the row for cursors before t2, not after.
	rows, err := db.GetThreadSummariesSince(ctx, t1.Add(-time.Second), 100)
	if err != nil {
		t.Fatalf("GetThreadSummariesSince: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.ChannelID == "C_TEST_DRV" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected test row in since-query results")
	}
}

func TestDerivedSync_SlackUserUpsert(t *testing.T) {
	pool := openTestPool(t)
	db := database.NewDB(pool)
	cleanDerivedTestRows(t, db)
	t.Cleanup(func() { cleanDerivedTestRows(t, db) })

	ctx := context.Background()
	t1 := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)

	u := &database.SyncableSlackUser{
		SlackUserID: "U_TEST_DRV", DisplayName: "enzo", IsBot: false,
		RefreshedAt: t1, MachineID: "cloud-test",
	}
	if err := db.UpsertSlackUserFromSync(ctx, u); err != nil {
		t.Fatalf("upsert user: %v", err)
	}

	// Fresher rename propagates.
	u2 := *u
	u2.DisplayName = "enzo-renamed"
	u2.RefreshedAt = t1.Add(30 * time.Second)
	if err := db.UpsertSlackUserFromSync(ctx, &u2); err != nil {
		t.Fatalf("upsert rename: %v", err)
	}

	var name string
	if err := pool.QueryRow(ctx, `
		SELECT display_name FROM graph.slack_users WHERE slack_user_id = 'U_TEST_DRV'`).Scan(&name); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if name != "enzo-renamed" {
		t.Fatalf("expected rename to win, got %q", name)
	}

	users, err := db.GetSlackUsersSince(ctx, t1.Add(-time.Second), 100)
	if err != nil {
		t.Fatalf("GetSlackUsersSince: %v", err)
	}
	if len(users) == 0 {
		t.Fatal("expected at least the test user in since-query")
	}

	ch := &database.SyncableSlackChannel{
		SlackChannelID: "C_TEST_DRV", Name: "test-chan",
		RefreshedAt: t1, MachineID: "cloud-test",
	}
	if err := db.UpsertSlackChannelFromSync(ctx, ch); err != nil {
		t.Fatalf("upsert channel: %v", err)
	}
	chans, err := db.GetSlackChannelsSince(ctx, t1.Add(-time.Second), 100)
	if err != nil {
		t.Fatalf("GetSlackChannelsSince: %v", err)
	}
	if len(chans) == 0 {
		t.Fatal("expected at least the test channel in since-query")
	}
}
