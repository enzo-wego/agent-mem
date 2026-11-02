package handlers

import "testing"

// TestSummarySkip pins the six-input cache decision, including safe legacy
// backfills and bot-only full-signature changes.
func TestSummarySkip(t *testing.T) {
	fullA := threadSummarySignature(3, 1785480000000)
	fullB := threadSummarySignature(4, 1785480999000)
	humanA := "h1:1:1785480000000"
	humanB := "h1:2:1785480999000"
	const (
		linkA = "a1b2c3d4e5f60718"
		linkB = "0f1e2d3c4b5a6978"
	)
	for _, tc := range []struct {
		name                                                         string
		existingFull, full, existingHuman, human, existingLink, link string
		wantSkip, wantBackfillHuman, wantBackfillLink                bool
	}{
		{"no row", "", fullA, "", humanA, "", linkA, false, false, false},
		{"old version", "v8:3:1785480000000", fullA, humanA, humanA, linkA, linkA, false, false, false},
		{"legacy equal full", fullA, fullA, "", humanA, linkA, linkA, true, true, false},
		{"legacy differing full", fullA, fullB, "", humanA, linkA, linkA, false, false, false},
		{"equal human ignores differing full", fullA, fullB, humanA, humanA, linkA, linkA, true, false, false},
		{"differing human", fullA, fullB, humanA, humanB, linkA, linkA, false, false, false},
		{"differing link", fullA, fullB, humanA, humanA, linkA, linkB, false, false, false},
		{"empty existing link backfills", fullA, fullB, humanA, humanA, "", linkA, true, false, true},
		{"legacy both empty backfills both", fullA, fullA, "", humanA, "", linkA, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			skip, backfillHuman, backfillLink := summarySkip(
				tc.existingFull, tc.full, tc.existingHuman, tc.human, tc.existingLink, tc.link,
			)
			if skip != tc.wantSkip || backfillHuman != tc.wantBackfillHuman || backfillLink != tc.wantBackfillLink {
				t.Fatalf("summarySkip(%q,%q,%q,%q,%q,%q) = (%v,%v,%v), want (%v,%v,%v)",
					tc.existingFull, tc.full, tc.existingHuman, tc.human, tc.existingLink, tc.link,
					skip, backfillHuman, backfillLink,
					tc.wantSkip, tc.wantBackfillHuman, tc.wantBackfillLink)
			}
		})
	}
}

// TestLinkSignature covers the two properties the skip check depends on: the
// same resource block always hashes the same (or unchanged links would look
// changed and re-summarize forever), and a changed title always hashes
// differently (or a landed title would never reach the summary).
func TestLinkSignature(t *testing.T) {
	const block = "Linked resources:\n- Jira: PAY-2111 tax rounding\n\nThread (oldest first):\n"

	if got, want := linkSignature(""), ""; got != want {
		t.Errorf("no linked resources: got %q, want %q", got, want)
	}
	// Hash content, not string identity: two equal blocks built separately must
	// agree, or an unchanged thread would re-summarize on every fetch.
	rebuilt := "Linked resources:\n- Jira: PAY-2111 tax rounding\n" + "\nThread (oldest first):\n"
	if linkSignature(block) != linkSignature(rebuilt) {
		t.Error("equal content hashed differently; unchanged links would re-summarize every time")
	}
	// The exact case fetch_body triggers on: an untitled resource gets its title.
	untitled := "Linked resources:\n- Jira: PAY-2111\n\nThread (oldest first):\n"
	if linkSignature(untitled) == linkSignature(block) {
		t.Error("a landed title did not change the signature; the refresh would be skipped")
	}
}
