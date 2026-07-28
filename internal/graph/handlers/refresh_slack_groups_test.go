package handlers

import (
	"context"
	"os"
	"testing"

	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/database"
)

func TestRefreshSlackGroupsHandler_MissingToken(t *testing.T) {
	t.Setenv("SLACK_BOT_TOKEN", "")
	deps := Deps{Logger: zerolog.Nop(), MachineID: "test"}
	h := NewRefreshSlackGroupsHandler(deps)

	err := h.Handler(context.Background(), []byte("{}"))
	if err == nil {
		t.Fatal("expected error when SLACK_BOT_TOKEN is not set")
	}
}

func TestRefreshSlackGroupsHandler_WithDB(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
	// Integration test placeholder.
}

func TestIsInterestGroup_Patterns(t *testing.T) {
	if !isInterestGroup("payments-geeks") {
		t.Error("expected payments-geeks to match -geeks")
	}
	if !isInterestGroup("platform-ops") {
		t.Error("expected platform-ops to match -ops")
	}
	if isInterestGroup("general") {
		t.Error("expected general NOT to match")
	}
	if isInterestGroup("geeks-club") {
		t.Error("expected geeks-club NOT to match (prefix, not suffix)")
	}
}

// TestUpsertSlackGroup_MembersEncodeAsArray pins the contract that made this job a silent
// no-op from its very first run: member_user_ids is text[], so members must reach pgx as a
// []string. Marshalling to JSON first made Postgres reject every row ("malformed array
// literal") while the job still reported done, because the loop only warns and continues.
//
// Point AGENT_MEM_TEST_DATABASE_URL at a scratch database, never the dev one.
func TestUpsertSlackGroup_MembersEncodeAsArray(t *testing.T) {
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

	deps := Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "test-groups"}
	const id = "STESTGRP1"
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM graph.slack_groups WHERE id = $1`, id)
	})

	g := slackUserGroup{
		ID: id, Handle: "payments-geeks", Name: "Payments Geeks",
		UserCount: 3, Users: []string{"UUK3WPNNQ", "U07UAC0J7T3", "U050BBA607M"},
	}
	if err := upsertSlackGroup(ctx, deps, g, g.Users); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	var members []string
	var handle string
	if err := pool.QueryRow(ctx,
		`SELECT handle, member_user_ids FROM graph.slack_groups WHERE id = $1`, id).
		Scan(&handle, &members); err != nil {
		t.Fatalf("read: %v", err)
	}
	if handle != "payments-geeks" || len(members) != 3 || members[0] != "UUK3WPNNQ" {
		t.Fatalf("stored row wrong: handle=%q members=%v", handle, members)
	}

	// Re-upsert with a shrunk roster: the conflict path must replace the array.
	g.Users, g.UserCount = []string{"UUK3WPNNQ"}, 1
	if err := upsertSlackGroup(ctx, deps, g, g.Users); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT member_user_ids FROM graph.slack_groups WHERE id = $1`, id).Scan(&members); err != nil {
		t.Fatalf("read 2: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("roster not replaced on conflict: %v", members)
	}

	// An empty group must still store, as an empty array rather than NULL.
	if err := upsertSlackGroup(ctx, deps, slackUserGroup{ID: id, Handle: "empty-geeks"}, []string{}); err != nil {
		t.Fatalf("empty upsert: %v", err)
	}
}
