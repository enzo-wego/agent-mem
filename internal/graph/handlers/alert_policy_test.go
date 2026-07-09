package handlers

import "testing"

func TestIsAlertChannelName(t *testing.T) {
	for _, name := range []string{"payments-alerts", "payment-alert", "alerts-payments"} {
		if !isAlertChannelName(name) {
			t.Fatalf("%q should be classified as an alert channel", name)
		}
	}
	for _, name := range []string{"payments-dev", "alerting-design", "sales"} {
		if isAlertChannelName(name) {
			t.Fatalf("%q should not be classified as an alert channel", name)
		}
	}
}

func TestAlertFingerprintStripsVolatileValues(t *testing.T) {
	a := alertFingerprint("PaymentFailed order 123456 amount 50.00 at 2026-07-09T10:11:12Z")
	b := alertFingerprint("PaymentFailed order 999999 amount 70.00 at 2026-07-09T10:12:13Z")
	if a == "" {
		t.Fatal("fingerprint is empty")
	}
	if a != b {
		t.Fatalf("fingerprints differ for same template:\n%s\n%s", a, b)
	}
}

func TestShouldSkipSlackMessageForAlertPolicy(t *testing.T) {
	botRoot := slackMessage{BotID: "B1", Subtype: "bot_message", Text: "PaymentFailed 123", ReplyCount: 1}
	if !shouldSkipSlackMessageForAlertPolicy(botRoot, alertBotDecision{Skip: true}, false) {
		t.Fatal("normal alert backfill should skip known bot fingerprints")
	}
	if shouldSkipSlackMessageForAlertPolicy(botRoot, alertBotDecision{Skip: true}, true) {
		t.Fatal("forced alert-thread backfill should ingest the bot root")
	}

	join := slackMessage{Subtype: "channel_join"}
	if !shouldSkipSlackMessageForAlertPolicy(join, alertBotDecision{}, true) {
		t.Fatal("forced alert-thread backfill should still skip non-message subtypes")
	}
}

func TestAlertThreadBackfillEscalatesSkippedBotWithReplies(t *testing.T) {
	msg := slackMessage{Ts: "100.000001", ThreadTs: "100.000001", BotID: "B1", Subtype: "bot_message", ReplyCount: 2}
	if !forceAlertThreadBackfill(msg, alertBotDecision{Skip: true}) {
		t.Fatal("skipped bot alert with replies should trigger forced thread backfill")
	}
}
