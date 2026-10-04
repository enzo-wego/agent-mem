package temporal

import (
	"testing"
	"time"
)

func TestParseWindows_Timezone(t *testing.T) {
	ict, err := time.LoadLocation("Asia/Ho_Chi_Minh")
	if err != nil {
		t.Fatal(err)
	}
	at := func(loc *time.Location, y int, m time.Month, d, h, mi int) time.Time {
		return time.Date(y, m, d, h, mi, 0, 0, loc)
	}
	cases := []struct {
		name       string
		q          string
		now        time.Time
		loc        *time.Location
		start, end time.Time
	}{
		{"yesterday_early_morning", "yesterday", at(ict, 2026, 10, 4, 6, 0), ict,
			at(ict, 2026, 10, 3, 0, 0), at(ict, 2026, 10, 4, 0, 0)},
		{"this_month_end", "this month", at(ict, 2026, 10, 31, 23, 30), ict,
			at(ict, 2026, 10, 1, 0, 0), at(ict, 2026, 11, 1, 0, 0)},
		{"last_year_new_year", "last year", at(ict, 2027, 1, 1, 0, 30), ict,
			at(ict, 2026, 1, 1, 0, 0), at(ict, 2027, 1, 1, 0, 0)},
		{"iso_date", "on 2026-09-15", at(ict, 2026, 10, 4, 6, 0), ict,
			at(ict, 2026, 9, 15, 0, 0), at(ict, 2026, 9, 16, 0, 0)},
		{"utc_location", "yesterday", at(ict, 2026, 10, 4, 6, 0), time.UTC,
			at(time.UTC, 2026, 10, 2, 0, 0), at(time.UTC, 2026, 10, 3, 0, 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, _, ok := Parse(tc.q, tc.now, tc.loc)
			if !ok {
				t.Fatal("no match")
			}
			if !w.Start.Equal(tc.start) || !w.End.Equal(tc.end) {
				t.Errorf("window = [%s, %s), want [%s, %s)", w.Start, w.End, tc.start, tc.end)
			}
		})
	}
}

// Passes only because the package embeds time/tzdata: run it in an image with
// no zoneinfo to prove that.
func TestTZData_NoSystemZoneinfo(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Ho_Chi_Minh")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	if _, off := time.Date(2026, 1, 1, 0, 0, 0, 0, loc).Zone(); off != 7*3600 {
		t.Fatalf("offset = %d, want +7h", off)
	}
}
