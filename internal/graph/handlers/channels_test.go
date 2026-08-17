package handlers

import (
	"encoding/json"
	"strings"
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

func TestFlattenLines(t *testing.T) {
	// Production regression (node slack:C09H1QMK882:1786709371.372099). firstLine
	// cut this multi-line body at "Hi @Supriya @liping", deleting the release ask;
	// flattenLines must keep the whole body so the summarizer and topic judge see
	// the real content. This is the case that fails if the bug returns.
	body := "Hi @Supriya @liping\n" +
		"Now that we have completed all the changes for PK & if all is good with the latest\n" +
		"test cases, we'd like to plan the release of PK taxation.\n" +
		"To keep things streamlined from a filing perspective, 1st September can be targetted.\n" +
		"Let us know what you think.\n" +
		"cc @Payments Geeks @Alex"
	got := flattenLines(body, 400)
	for _, want := range []string{"PK taxation", "1st September"} {
		if !strings.Contains(got, want) {
			t.Errorf("flattenLines(prod body) = %q, missing %q", got, want)
		}
	}

	cases := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"single line unchanged", "just a normal title", 280, "just a normal title"},
		{"two lines separator", "line1\nline2", 280, "line1 / line2"},
		{"blank line no doubled sep", "one\n\ntwo", 280, "one / two"},
		{"blank whitespace line no doubled sep", "one\n \ntwo", 280, "one / two"},
		{"leading/trailing newlines trimmed", "\n\nhello\nworld\n\n", 280, "hello / world"},
		{"internal whitespace collapsed", "a\t\tb   c", 280, "a b c"},
		{"empty", "", 280, ""},
		{"whitespace only", "   \n\t \n ", 280, ""},
	}
	for _, c := range cases {
		if got := flattenLines(c.in, c.n); got != c.want {
			t.Errorf("%s: flattenLines(%q, %d) = %q, want %q", c.name, c.in, c.n, got, c.want)
		}
	}

	// A single-line input must be byte-for-byte identical to firstLine, truncation
	// included, so swapping helpers at a title-style call site would be a no-op.
	for _, s := range []string{"just a normal title", strings.Repeat("x", 500)} {
		if fl, first := flattenLines(s, 280), firstLine(s, 280); fl != first {
			t.Errorf("single-line divergence for %.20q: flattenLines=%q firstLine=%q", s, fl, first)
		}
	}

	// Over-cap: exactly n runes plus the ellipsis, matching firstLine's convention.
	if over, want := flattenLines(strings.Repeat("a", 500), 400), strings.Repeat("a", 400)+"…"; over != want {
		t.Errorf("over-cap = %q (%d runes), want 400 runes + ellipsis", over, len([]rune(over)))
	}

	// Multi-byte input truncates on rune boundaries, not bytes: 10 runes capped at
	// 3 yields 3 intact runes + "…".
	if mb, want := flattenLines(strings.Repeat("界", 10), 3), "界界界…"; mb != want {
		t.Errorf("multibyte = %q, want %q", mb, want)
	} else if r := []rune(mb); len(r) != 4 {
		t.Errorf("multibyte rune count = %d, want 4", len(r))
	}
}
