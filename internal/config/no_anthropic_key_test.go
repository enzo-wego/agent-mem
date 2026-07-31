package config

import (
	"os"
	"strings"
	"testing"
)

// TestNoAnthropicKeyIsEverRead is a guard, not a behavior test. Routing graph
// summaries through a metered Anthropic API key cost ~$11/hour during a
// summarize_thread amplification bug, with no spend ceiling to stop it. The key
// was removed on 2026-08-01 and Claude is reached only via llm-gateway, which
// authenticates with a subscription seat and rate-limits instead of billing.
//
// The load path is the thing worth pinning: a stray ANTHROPIC_API_KEY in the
// worker's environment used to be picked up silently, which is how metered
// billing started without anyone choosing it.
func TestNoAnthropicKeyIsEverRead(t *testing.T) {
	// A key in the environment must have no effect whatsoever.
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-must-be-ignored")
	t.Setenv("AGENT_MEM_ANTHROPIC_API_KEY", "sk-ant-must-also-be-ignored")
	t.Setenv("AGENT_MEM_ANTHROPIC_MODEL", "claude-opus-4-8")

	c := defaults()
	ApplyEnv(c)

	// A key arriving from the settings table must be dropped just as hard: the
	// row is deleted by migration 20260801000001, but an older replica could
	// still hand it over on startup.
	c.ApplyDBSettings(map[string]string{
		"anthropic_api_key": "sk-ant-from-db",
		"anthropic_model":   "claude-opus-4-8",
	})
	// Same for a live dashboard PUT.
	c.Update(map[string]any{
		"anthropic_api_key": "sk-ant-from-api",
		"anthropic_model":   "claude-opus-4-8",
	})

	// Nothing may reach the snapshot handlers read, the settings persisted back
	// to the DB, or any other config field.
	for k, v := range c.RuntimeSettings() {
		if strings.Contains(strings.ToLower(k), "anthropic") {
			t.Errorf("RuntimeSettings exposes %q — the key would be persisted again", k)
		}
		if strings.Contains(v, "sk-ant-") {
			t.Errorf("setting %q leaked an Anthropic key into persisted settings: %q", k, v)
		}
	}
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Fatal("test bug: the env var should still be set; this test proves it is IGNORED, not unset")
	}
}
