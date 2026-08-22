package fetchers

import (
	"errors"
	"testing"
)

func TestClassifySlackAPIError(t *testing.T) {
	tests := []struct {
		name      string
		code      string
		permanent bool
	}{
		{name: "not authed", code: "not_authed", permanent: true},
		{name: "invalid auth", code: "invalid_auth", permanent: true},
		{name: "account inactive", code: "account_inactive", permanent: true},
		{name: "token revoked", code: "token_revoked", permanent: true},
		{name: "token expired", code: "token_expired", permanent: true},
		{name: "missing scope", code: "missing_scope", permanent: true},
		{name: "channel not found", code: "channel_not_found", permanent: true},
		{name: "thread not found", code: "thread_not_found", permanent: true},
		{name: "message not found", code: "message_not_found", permanent: true},
		{name: "rate limited", code: "ratelimited", permanent: false},
		{name: "internal error", code: "internal_error", permanent: false},
		{name: "service unavailable", code: "service_unavailable", permanent: false},
		{name: "fatal error is Slack-transient", code: "fatal_error", permanent: false},
		{name: "unknown defaults transient", code: "new_slack_error", permanent: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := classifySlackAPIError(tt.code)
			var permanent *PermanentError
			if got := errors.As(err, &permanent); got != tt.permanent {
				t.Fatalf("classifySlackAPIError(%q) permanent = %v, want %v; err=%v",
					tt.code, got, tt.permanent, err)
			}
		})
	}
}
