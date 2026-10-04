package temporal

import (
	"testing"
	"time"
)

// now is fixed to Sunday 2026-09-27 so relative phrases are deterministic.
var now = time.Date(2026, 9, 27, 15, 4, 5, 0, time.UTC)

func d(y int, m time.Month, day int) time.Time {
	return time.Date(y, m, day, 0, 0, 0, 0, time.UTC)
}

func TestParse(t *testing.T) {
	cases := []struct {
		name       string
		q          string
		start, end time.Time
		rest       string
		ok         bool
	}{
		{"in August is the whole month of August 2026", "GST invoices in August",
			d(2026, 8, 1), d(2026, 9, 1), "GST invoices", true},
		{"bare month after now rolls back a year", "refunds in December",
			d(2025, 12, 1), d(2026, 1, 1), "refunds", true},
		{"bare month equal to now is this year", "refunds in September",
			d(2026, 9, 1), d(2026, 10, 1), "refunds", true},
		{"abbreviated month", "Tabby settlement Aug",
			d(2026, 8, 1), d(2026, 9, 1), "Tabby settlement", true},
		{"month year", "chargebacks in March 2025",
			d(2025, 3, 1), d(2025, 4, 1), "chargebacks", true},
		{"iso date", "outage on 2026-07-14",
			d(2026, 7, 14), d(2026, 7, 15), "outage", true},
		{"iso range", "deploys from 2026-07-01 to 2026-07-10",
			d(2026, 7, 1), d(2026, 7, 11), "deploys", true},
		{"iso range wins over bare date", "2026-07-01..2026-07-03 incidents",
			d(2026, 7, 1), d(2026, 7, 4), "incidents", true},
		{"last week is the previous Monday to Sunday", "PK IP blocks last week",
			d(2026, 9, 14), d(2026, 9, 21), "PK IP blocks", true},
		{"last month", "Juspay errors in the last month",
			d(2026, 8, 1), d(2026, 9, 1), "Juspay errors", true},
		{"last quarter", "OKRs last quarter",
			d(2026, 4, 1), d(2026, 7, 1), "OKRs", true},
		{"this week runs to end of today", "this week deploys",
			d(2026, 9, 21), d(2026, 9, 28), "deploys", true},
		{"this month", "what shipped this month",
			d(2026, 9, 1), d(2026, 9, 28), "what shipped", true},
		{"quarter", "revenue Q2 2026",
			d(2026, 4, 1), d(2026, 7, 1), "revenue", true},
		{"since month is open ended, not the month", "Apple Pay since August",
			d(2026, 8, 1), d(2026, 9, 28), "Apple Pay", true},
		{"since date", "since 2026-09-01 refunds",
			d(2026, 9, 1), d(2026, 9, 28), "refunds", true},
		{"yesterday", "alerts yesterday",
			d(2026, 9, 26), d(2026, 9, 27), "alerts", true},
		{"today", "what broke today",
			d(2026, 9, 27), d(2026, 9, 28), "what broke", true},
		{"bare may is a verb, not a month", "refunds may fail",
			time.Time{}, time.Time{}, "refunds may fail", false},
		{"in May is the month", "refunds in May",
			d(2026, 5, 1), d(2026, 6, 1), "refunds", true},
		{"no time phrase", "TripleA refund status",
			time.Time{}, time.Time{}, "TripleA refund status", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, rest, ok := Parse(tc.q, now, time.UTC)
			start, end := w.Start, w.End
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if rest != tc.rest {
				t.Errorf("rest = %q, want %q", rest, tc.rest)
			}
			if !ok {
				return
			}
			if !start.Equal(tc.start) || !end.Equal(tc.end) {
				t.Errorf("window = [%s, %s), want [%s, %s)",
					start.Format(time.RFC3339), end.Format(time.RFC3339),
					tc.start.Format(time.RFC3339), tc.end.Format(time.RFC3339))
			}
		})
	}
}

func TestWindowProximity(t *testing.T) {
	w := Window{d(2026, 8, 1), d(2026, 9, 1)}
	if p := w.Proximity(w.Centre()); p != 1 {
		t.Errorf("centre = %v, want 1", p)
	}
	if p := w.Proximity(d(2026, 8, 1)); p != 0 {
		t.Errorf("start edge = %v, want 0", p)
	}
	if p := w.Proximity(d(2026, 10, 1)); p != 0 {
		t.Errorf("outside = %v, want 0", p)
	}
	if p := w.Proximity(d(2026, 8, 24)); p <= 0.4 || p >= 0.6 {
		t.Errorf("quarter-way from centre = %v, want ≈0.5", p)
	}
}

func TestRoundRobin(t *testing.T) {
	w := Window{d(2026, 8, 1), d(2026, 8, 9)} // 8 days → one day per bucket
	in := []Dated{
		{"a1", d(2026, 8, 1), 0.9},
		{"a2", d(2026, 8, 1), 0.8},
		{"a3", d(2026, 8, 1), 0.7},
		{"b1", d(2026, 8, 4), 0.6},
		{"c1", d(2026, 8, 8), 0.5},
		{"late", d(2026, 9, 1), 0.4}, // outside → last bucket, after c1
	}
	got := RoundRobin(in, w, 8)
	want := []string{"a1", "b1", "c1", "a2", "late", "a3"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("pos %d = %s, want %s (got %v)", i, got[i].ID, id, got)
		}
	}
	if out := RoundRobin(in[:1], w, 8); len(out) != 1 {
		t.Errorf("single item passthrough failed")
	}
}
