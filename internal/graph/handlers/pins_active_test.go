package handlers

import (
	"os"
	"testing"
)

func TestBoardActiveHours(t *testing.T) {
	os.Unsetenv("AGENT_MEM_BOARD_ACTIVE_HOURS")
	if got := boardActiveHours(); got != 24 {
		t.Fatalf("default = %d, want 24", got)
	}
	t.Setenv("AGENT_MEM_BOARD_ACTIVE_HOURS", "48")
	if got := boardActiveHours(); got != 48 {
		t.Fatalf("override = %d, want 48", got)
	}
	t.Setenv("AGENT_MEM_BOARD_ACTIVE_HOURS", "0") // non-positive → default
	if got := boardActiveHours(); got != 24 {
		t.Fatalf("zero override = %d, want 24 (fallback)", got)
	}
	t.Setenv("AGENT_MEM_BOARD_ACTIVE_HOURS", "abc") // garbage → default
	if got := boardActiveHours(); got != 24 {
		t.Fatalf("garbage override = %d, want 24 (fallback)", got)
	}
}

func TestCountActiveThreads(t *testing.T) {
	threads := []pinnedThread{
		{LastMs: 1000}, // == cutoff → counts
		{LastMs: 1500}, // > cutoff → counts
		{LastMs: 999},  // < cutoff → not
		{LastMs: 0},    // no activity → not
	}
	if got := countActiveThreads(threads, 1000); got != 2 {
		t.Fatalf("countActiveThreads = %d, want 2", got)
	}
	if got := countActiveThreads(nil, 1000); got != 0 {
		t.Fatalf("countActiveThreads(nil) = %d, want 0", got)
	}
}
