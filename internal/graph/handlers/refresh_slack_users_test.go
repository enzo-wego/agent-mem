package handlers

import (
	"context"
	"os"
	"testing"

	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/database"
)

// TestLinkSlackPersonByEmail pins the exact-email identity reconciliation used by
// refresh_slack_users. graph.people.email is UNIQUE, so a Slack-side row cannot first be
// given the same email as its BambooHR row and then passed to MergeByEmail: the UPDATE is
// rejected before the merge can run. The link must instead use the Slack profile email as
// evidence while keeping the email on the canonical EEID row.
//
// Point AGENT_MEM_TEST_DATABASE_URL at a scratch database, never the dev one.
func TestLinkSlackPersonByEmail(t *testing.T) {
	dsn := os.Getenv("AGENT_MEM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AGENT_MEM_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := database.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	deps := Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "test-slack-email-link"}
	const (
		mergedEEID      = -202607281
		mergedEmail     = "task-eh9-merge@wego.test"
		mergedSlackID   = "UTASKEH9MERGE"
		attachedEEID    = -202607282
		attachedEmail   = "task-eh9-attach@wego.test"
		attachedSlackID = "UTASKEH9ATTACH"
	)
	cleanup := func() {
		_, _ = pool.Exec(context.Background(), `
			DELETE FROM graph.identity_map
			WHERE source = 'slack' AND external_id = ANY($1)`,
			[]string{mergedSlackID, attachedSlackID})
		_, _ = pool.Exec(context.Background(), `
			DELETE FROM graph.user_affinity_config WHERE eeid = ANY($1)`,
			[]int{mergedEEID, attachedEEID})
		_, _ = pool.Exec(context.Background(), `
			UPDATE graph.people SET merged_into = NULL WHERE machine_id = $1`,
			deps.MachineID)
		_, _ = pool.Exec(context.Background(), `
			DELETE FROM graph.people WHERE machine_id = $1`,
			deps.MachineID)
	}
	cleanup()
	t.Cleanup(cleanup)

	var canonicalID, duplicateID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO graph.people (eeid, email, display_name, machine_id)
		VALUES ($1, $2, 'Canonical Employee', $3)
		RETURNING id`,
		mergedEEID, mergedEmail, deps.MachineID).Scan(&canonicalID); err != nil {
		t.Fatalf("seed canonical employee: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO graph.people (slack_user_id, display_name, machine_id)
		VALUES ($1, 'slack-handle', $2)
		RETURNING id`,
		mergedSlackID, deps.MachineID).Scan(&duplicateID); err != nil {
		t.Fatalf("seed Slack duplicate: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO graph.identity_map (source, external_id, person_id)
		VALUES ('slack', $1, $2)`,
		mergedSlackID, duplicateID); err != nil {
		t.Fatalf("seed identity map: %v", err)
	}

	linked, err := linkSlackPersonByEmail(ctx, deps, mergedSlackID, mergedEmail)
	if err != nil {
		t.Fatalf("link split identity: %v", err)
	}
	if !linked {
		t.Fatal("split identity was not linked")
	}

	var gotEEID int
	var gotSlackID, gotEmail string
	if err := pool.QueryRow(ctx, `
		SELECT eeid, slack_user_id, email::text
		FROM graph.people WHERE id = $1`,
		canonicalID).Scan(&gotEEID, &gotSlackID, &gotEmail); err != nil {
		t.Fatalf("read canonical employee: %v", err)
	}
	if gotEEID != mergedEEID || gotSlackID != mergedSlackID || gotEmail != mergedEmail {
		t.Fatalf("canonical identity wrong: eeid=%d slack=%q email=%q", gotEEID, gotSlackID, gotEmail)
	}

	var mergedInto *int64
	var duplicateSlackID *string
	if err := pool.QueryRow(ctx, `
		SELECT merged_into, slack_user_id
		FROM graph.people WHERE id = $1`,
		duplicateID).Scan(&mergedInto, &duplicateSlackID); err != nil {
		t.Fatalf("read duplicate employee: %v", err)
	}
	if mergedInto == nil || *mergedInto != canonicalID || duplicateSlackID != nil {
		t.Fatalf("duplicate not folded into canonical: merged_into=%v slack_user_id=%v", mergedInto, duplicateSlackID)
	}

	var mappedPersonID int64
	if err := pool.QueryRow(ctx, `
		SELECT person_id FROM graph.identity_map
		WHERE source = 'slack' AND external_id = $1`,
		mergedSlackID).Scan(&mappedPersonID); err != nil {
		t.Fatalf("read identity map: %v", err)
	}
	if mappedPersonID != canonicalID {
		t.Fatalf("identity map points to %d, want %d", mappedPersonID, canonicalID)
	}
	linked, err = linkSlackPersonByEmail(ctx, deps, mergedSlackID, mergedEmail)
	if err != nil {
		t.Fatalf("relink canonical identity: %v", err)
	}
	if linked {
		t.Fatal("already canonical identity reported a new link")
	}

	var attachCanonicalID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO graph.people (eeid, email, display_name, machine_id)
		VALUES ($1, $2, 'Employee Without Slack Row', $3)
		RETURNING id`,
		attachedEEID, attachedEmail, deps.MachineID).Scan(&attachCanonicalID); err != nil {
		t.Fatalf("seed attach-only employee: %v", err)
	}
	linked, err = linkSlackPersonByEmail(ctx, deps, attachedSlackID, attachedEmail)
	if err != nil {
		t.Fatalf("attach Slack identity: %v", err)
	}
	if !linked {
		t.Fatal("Slack identity without a person row was not attached")
	}
	if err := pool.QueryRow(ctx, `
		SELECT slack_user_id FROM graph.people WHERE id = $1`,
		attachCanonicalID).Scan(&gotSlackID); err != nil {
		t.Fatalf("read attached employee: %v", err)
	}
	if gotSlackID != attachedSlackID {
		t.Fatalf("attached slack_user_id = %q, want %q", gotSlackID, attachedSlackID)
	}
}
