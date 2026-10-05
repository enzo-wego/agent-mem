package config

import "testing"

// processing_paused has to survive four hops: DB string -> Config, Config ->
// snapshot (what the dispatchers actually read), Config -> RuntimeSettings (what
// gets persisted), and the dashboard's JSON PUT -> Config. A typo in any one of
// them yields a switch that looks wired but silently never pauses — or worse,
// never unpauses.
func TestProcessingPausedRoundTrip(t *testing.T) {
	c := &Config{}

	// DB load: string "true" -> bool, and reaches the snapshot dispatchers read.
	c.ApplyDBSettings(map[string]string{"processing_paused": "true"})
	if !c.ProcessingPaused {
		t.Fatal("ApplyDBSettings did not set ProcessingPaused")
	}
	if !c.Snapshot().ProcessingPaused {
		t.Error("Snapshot dropped ProcessingPaused — dispatchers would never pause")
	}

	// Persisted form must round-trip back through ApplyDBSettings.
	if got := c.RuntimeSettings()["processing_paused"]; got != "true" {
		t.Errorf("RuntimeSettings[processing_paused] = %q, want \"true\"", got)
	}

	// Dashboard PUT sends a JSON bool, not a string.
	c.Update(map[string]any{"processing_paused": false})
	if c.ProcessingPaused {
		t.Error("Update(false) did not unpause — the switch would be one-way")
	}
	if c.Snapshot().ProcessingPaused {
		t.Error("Snapshot still paused after Update(false)")
	}

	// Anything other than "true" is not paused: a missing or malformed row must
	// fail safe toward processing, never toward a silent indefinite pause.
	for _, v := range []string{"false", "", "yes", "1"} {
		c2 := &Config{}
		c2.ApplyDBSettings(map[string]string{"processing_paused": v})
		if c2.ProcessingPaused {
			t.Errorf("ApplyDBSettings(%q) paused; want running", v)
		}
	}
}

func TestSlackUserCredentialsRoundTrip(t *testing.T) {
	c := &Config{}
	want := map[string]string{
		"slack_user_token":  "xoxc-FAKE-LEAK-CANARY",
		"slack_user_cookie": "xoxd-FAKE-LEAK-CANARY",
	}
	if c.Update(map[string]any{"slack_user_token": want["slack_user_token"], "slack_user_cookie": want["slack_user_cookie"]}) {
		t.Fatal("Slack credentials must not rebuild LLM clients")
	}
	for key, value := range want {
		if got := c.RuntimeSettings()[key]; got != value {
			t.Fatalf("%s was not saved", key)
		}
	}
	c.Update(map[string]any{"slack_user_token": "", "slack_user_cookie": "   "})
	c.Update(map[string]any{"log_level": "info"})
	if snap := c.Snapshot(); snap.SlackUserToken != want["slack_user_token"] || snap.SlackUserCookie != want["slack_user_cookie"] {
		t.Fatal("Snapshot dropped Slack credentials")
	}
	for key, value := range want {
		if got := c.RuntimeSettings()[key]; got != value {
			t.Fatalf("blank or omitted update erased %s", key)
		}
	}
	reloaded := &Config{}
	reloaded.ApplyDBSettings(c.RuntimeSettings())
	for key, value := range want {
		if got := reloaded.RuntimeSettings()[key]; got != value {
			t.Fatalf("%s did not survive reload", key)
		}
	}
	// Clear wins even if the same request also supplies a replacement.
	c.Update(map[string]any{
		"slack_user_cookie": "replacement",
		"clear_settings":    []any{"slack_user_cookie"},
	})
	persisted := c.RuntimeSettings()
	if value, ok := persisted["slack_user_cookie"]; !ok || value != "" {
		t.Fatal("clear must persist an explicit empty cookie")
	}
	if persisted["slack_user_token"] != want["slack_user_token"] {
		t.Fatal("cookie clear changed token")
	}
	reloaded.ApplyDBSettings(persisted)
	if reloaded.RuntimeSettings()["slack_user_cookie"] != "" {
		t.Fatal("empty persisted cookie did not overwrite old cookie")
	}
	c.Update(map[string]any{"llm_gateway_api_key": "gateway-test"})
	c.Update(map[string]any{"clear_settings": []any{"slack_user_token", "llm_gateway_api_key"}})
	if c.RuntimeSettings()["slack_user_token"] != "" {
		t.Fatal("explicit token clear failed")
	}
	if c.RuntimeSettings()["llm_gateway_api_key"] != "gateway-test" {
		t.Fatal("clear whitelist allowed unrelated gateway key")
	}
	reloaded.ApplyDBSettings(c.RuntimeSettings())
	if reloaded.Snapshot().SlackUserToken != "" || reloaded.Snapshot().SlackUserCookie != "" {
		t.Fatal("cleared credentials returned after reload")
	}
}
