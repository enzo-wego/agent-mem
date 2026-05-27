package jobs_test

import (
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

func TestBackoff_Sequence(t *testing.T) {
	base := 30 * time.Second
	cap_ := 1 * time.Hour

	cases := []struct {
		attempts int16
		min, max time.Duration
	}{
		{1, 24 * time.Second, 36 * time.Second},   // 30s ±20%
		{2, 48 * time.Second, 72 * time.Second},   // 60s ±20%
		{3, 96 * time.Second, 144 * time.Second},  // 120s ±20%
		{4, 192 * time.Second, 288 * time.Second}, // 240s ±20%
		// attempts=7: 30s * 2^6 = 1920s = 32min, ±20% → [25.6min, 38.4min]
		{7, 24 * time.Minute, 40 * time.Minute},
		// attempts=20: 30s * 2^19 >> cap=1h → capped at 1h, ±20% → [48min, 1h]
		{20, 48 * time.Minute, 1 * time.Hour},
	}
	for _, tc := range cases {
		for i := 0; i < 50; i++ {
			d := jobs.Backoff(tc.attempts, base, cap_)
			if d < tc.min || d > tc.max {
				t.Errorf("attempts=%d sample %d: got %v, want [%v..%v]",
					tc.attempts, i, d, tc.min, tc.max)
				break
			}
		}
	}
}

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{jobs.NewHTTPError(500, "server"), true},
		{jobs.NewHTTPError(503, "unavailable"), true},
		{jobs.NewHTTPError(429, "rate"), true},
		{jobs.NewHTTPError(404, "not found"), false},
		{jobs.NewHTTPError(401, "auth"), false},
		{jobs.ErrTransient, true},
		{jobs.ErrFatal, false},
	}
	for _, tc := range cases {
		got := jobs.IsRetryable(tc.err)
		if got != tc.want {
			t.Errorf("IsRetryable(%v): got %v want %v", tc.err, got, tc.want)
		}
	}
}
