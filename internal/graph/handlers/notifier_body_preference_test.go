package handlers

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

func TestBuildAlert_PrefersBody(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	for _, tbl := range []string{"graph.thread_summaries", "graph.nodes", "graph.people"} {
		if _, err := pool.Exec(ctx, "DELETE FROM "+tbl); err != nil {
			t.Fatalf("clean %s: %v", tbl, err)
		}
	}

	const body = "Merchant of Record support is required"
	author := insPerson(t, pool, "Ross", 0)
	messageTS := ts(time.Now(), 0)
	insSlack(t, pool, "CBUILD", messageTS, "", author, body)
	if _, err := pool.Exec(ctx, `UPDATE graph.nodes SET title='Short generated label' WHERE id=$1`, "slack:CBUILD:"+messageTS); err != nil {
		t.Fatalf("set title: %v", err)
	}

	got := buildAlert(ctx, Deps{DB: pool, Logger: zerolog.Nop()}, subscription{Topic: "payments"}, hotThread{
		RootNodeID: "slack:CBUILD:" + messageTS,
		Channel:    "CBUILD",
	})
	if !strings.Contains(got, body) {
		t.Errorf("alert transcript missing body: %q", got)
	}
	if strings.Contains(got, "Short generated label") {
		t.Errorf("alert transcript used generated title instead of body: %q", got)
	}
}

func TestNewWatchedMessages_PrefersBodyWithTitleFallback(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	for _, tbl := range []string{"graph.channel_notifications", "graph.nodes", "graph.people"} {
		if _, err := pool.Exec(ctx, "DELETE FROM "+tbl); err != nil {
			t.Fatalf("clean %s: %v", tbl, err)
		}
	}

	author := insPerson(t, pool, "Reporter", 5)
	bodyTS := ts(time.Now(), 0)
	insSlack(t, pool, "CWATCH", bodyTS, "", author, "full watched-channel message")
	if _, err := pool.Exec(ctx, `UPDATE graph.nodes SET title='Short generated label' WHERE id=$1`, "slack:CWATCH:"+bodyTS); err != nil {
		t.Fatalf("set body-message title: %v", err)
	}
	fallbackTS := ts(time.Now(), 1)
	insSlack(t, pool, "CWATCH", fallbackTS, "", author, "")
	if _, err := pool.Exec(ctx, `UPDATE graph.nodes SET title='Attachment-only title' WHERE id=$1`, "slack:CWATCH:"+fallbackTS); err != nil {
		t.Fatalf("set fallback title: %v", err)
	}

	msgs, err := newWatchedMessages(ctx, pool, []string{"CWATCH"}, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatalf("newWatchedMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	if msgs[0].text != "full watched-channel message" {
		t.Errorf("body message text = %q, want body", msgs[0].text)
	}
	if msgs[1].text != "Attachment-only title" {
		t.Errorf("empty-body message text = %q, want title fallback", msgs[1].text)
	}
}
