package jobs_test

import (
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

func singaporeTime(t *testing.T, hour, min int) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Singapore")
	if err != nil {
		t.Fatalf("LoadLocation Asia/Singapore: %v", err)
	}
	// Use an arbitrary fixed date.
	return time.Date(2024, 6, 15, hour, min, 0, 0, loc)
}

func TestIsWorkingHours_BoundaryStart(t *testing.T) {
	w := jobs.DefaultWindow()

	if w.IsWorkingHours(singaporeTime(t, 8, 59)) {
		t.Error("08:59 should NOT be working hours")
	}
	if !w.IsWorkingHours(singaporeTime(t, 9, 0)) {
		t.Error("09:00 should be working hours")
	}
}

func TestIsWorkingHours_BoundaryEnd(t *testing.T) {
	w := jobs.DefaultWindow()

	if !w.IsWorkingHours(singaporeTime(t, 18, 59)) {
		t.Error("18:59 should be working hours")
	}
	if w.IsWorkingHours(singaporeTime(t, 19, 0)) {
		t.Error("19:00 should NOT be working hours")
	}
}

func TestNextOffHours_FromMidDay(t *testing.T) {
	w := jobs.DefaultWindow()
	loc, _ := time.LoadLocation("Asia/Singapore")

	midday := singaporeTime(t, 14, 0)
	next := w.NextOffHours(midday)

	want := time.Date(2024, 6, 15, 19, 0, 0, 0, loc)
	if !next.Equal(want) {
		t.Errorf("NextOffHours(14:00) = %v, want %v", next, want)
	}
}

func TestNextOffHours_AlreadyOff(t *testing.T) {
	w := jobs.DefaultWindow()

	offHours := singaporeTime(t, 20, 0)
	next := w.NextOffHours(offHours)

	if !next.Equal(offHours) {
		t.Errorf("NextOffHours(20:00) = %v, want %v (same)", next, offHours)
	}
}

func TestScheduleBatch_DuringWorkingDay(t *testing.T) {
	w := jobs.DefaultWindow()
	loc, _ := time.LoadLocation("Asia/Singapore")

	now := singaporeTime(t, 10, 0)
	got := w.ScheduleBatch(now)

	want := time.Date(2024, 6, 15, 19, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("ScheduleBatch(10:00) = %v, want %v", got, want)
	}
}
