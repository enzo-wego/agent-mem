package llmgateway

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// IsRetryable is what stands between an LLM outage and permanent data loss:
// pending_messages.status='failed' is terminal and nothing re-claims it, so a
// transient fault misclassified as permanent silently discards the observation.
// The opposite mistake is also real — a 400 classified as retryable requeues
// forever and the queue never drains.
func TestIsRetryable(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},

		// Gateway down: the work is still valid and the service will come back.
		{"unreachable", fmt.Errorf("%w: dial tcp: refused", ErrUnreachable), true},

		// Seat quota window — reopens on a timer. The single most important
		// retryable case, since it is the expected steady-state failure.
		{"503 seat quota", &StatusError{Path: "/generate", Code: 503}, true},
		{"429 rate limited", &StatusError{Path: "/generate", Code: 429}, true},
		{"502 upstream", &StatusError{Path: "/generate", Code: 502}, true},
		{"500 gateway bug", &StatusError{Path: "/generate", Code: 500}, true},
		{"504 timeout", &StatusError{Path: "/generate", Code: 504}, true},
		{"408 request timeout", &StatusError{Path: "/embed", Code: 408}, true},

		// These will never succeed on their own. Retrying burns the queue.
		{"400 bad request", &StatusError{Path: "/generate", Code: 400}, false},
		{"401 bad key", &StatusError{Path: "/generate", Code: 401}, false},
		{"404 wrong path", &StatusError{Path: "/nope", Code: 404}, false},
		{"422 unprocessable", &StatusError{Path: "/embed", Code: 422}, false},

		// A parse failure means the gateway answered with something we cannot
		// use; retrying yields the same bytes.
		{"decode failure", errors.New("llm-gateway: decode /generate: bad json"), false},

		// Wrapping must survive: callers add context as errors bubble up.
		{"wrapped unreachable", fmt.Errorf("summarise: %w",
			fmt.Errorf("%w: dial tcp", ErrUnreachable)), true},
		{"wrapped 503", fmt.Errorf("summarise: %w", &StatusError{Code: 503}), true},
		{"wrapped 401", fmt.Errorf("summarise: %w", &StatusError{Code: 401}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRetryable(tc.err); got != tc.want {
				verb := map[bool]string{true: "requeued", false: "dropped"}
				t.Errorf("IsRetryable(%v) = %v, want %v — the message would be %s",
					tc.err, got, tc.want, verb[got])
			}
		})
	}
}

// The status code and body must survive into the message: the 503 body carries
// the seat reset time, which is the detail that makes the failure actionable.
func TestStatusErrorMessage(t *testing.T) {
	e := &StatusError{Path: "/generate", Code: 503, Body: `{"detail":"resets_at=1785534600"}`}
	msg := e.Error()
	for _, want := range []string{"/generate", "503", "1785534600"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q lost %q", msg, want)
		}
	}
}
