package handlers

import "testing"

// TestLoadImportanceConfig verifies the embedded importance.json parses and carries
// the owner anchor + at least one override (so a typo in the JSON fails the build's
// test run, not the live notifier).
func TestLoadImportanceConfig(t *testing.T) {
	c := loadImportanceConfig()
	if c.OwnerEEID == 0 {
		t.Fatal("owner_eeid not parsed from embedded importance.json")
	}
	if c.ManagerEEID == 0 {
		t.Error("manager_eeid not parsed")
	}
	if len(c.Overrides) == 0 {
		t.Error("expected at least one override person")
	}
	for _, o := range c.Overrides {
		if o.Name == "" || o.Score <= 0 {
			t.Errorf("override has empty name or non-positive score: %+v", o)
		}
	}
}

// TestWithDept checks the department label formatting.
func TestWithDept(t *testing.T) {
	if got := withDept("Lei Zheng", "Engineering", "Staff Software Engineer", "payments", "engineering lead"); got != "Lei Zheng (payments · engineering lead)" {
		t.Errorf("derived role: got %q", got)
	}
	if got := withDept("Lei Zheng", "Engineering", "Staff Software Engineer", "", ""); got != "Lei Zheng (Engineering · Staff Software Engineer)" {
		t.Errorf("dept+title: got %q", got)
	}
	// The department is dropped when the title already names it, so the label doesn't
	// read "Payments · Director, Payments, Risk & Fintech".
	if got := withDept("Alexandre Morin", "Payments", "Director, Payments, Risk & Fintech", "", ""); got != "Alexandre Morin (Director, Payments, Risk & Fintech)" {
		t.Errorf("redundant dept: got %q", got)
	}
	// Slack-only accounts and bots have no BambooHR title.
	if got := withDept("Sentry", "", "", "", ""); got != "Sentry" {
		t.Errorf("no dept/title: got %q", got)
	}
	if got := withDept("Hazwan", "Flights", "", "", ""); got != "Hazwan (Flights)" {
		t.Errorf("got %q, want %q", got, "Hazwan (Flights)")
	}
	if got := withDept("Enzo", "", "", "", ""); got != "Enzo" {
		t.Errorf("blank dept should give bare name, got %q", got)
	}
	if got := withDept("Enzo", "   ", "  ", " ", " "); got != "Enzo" {
		t.Errorf("whitespace dept should give bare name, got %q", got)
	}
}
