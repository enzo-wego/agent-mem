package llmgateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// PermitWait bounds the time a generate call may wait for one of the shared
// dispatch permits before returning a retryable admission error.
const PermitWait = 30 * time.Second

type generateLimiter struct {
	mu sync.Mutex

	cap        int
	tokens     float64
	lastRefill time.Time

	countHour time.Time
	count     int
	loggedCap bool
}

var (
	sharedLimiter     generateLimiter
	limiterNow        = time.Now
	limiterPermitWait = PermitWait
	limiterPermits    = make(chan struct{}, 2)
	acquirePermit     = acquireGeneratePermit
)

func (l *generateLimiter) capacityLocked() float64 {
	capacity := float64(l.cap) / 12
	if capacity < 1 {
		return 1
	}
	return capacity
}

func (l *generateLimiter) refillLocked(now time.Time) {
	if l.cap <= 0 {
		return
	}
	if l.lastRefill.IsZero() {
		l.lastRefill = now
		return
	}
	if !now.After(l.lastRefill) {
		return
	}
	l.tokens += now.Sub(l.lastRefill).Seconds() * float64(l.cap) / 3600
	if capacity := l.capacityLocked(); l.tokens > capacity {
		l.tokens = capacity
	}
	l.lastRefill = now
}

func (l *generateLimiter) setCap(n int) {
	if n < 0 {
		n = 0
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if n == l.cap {
		return
	}

	now := limiterNow()
	switch {
	case l.cap <= 0 && n > 0:
		l.cap = n
		l.tokens = l.capacityLocked()
		l.lastRefill = now
	case l.cap > 0 && n > 0:
		l.refillLocked(now)
		l.cap = n
		if capacity := l.capacityLocked(); l.tokens > capacity {
			l.tokens = capacity
		}
		l.lastRefill = now
	default:
		l.cap = 0
		l.tokens = 0
		l.lastRefill = now
	}
}

func (l *generateLimiter) capped(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refillLocked(now)
	return l.cap > 0 && l.tokens < 1
}

func (l *generateLimiter) admit(now time.Time) (count, cap int, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refillLocked(now)
	if l.cap > 0 && l.tokens < 1 {
		return l.count, l.cap, false
	}
	if l.cap > 0 {
		l.tokens--
	}
	l.tickCountLocked(now)
	l.count++
	return l.count, l.cap, true
}

func (l *generateLimiter) callCount(now time.Time) (count, cap int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tickCountLocked(now)
	return l.count, l.cap
}

func (l *generateLimiter) tickCountLocked(now time.Time) {
	hour := now.Truncate(time.Hour)
	if !l.countHour.Equal(hour) {
		l.countHour = hour
		l.count = 0
		l.loggedCap = false
	}
}

func (l *generateLimiter) capHit(now time.Time) (cap, count int, shouldLog bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tickCountLocked(now)
	cap, count = l.cap, l.count
	if !l.loggedCap {
		l.loggedCap = true
		shouldLog = true
	}
	return cap, count, shouldLog
}

func tokenCapError(caller, tier string) error {
	cap, count, shouldLog := sharedLimiter.capHit(limiterNow())
	if shouldLog {
		log.Warn().Int("cap", cap).Int("count", count).Msg("LLM hourly cap token unavailable")
	}
	return admissionError(
		fmt.Sprintf("hourly cap of %d generate calls has no token available (caller=%s tier=%s)", cap, caller, tier),
		nil,
	)
}

func admissionError(reason string, cause error) error {
	if cause != nil {
		return fmt.Errorf("%w: %w: %w: %s", ErrUnreachable, ErrCapped, cause, reason)
	}
	return fmt.Errorf("%w: %w: %s", ErrUnreachable, ErrCapped, reason)
}

func classifyPermitContext(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return admissionError("context deadline expired while waiting for shared generate permit", err)
}

func acquireGeneratePermit(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, classifyPermitContext(err)
	}

	permits := limiterPermits
	select {
	case permits <- struct{}{}:
		return func() { <-permits }, nil
	default:
	}

	timer := time.NewTimer(limiterPermitWait)
	defer timer.Stop()
	select {
	case permits <- struct{}{}:
		return func() { <-permits }, nil
	case <-ctx.Done():
		return nil, classifyPermitContext(ctx.Err())
	case <-timer.C:
		return nil, admissionError("timed out waiting for shared generate permit", nil)
	}
}
