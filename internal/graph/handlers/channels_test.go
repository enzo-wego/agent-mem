package handlers

import (
	"encoding/json"
	"testing"
)

// The shipped default must parse and mute #payments-staging so a fresh install
// doesn't DM every staging deploy-bot notification.
func TestDefaultContinentsIgnore(t *testing.T) {
	var cfg continentsConfig
	if err := json.Unmarshal([]byte(defaultContinents), &cfg); err != nil {
		t.Fatalf("defaultContinents is not valid JSON: %v", err)
	}
	ignored := map[string]bool{}
	for _, id := range cfg.Ignore {
		ignored[id] = true
	}
	if !ignored["C0B1BR522F5"] {
		t.Errorf("payments-staging (C0B1BR522F5) not in ignore list: %v", cfg.Ignore)
	}
}

func TestLooksLikeSlackID(t *testing.T) {
	// Raw Slack ids that leak into author chips when unresolved — must be hidden.
	rawIDs := []string{"B0AEXGRC10C", "B08MZ7M36N9", "B500YRZN1", "BGADP3STV", "U01TMG8Q65R", "W012ABC3DEF"}
	for _, s := range rawIDs {
		if !looksLikeSlackID(s) {
			t.Errorf("looksLikeSlackID(%q) = false, want true", s)
		}
	}
	// Real display names must never be mistaken for ids.
	names := []string{"Enzo", "Surbhi Babbar", "yanyi", "mike.hoang", "GitHub", "PagerDuty", "Claude [debugging]", "B01"}
	for _, s := range names {
		if looksLikeSlackID(s) {
			t.Errorf("looksLikeSlackID(%q) = true, want false", s)
		}
	}
}
