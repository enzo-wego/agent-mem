package handlers

import "testing"

func TestSortBoardGroups(t *testing.T) {
	groups := []boardEpicGroup{
		{EpicKey: "", EpicRank: boardEpicNoRank, LastMs: 999}, // no-epic, newest — must sink to bottom
		{EpicKey: "PAY-50", EpicRank: 2, LastMs: 10},
		{EpicKey: "PAY-100", EpicRank: 0, LastMs: 5},
		{EpicKey: "PAY-70", EpicRank: 1, LastMs: 20},
		{EpicKey: "PAY-999", EpicRank: boardEpicNoRank, LastMs: 50}, // real epic, off-board → after ranked, ties by LastMs
		{EpicKey: "PAY-888", EpicRank: boardEpicNoRank, LastMs: 40},
	}
	sortBoardGroups(groups)

	got := make([]string, len(groups))
	for i, g := range groups {
		got[i] = g.EpicKey
	}
	want := []string{"PAY-100", "PAY-70", "PAY-50", "PAY-999", "PAY-888", ""}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}
