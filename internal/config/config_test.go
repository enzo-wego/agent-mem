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

// A pasted pool (or the legacy AGENT_MEM_GOOGLE_API_KEY joining it) can repeat a
// key; a duplicate would just get double the traffic.
func TestSplitKeysDedupes(t *testing.T) {
	got := SplitKeys("AIza-one\nAIza-two\nAIza-one")
	if len(got) != 2 || got[0] != "AIza-one" || got[1] != "AIza-two" {
		t.Errorf("SplitKeys = %v, want [AIza-one AIza-two]", got)
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
		GoogleAPIKeys: "-- a\npool-one\npool-two",
		GeminiAPIKey:  "sk-or-openrouter",
	}
	if got := snap.ActiveLLMKeys(); len(got) != 2 || got[0] != "pool-one" {
		t.Errorf("google pool = %v, want [pool-one pool-two]", got)
	}

	// A single google key is just a pool of one.
	snap.GoogleAPIKeys = "solo"
	if got := snap.ActiveLLMKeys(); len(got) != 1 || got[0] != "solo" {
		t.Errorf("one-key pool = %v, want [solo]", got)
	}

	// The pool is google-only; openrouter keeps its one key.
	snap.LLMProvider = "openrouter"
	snap.GoogleAPIKeys = "pool-one\npool-two"
	if got := snap.ActiveLLMKeys(); len(got) != 1 || got[0] != "sk-or-openrouter" {
		t.Errorf("openrouter keys = %v, want [sk-or-openrouter]", got)
	}
}
