package handlers

import (
	"testing"
	"time"
)

func TestHandlersShareDefaultClock(t *testing.T) {
	fixed := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	old := nowFunc
	nowFunc = func() time.Time { return fixed }
	t.Cleanup(func() { nowFunc = old })

	if got := nowFunc(); !got.Equal(fixed) {
		t.Fatalf("nowFunc() = %v, want %v", got, fixed)
	}
}
