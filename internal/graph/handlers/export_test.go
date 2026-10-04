package handlers

import "time"

// SetSearchNow pins the clock the search handler boosts and parses windows with.
func SetSearchNow(s *Search, now func() time.Time) { s.now = now }
