package jobs

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"time"
)

// Backoff returns the retry delay for the given attempt count.
// Formula: base * 2^(attempts-1), capped, with ±20% jitter.
// attempts==1 means "this is the first retry" — already-incremented.
func Backoff(attempts int16, base, cap_ time.Duration) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	mult := math.Pow(2, float64(attempts-1))
	d := time.Duration(float64(base) * mult)
	if d > cap_ {
		d = cap_
	}
	// ±20% jitter
	jitter := 1.0 + (rand.Float64()*0.4 - 0.2)
	d = time.Duration(float64(d) * jitter)
	if d > cap_ {
		d = cap_
	}
	return d
}

// HTTPError carries an HTTP status code for retryability classification.
type HTTPError struct {
	Status int
	Msg    string
}

// NewHTTPError creates an HTTPError.
func NewHTTPError(status int, msg string) *HTTPError {
	return &HTTPError{Status: status, Msg: msg}
}

// Error implements the error interface.
func (e *HTTPError) Error() string {
	return fmt.Sprintf("http %d: %s", e.Status, e.Msg)
}

// Sentinel errors handlers may return.
var (
	ErrTransient = errors.New("transient")
	ErrFatal     = errors.New("fatal")
)

// IsRetryable returns true if the worker should reschedule rather than
// mark failed. Rule: 5xx, 429, transient sentinel, or anything that
// isn't an HTTPError with 4xx (other than 429) and isn't ErrFatal.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrFatal) {
		return false
	}
	if errors.Is(err, ErrTransient) {
		return true
	}
	var he *HTTPError
	if errors.As(err, &he) {
		if he.Status == 429 || he.Status >= 500 {
			return true
		}
		// Any other HTTP status (4xx, 3xx, 2xx) from an HTTPError → not retryable.
		return false
	}
	// Network errors, DB serialisation errors, anything else — retry.
	return true
}
