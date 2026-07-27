package config

import "testing"

// Key pools are kept as labelled lists in practice (~/gkey.txt is "-- name" then
// the key). A label parsed as a key would be blocked on first use, so comments
// and blank lines must never reach the pool.
func TestSplitKeysDropsCommentsAndLabels(t *testing.T) {
	in := `-- Source key
AIzaSy-one

-- n8n cleen
AIzaSy-two
# hashed comment
AIzaSy-three  # personal
// slashed comment
AIzaSy-four, AIzaSy-five
`
	want := []string{"AIzaSy-one", "AIzaSy-two", "AIzaSy-three", "AIzaSy-four", "AIzaSy-five"}
	got := SplitKeys(in)
	if len(got) != len(want) {
		t.Fatalf("SplitKeys = %v (%d keys), want %v (%d)", got, len(got), want, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("key %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSplitKeysEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\n", "-- only a comment"} {
		if got := SplitKeys(in); len(got) != 0 {
			t.Errorf("SplitKeys(%q) = %v, want no keys", in, got)
		}
	}
}

// A hyphen inside a key must not be read as a comment marker.
func TestSplitKeysKeepsHyphensInsideKeys(t *testing.T) {
	got := SplitKeys("AIzaSy--weird-key")
	if len(got) != 1 || got[0] != "AIzaSy--weird-key" {
		t.Errorf("SplitKeys = %v, want the whole key intact", got)
	}
}

func TestActiveLLMKeysPrefersPoolOnGoogle(t *testing.T) {
	snap := ConfigSnapshot{
		LLMProvider:   "google",
		GoogleAPIKey:  "single",
		GoogleAPIKeys: "-- a\npool-one\npool-two",
		GeminiAPIKey:  "sk-or-openrouter",
	}
	if got := snap.ActiveLLMKeys(); len(got) != 2 || got[0] != "pool-one" {
		t.Errorf("google pool = %v, want [pool-one pool-two]", got)
	}

	// Empty pool falls back to the single google key.
	snap.GoogleAPIKeys = ""
	if got := snap.ActiveLLMKeys(); len(got) != 1 || got[0] != "single" {
		t.Errorf("google fallback = %v, want [single]", got)
	}

	// The pool is google-only; openrouter keeps its one key.
	snap.LLMProvider = "openrouter"
	snap.GoogleAPIKeys = "pool-one\npool-two"
	if got := snap.ActiveLLMKeys(); len(got) != 1 || got[0] != "sk-or-openrouter" {
		t.Errorf("openrouter keys = %v, want [sk-or-openrouter]", got)
	}
}
