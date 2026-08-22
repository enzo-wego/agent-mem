package handlers

import (
	"os"
	"testing"
)

// TestMain hardens the suite against accidental real-network calls: several
// handlers DM via api.slack.com using whatever token Deps carries, and a test
// that reached the real Slack API with a placeholder token would be both slow
// and wrong. Point slackAPIBaseURL at an unroutable loopback address for every
// test unless a test explicitly stubs it (see credential_guard_test.go); a
// stray real call then fails fast with a connection refused instead of
// reaching Slack. Production code never reads this var outside tests.
func TestMain(m *testing.M) {
	slackAPIBaseURL = "http://127.0.0.1:1" // port 1: nothing listens there
	os.Exit(m.Run())
}
