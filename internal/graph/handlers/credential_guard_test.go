package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// These tests pin the agent-mem-egsf contract: a handler that needs a
// credential must FAIL (jobs.ErrFatal, terminal — IsRetryable treats it as
// such) when the credential is missing, while an empty recipient stays a
// legitimate data-dependent no-op returning nil. Before the fix the first two
// handlers returned nil in both cases, so ~914k "done" rows hid the 10-day
// post-migration credential outage.

// missingTokenErrFatal runs h with no Slack token but a valid recipient and
// requires a terminal ErrFatal error. It fails on nil or non-ErrFatal errors.
func missingTokenErrFatal(t *testing.T, name string, h func(ctx context.Context) error) {
	t.Helper()
	err := h(context.Background())
	if err == nil {
		t.Fatalf("%s: nil error with empty SlackBotToken; want jobs.ErrFatal", name)
	}
	if !errors.Is(err, jobs.ErrFatal) {
		t.Fatalf("%s: err = %v; want errors.Is(err, jobs.ErrFatal)", name, err)
	}
	if !strings.Contains(err.Error(), "SLACK_BOT_TOKEN not set") {
		t.Fatalf("%s: err %q should mention SLACK_BOT_TOKEN not set", name, err)
	}
}

func testDeps(pool *pgxpool.Pool) Deps {
	return Deps{
		DB:            pool,
		Logger:        zerolog.Nop(),
		MachineID:     "test",
		SlackDMUserID: "USUBSCRIBER",
	}
}

// missingCredsErrFatal runs h with the given credentials absent and requires a
// terminal ErrFatal error whose message contains wantSubstr.
func missingCredsErrFatal(t *testing.T, name, wantSubstr string, h func(ctx context.Context) error) {
	t.Helper()
	err := h(context.Background())
	if err == nil {
		t.Fatalf("%s: nil error with missing credentials; want jobs.ErrFatal", name)
	}
	if !errors.Is(err, jobs.ErrFatal) {
		t.Fatalf("%s: err = %v; want errors.Is(err, jobs.ErrFatal)", name, err)
	}
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Fatalf("%s: err %q should mention %q", name, err, wantSubstr)
	}
}

// TestNotifyWatchChannels_MissingTokenFails pins Fix B for the watch-channel
// DM loop: empty deps.SlackBotToken + valid recipient (SlackDMUserID set) is a
// misconfiguration and must return ErrFatal, not nil. Needs a DB because the
// reschedule defer enqueues the next tick.
func TestNotifyWatchChannels_MissingTokenFails(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)

	h := NewNotifyWatchChannels(testDeps(pool))
	missingTokenErrFatal(t, "notify_watch_channels", func(ctx context.Context) error {
		return h(ctx, nil)
	})
}

// TestMonitorHourlyReport_MissingTokenFails pins Fix B for the hourly monitor:
// same contract as notify_watch_channels.
func TestMonitorHourlyReport_MissingTokenFails(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)

	h := NewMonitorHourlyReport(testDeps(pool))
	missingTokenErrFatal(t, "monitor_hourly_report", func(ctx context.Context) error {
		return h(ctx, nil)
	})
}

// TestRefreshJiraBoard_MissingCredsFails pins Fix B for refresh_jira_board:
// any of BASE_URL/EMAIL/TOKEN unset must fail terminally instead of logging
// "skipping" and marking the job done with a stale epic map.
func TestRefreshJiraBoard_MissingCredsFails(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	deps := testDeps(pool)
	h := refreshJiraBoardHandler(deps)

	cases := []struct{ name string }{
		{"all unset"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, k := range []string{"AGENT_MEM_JIRA_BASE_URL", "AGENT_MEM_JIRA_EMAIL", "AGENT_MEM_JIRA_TOKEN"} {
				t.Setenv(k, "")
			}
			missingCredsErrFatal(t, "refresh_jira_board/"+c.name, "AGENT_MEM_JIRA_BASE_URL/EMAIL/TOKEN not set", func(ctx context.Context) error {
				return h(ctx, nil)
			})
		})
	}

	// Each single missing piece fails too: partial config is still misconfig.
	full := map[string]string{
		"AGENT_MEM_JIRA_BASE_URL": "https://example.atlassian.net",
		"AGENT_MEM_JIRA_EMAIL":    "you@example.com",
		"AGENT_MEM_JIRA_TOKEN":    "placeholder",
	}
	for omit := range full {
		t.Run("omit "+omit, func(t *testing.T) {
			for k, v := range full {
				if k == omit {
					t.Setenv(k, "")
				} else {
					t.Setenv(k, v)
				}
			}
			if err := h(context.Background(), nil); !errors.Is(err, jobs.ErrFatal) {
				t.Fatalf("refresh_jira_board without %s: err = %v; want ErrFatal", omit, err)
			}
		})
	}
}

// TestDetectHotTopics_MissingTokenFails drives detect_hot_topics end to end
// against a real schema and a stubbed Slack API: one active subscription, one
// hot on-topic thread that passes the topic gate and gets its dedup claim —
// then delivery hits the missing token. The run must fail with ErrFatal so the
// queue shows the outage instead of a healthy-looking done row.
func TestDetectHotTopics_MissingTokenFails(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	ctx := context.Background()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"DTEST"}}`))
	}))
	defer srv.Close()
	oldBase, oldHTTP := slackAPIBaseURL, slackHTTP
	slackAPIBaseURL, slackHTTP = srv.URL, srv.Client()
	t.Cleanup(func() { slackAPIBaseURL, slackHTTP = oldBase, oldHTTP })

	// 4 distinct participants so the volume trigger fires (min_participants=2).
	// Fixed base: a relative-to-now window boundary must not flip the fixture.
	now := fixedBase()
	root := ts(now, 10)
	for i := range 4 {
		p := insPerson(t, pool, fmt.Sprintf("Participant %d", i), 5)
		insSlack(t, pool, "C1", ts(now, 11+i), root, p, "discussing payments incident")
	}
	subID := insSubscription(t, pool, "", "payments") // "" → falls back to deps.SlackDMUserID

	deps := testDeps(pool)
	h := NewDetectHotTopics(deps)
	missingTokenErrFatal(t, "detect_hot_topics", func(c context.Context) error {
		return h(c, nil)
	})

	// The dedup claim was rolled back, so the next successful run can send it.
	var claims int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM graph.topic_notifications WHERE subscription_id=$1`, subID).Scan(&claims); err != nil {
		t.Fatalf("count claims: %v", err)
	}
	if claims != 0 {
		t.Fatalf("topic_notifications claims = %d after failed delivery; want 0 (claim rolled back)", claims)
	}
}

// insSubscription inserts one active topic subscription and returns its id.
// subscriber may be "" (the handler then falls back to deps.SlackDMUserID).
func insSubscription(t *testing.T, pool *pgxpool.Pool, subscriber, topic string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO graph.topic_subscriptions
		   (subscriber_slack_id, topic, channel_filter, min_participants, max_author_depth, sources)
		 VALUES ($1,$2,'{}'::text[],$3,$4,'[]'::jsonb) RETURNING id`,
		subscriber, topic, 2, 99).Scan(&id)
	if err != nil {
		t.Fatalf("insSubscription: %v", err)
	}
	return id
}

// TestNotifyWatchChannels_EmptyRecipientNoOp preserves the OTHER half of the
// guard: token present, recipient resolved to "" → legitimate data-dependent
// no-op, must return nil (NOT an error), even with watched channels present.
func TestNotifyWatchChannels_EmptyRecipientNoOp(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)

	t.Setenv("SLACK_BOT_TOKEN", "xoxb-placeholder-not-real")
	deps := Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "test", SlackBotToken: "xoxb-placeholder-not-real"}
	h := NewNotifyWatchChannels(deps)
	if err := h(context.Background(), nil); err != nil {
		t.Fatalf("notify_watch_channels with token but no recipient: err = %v; want nil no-op", err)
	}
}

// TestMonitorHourlyReport_EmptyRecipientNoOp: same no-op contract for the
// hourly monitor. The reschedule defer fires first (window not expired); the
// recipient check must then return nil without touching Slack.
func TestMonitorHourlyReport_EmptyRecipientNoOp(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)

	token := "xoxb-placeholder-not-real"
	t.Setenv("SLACK_BOT_TOKEN", token)
	deps := Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "test", SlackBotToken: token}
	h := NewMonitorHourlyReport(deps)
	if err := h(context.Background(), nil); err != nil {
		t.Fatalf("monitor_hourly_report with token but no recipient: err = %v; want nil no-op", err)
	}
}

// TestDetectHotTopics_EmptyRecipientSkipsDelivery proves the data-dependent
// no-op survived the fix: token present, sub has no subscriber AND no default,
// thread otherwise deliverable → handler returns nil and leaves NO dedup claim
// behind, so configuring a recipient later lets the alert fire.
func TestDetectHotTopics_EmptyRecipientSkipsDelivery(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)

	// Fixed base so a detection-window boundary cannot flip this fixture.
	now := fixedBase()
	root := ts(now, 20)
	for i := range 4 {
		p := insPerson(t, pool, fmt.Sprintf("Participant %d", i), 5)
		insSlack(t, pool, "C1", ts(now, 21+i), root, p, "discussing payments incident")
	}
	subID := insSubscription(t, pool, "", "payments")

	token := "xoxb-placeholder-not-real"
	deps := Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "test", SlackBotToken: token} // no recipient anywhere
	h := NewDetectHotTopics(deps)
	if err := h(context.Background(), nil); err != nil {
		t.Fatalf("detect_hot_topics with token but no recipient anywhere: err = %v; want nil", err)
	}
	var claims int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM graph.topic_notifications WHERE subscription_id=$1`, subID).Scan(&claims); err != nil {
		t.Fatalf("count claims: %v", err)
	}
	if claims != 0 {
		t.Fatalf("claims = %d; want 0 — the claim must be released so a later run with a recipient can deliver", claims)
	}
}

// TestDetectHotTopics_TokenPresentDelivers closes the loop on the happy path:
// with token + recipient resolved from the default, the stubbed Slack receives
// chat.postMessage and the claim sticks. Guards the fix against over-failing.
func TestDetectHotTopics_TokenPresentDelivers(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)

	var posted int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "chat.postMessage") {
			posted++
		}
		_, _ = w.Write([]byte(`{"ok":true,"channel":{"id":"DTEST"},"ts":"1700000000.000001"}`))
	}))
	defer srv.Close()
	oldBase, oldHTTP := slackAPIBaseURL, slackHTTP
	slackAPIBaseURL, slackHTTP = srv.URL, srv.Client()
	t.Cleanup(func() { slackAPIBaseURL, slackHTTP = oldBase, oldHTTP })

	token := "xoxb-placeholder-not-real"
	// Fixed base so a detection-window boundary cannot flip this fixture.
	now := fixedBase()
	root := ts(now, 30)
	for i := range 4 {
		p := insPerson(t, pool, fmt.Sprintf("Participant %d", i), 5)
		insSlack(t, pool, "C1", ts(now, 31+i), root, p, "discussing payments incident")
	}
	insSubscription(t, pool, "", "payments")

	deps := Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "test", SlackBotToken: token, SlackDMUserID: "USUBSCRIBER"}
	h := NewDetectHotTopics(deps)
	if err := h(context.Background(), nil); err != nil {
		t.Fatalf("detect_hot_topics happy path: err = %v; want nil", err)
	}
	if posted == 0 {
		t.Fatal("expected at least one chat.postMessage to the stubbed Slack API")
	}
	var claims int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM graph.topic_notifications`).Scan(&claims); err != nil {
		t.Fatalf("count claims: %v", err)
	}
	if claims == 0 {
		t.Fatal("expected the dedup claim to stick after successful delivery")
	}
}

// fixedBase returns a deterministic fixture timestamp. Fixtures used
// time.Now(), which flaked when a run straddled a detection-window boundary;
// the absolute value is irrelevant to these tests, only relative offsets are.
func fixedBase() time.Time {
	return time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)
}
