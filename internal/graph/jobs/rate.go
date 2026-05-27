package jobs

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"golang.org/x/sync/semaphore"
)

// Rate is the per-source concurrency cap configuration.
type Rate struct {
	Slack      int64
	Jira       int64
	Github     int64
	Confluence int64
	Pagerduty  int64
	Datadog    int64
	Sentry     int64
	GWS        int64
	Gemini     int64
}

// DefaultRate returns the design defaults (matches app_settings seed).
func DefaultRate() Rate {
	return Rate{
		Slack:      5,
		Jira:       5,
		Github:     10,
		Confluence: 5,
		Pagerduty:  3,
		Datadog:    3,
		Sentry:     5,
		GWS:        5,
		Gemini:     4,
	}
}

// Semaphores wraps the per-system semaphore map.
type Semaphores struct {
	M map[string]*semaphore.Weighted
}

// NewSemaphores creates a Semaphores from a Rate configuration.
func NewSemaphores(r Rate) *Semaphores {
	m := map[string]*semaphore.Weighted{
		"slack":      semaphore.NewWeighted(r.Slack),
		"jira":       semaphore.NewWeighted(r.Jira),
		"github":     semaphore.NewWeighted(r.Github),
		"confluence": semaphore.NewWeighted(r.Confluence),
		"pagerduty":  semaphore.NewWeighted(r.Pagerduty),
		"datadog":    semaphore.NewWeighted(r.Datadog),
		"sentry":     semaphore.NewWeighted(r.Sentry),
		"gws":        semaphore.NewWeighted(r.GWS),
		"gemini":     semaphore.NewWeighted(r.Gemini),
	}
	return &Semaphores{M: m}
}

// Acquire grabs the named semaphore for one slot. Returns a release func.
// If the semaphore is unknown, the release is a no-op (defensive: unknown
// sources don't block, but a warning is logged by the caller).
func (s *Semaphores) Acquire(ctx context.Context, system string) (release func(), err error) {
	sem, ok := s.M[system]
	if !ok {
		return func() {}, nil
	}
	if err := sem.Acquire(ctx, 1); err != nil {
		return nil, fmt.Errorf("acquire semaphore %q: %w", system, err)
	}
	return func() { sem.Release(1) }, nil
}

// AcquireMany grabs multiple semaphores in sorted order to prevent deadlock.
// Returns a single release function that releases all in reverse order.
func (s *Semaphores) AcquireMany(ctx context.Context, systems []string) (release func(), err error) {
	// Sort to ensure consistent acquisition order — prevents deadlock.
	sorted := make([]string, len(systems))
	copy(sorted, systems)
	sort.Strings(sorted)

	releases := make([]func(), 0, len(sorted))
	for _, sys := range sorted {
		rel, err := s.Acquire(ctx, sys)
		if err != nil {
			// Release already-acquired semaphores in reverse order.
			for i := len(releases) - 1; i >= 0; i-- {
				releases[i]()
			}
			return nil, err
		}
		releases = append(releases, rel)
	}

	return func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}, nil
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
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Msg)
}

// Sentinel errors handlers may return.
var (
	ErrTransient = errors.New("transient")
	ErrFatal     = errors.New("fatal")
)

// IsRetryable: 5xx, 429, transient sentinel → true. 4xx (except 429), fatal → false.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrTransient) {
		return true
	}
	if errors.Is(err, ErrFatal) {
		return false
	}
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		if httpErr.Status == 429 {
			return true
		}
		if httpErr.Status >= 500 {
			return true
		}
		// 4xx other than 429
		return false
	}
	// Unknown errors are treated as transient by default.
	return true
}
