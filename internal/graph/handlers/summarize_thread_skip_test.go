package handlers

import "testing"

// TestSummarySkip pins the cache-key logic that replaced the force flag. The
// force path re-summarized threads whose input was byte-identical: 1,335 LLM
// calls/hour for 3 real updates. Every row below is a case that must NOT call
// the LLM, or must.
func TestSummarySkip(t *testing.T) {
	sigA := threadSummarySignature(3, 1785480000000)
	sigB := threadSummarySignature(4, 1785480999000) // a reply arrived
	const (
		linkA = "a1b2c3d4e5f60718"
		linkB = "0f1e2d3c4b5a6978" // the ticket's title landed
	)
	for _, tc := range []struct {
		name                                 string
		existingSig, sig, existingLink, link string
		wantSkip, wantBackfill               bool
	}{
		// The case the whole fix exists for: messages unchanged, links unchanged.
		// force=true used to re-summarize here on every single fetch_body.
		{"nothing changed", sigA, sigA, linkA, linkA, true, false},
		// The case force was reached for, and the only one it was right about.
		{"link title changed", sigA, sigA, linkA, linkB, false, false},
		{"messages changed", sigA, sigB, linkA, linkA, false, false},
		{"both changed", sigA, sigB, linkA, linkB, false, false},
		// No cached row at all — existingSig is "" and can never equal a real sig.
		{"no cached row", "", sigA, "", linkA, false, false},
		// Deploy safety: pre-migration rows store "". Skip and record the hash,
		// otherwise the first run re-summarizes every thread that has links.
		{"legacy row backfills", sigA, sigA, "", linkA, true, true},
		// Legacy row on a thread with no links: nothing to backfill.
		{"legacy row without links", sigA, sigA, "", "", true, false},
		// A thread that lost its last link must regenerate — the summary still
		// names a resource the thread no longer references.
		{"links removed", sigA, sigA, linkA, "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			skip, backfill := summarySkip(tc.existingSig, tc.sig, tc.existingLink, tc.link)
			if skip != tc.wantSkip || backfill != tc.wantBackfill {
				t.Fatalf("summarySkip(%q,%q,%q,%q) = (skip=%v, backfill=%v), want (skip=%v, backfill=%v)",
					tc.existingSig, tc.sig, tc.existingLink, tc.link, skip, backfill, tc.wantSkip, tc.wantBackfill)
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
