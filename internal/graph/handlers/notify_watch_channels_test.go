package handlers

import (
	"encoding/json"
	"testing"
)

// TestContinentOf verifies the Go classifier matches the dashboard's continentOf:
// override wins, else first continent whose match is "*" or a name prefix. Uses
// the live partner config (ext-wego-, wego-tap -> partners).
func TestContinentOf(t *testing.T) {
	var cfg continentsConfig
	if err := json.Unmarshal([]byte(defaultContinents), &cfg); err != nil {
		t.Fatalf("parse defaultContinents: %v", err)
	}
	// defaultContinents only has ext-wego- for partners; add wego-tap to mirror prod.
	for i := range cfg.Continents {
		if cfg.Continents[i].ID == "partners" {
			cfg.Continents[i].Match = []string{"ext-wego-", "wego-tap"}
		}
	}
	cases := []struct {
		id, name, want string
	}{
		{"C1", "ext-wego-juspay", "partners"},
		{"C2", "wego-tap", "partners"},
		{"C3", "payments-alerts", "core"},
		{"C4", "random-channel", "other"}, // "*" catch-all
		{"C5", "", "other"},               // unknown name -> id "C5" -> catch-all
	}
	for _, c := range cases {
		if got := continentOf(c.id, c.name, cfg); got != c.want {
			t.Errorf("continentOf(%q,%q) = %q, want %q", c.id, c.name, got, c.want)
		}
	}

	// Override wins over name match.
	cfg.Overrides = map[string]string{"C9": "partners"}
	if got := continentOf("C9", "totally-unrelated", cfg); got != "partners" {
		t.Errorf("override should win, got %q", got)
	}
	// Config name beats the Slack fallback name.
	cfg.Names = map[string]string{"C0736FUE03W": "ext-wego-juspay"}
	if got := continentOf("C0736FUE03W", "some-other-name", cfg); got != "partners" {
		t.Errorf("config name should classify to partners, got %q", got)
	}
}

// TestSplitSlackRoot verifies thread root ids parse into (channel, ts) and that
// non-thread / malformed ids report ok=false so no topic lookup is attempted.
func TestSplitSlackRoot(t *testing.T) {
	cases := []struct {
		in             string
		wantCh, wantTS string
		wantOK         bool
	}{
		{"slack:C0736FUE03W:1782376386.293389", "C0736FUE03W", "1782376386.293389", true},
		{"slack:C123:", "", "", false},   // empty ts
		{"slack:C123", "", "", false},    // only 2 parts
		{"jira:PAY-2177", "", "", false}, // not slack
		{"", "", "", false},              // empty
	}
	for _, c := range cases {
		ch, ts, ok := splitSlackRoot(c.in)
		if ch != c.wantCh || ts != c.wantTS || ok != c.wantOK {
			t.Errorf("splitSlackRoot(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.in, ch, ts, ok, c.wantCh, c.wantTS, c.wantOK)
		}
	}
}
