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
