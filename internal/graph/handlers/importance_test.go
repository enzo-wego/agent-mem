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
	if got := withDept("Hazwan", "Flights"); got != "Hazwan (Flights)" {
		t.Errorf("got %q, want %q", got, "Hazwan (Flights)")
	}
	if got := withDept("Enzo", ""); got != "Enzo" {
		t.Errorf("blank dept should give bare name, got %q", got)
	}
	if got := withDept("Enzo", "   "); got != "Enzo" {
		t.Errorf("whitespace dept should give bare name, got %q", got)
	}
}
