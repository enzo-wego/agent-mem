package jobs

import "time"

// Window is the working-hours configuration.
type Window struct {
	TZ        *time.Location
	StartHour int // 9
	StartMin  int // 0
	EndHour   int // 19
	EndMin    int // 0
}

// DefaultWindow returns the default window: Asia/Singapore, 09:00-19:00.
func DefaultWindow() Window {
	loc, err := time.LoadLocation("Asia/Singapore")
	if err != nil {
		// Fallback to UTC if the timezone database is unavailable.
		loc = time.UTC
	}
	return Window{
		TZ:        loc,
		StartHour: 9,
		StartMin:  0,
		EndHour:   19,
		EndMin:    0,
	}
}

// IsWorkingHours reports whether t falls inside the working window.
func (w Window) IsWorkingHours(t time.Time) bool {
	local := t.In(w.TZ)
	h, m, _ := local.Clock()
	startMins := w.StartHour*60 + w.StartMin
	endMins := w.EndHour*60 + w.EndMin
	nowMins := h*60 + m
	return nowMins >= startMins && nowMins < endMins
}

// NextOffHours returns the next instant after t when off-hours starts.
// If t is already off-hours, returns t.
func (w Window) NextOffHours(t time.Time) time.Time {
	if !w.IsWorkingHours(t) {
		return t
	}
	local := t.In(w.TZ)
	y, mo, d := local.Date()
	// End of working day is EndHour:EndMin in TZ.
	endOfDay := time.Date(y, mo, d, w.EndHour, w.EndMin, 0, 0, w.TZ)
	return endOfDay
}

// ScheduleBatch returns an available_at for a batch-priority job: if now is
// inside working hours, returns the end-of-working-day in TZ; else returns now.
func (w Window) ScheduleBatch(now time.Time) time.Time {
	return w.NextOffHours(now)
}
