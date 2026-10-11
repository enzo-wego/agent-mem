package handlers

import (
	"strings"
	"testing"
)

func TestBuildSlackBackfillBody(t *testing.T) {
	id := func(s string) string { return s }
	const url = "https://wego.slack.com/archives/C019B36KGNR/p1791548199239769?thread_ts=1779440396.626379&cid=C019B36KGNR"
	share := slackAttachment{IsShare: true, IsMsgUnfurl: true, AuthorName: "someone", FromURL: url, Text: "fwd body"}

	t.Run("share block and url", func(t *testing.T) {
		got := buildSlackBackfillBody("hi", []slackAttachment{share}, id)
		want := "hi\n\n--- shared from someone ---\n" + url + "\nfwd body"
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	})
	t.Run("empty text with share is non-empty", func(t *testing.T) {
		if got := buildSlackBackfillBody("", []slackAttachment{share}, id); !strings.Contains(got, url) {
			t.Errorf("got %q", got)
		}
	})
	t.Run("no attachments unchanged", func(t *testing.T) {
		if got := buildSlackBackfillBody("hello", nil, id); got != "hello" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("attachment text normalized", func(t *testing.T) {
		got := buildSlackBackfillBody("", []slackAttachment{share}, func(s string) string { return "N(" + s + ")" })
		if !strings.Contains(got, "N(fwd body)") {
			t.Errorf("got %q", got)
		}
	})
	t.Run("non-share skipped", func(t *testing.T) {
		got := buildSlackBackfillBody("hi", []slackAttachment{{AuthorName: "a", Text: "t"}}, id)
		if got != "hi" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("fallback text and author omitted", func(t *testing.T) {
		got := buildSlackBackfillBody("", []slackAttachment{{IsMsgUnfurl: true, Fallback: "fb"}}, id)
		if got != "\n\n--- shared ---\nfb" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("all empty skipped", func(t *testing.T) {
		if got := buildSlackBackfillBody("x", []slackAttachment{{IsShare: true}}, id); got != "x" {
			t.Errorf("got %q", got)
		}
	})
}
