package normalizer

import (
	"context"
	"testing"
)

func TestSlackNormalizer(t *testing.T) {
	ctx := context.Background()
	cache := NewMemoryCache(map[string]string{
		"slack/U02FKR154T1": "Alexandre Morin",
		"slack/U1":          "Alice",
		"slack/U2":          "Bob",
	})
	n := NewSlackNormalizer(cache)

	if n.Source() != "slack" {
		t.Fatalf("Source() = %q, want \"slack\"", n.Source())
	}

	tests := []struct {
		name         string
		input        string
		wantText     string
		wantMentions []Mention
	}{
		{
			name:     "empty string",
			input:    "",
			wantText: "",
		},
		{
			name:     "mention at start",
			input:    "<@U1> hello",
			wantText: "@Alice hello",
			wantMentions: []Mention{
				{Source: "slack", ExternalID: "U1", DisplayName: "Alice"},
			},
		},
		{
			name:     "mention at middle",
			input:    "hello <@U1> world",
			wantText: "hello @Alice world",
			wantMentions: []Mention{
				{Source: "slack", ExternalID: "U1", DisplayName: "Alice"},
			},
		},
		{
			name:     "mention at end",
			input:    "hello <@U1>",
			wantText: "hello @Alice",
			wantMentions: []Mention{
				{Source: "slack", ExternalID: "U1", DisplayName: "Alice"},
			},
		},
		{
			name:     "consecutive mentions",
			input:    "<@U1> <@U2>",
			wantText: "@Alice @Bob",
			wantMentions: []Mention{
				{Source: "slack", ExternalID: "U1", DisplayName: "Alice"},
				{Source: "slack", ExternalID: "U2", DisplayName: "Bob"},
			},
		},
		{
			name:     "unknown user fallback",
			input:    "<@U99>",
			wantText: "@U99",
			wantMentions: []Mention{
				{Source: "slack", ExternalID: "U99", DisplayName: "U99"},
			},
		},
		{
			name:     "channel with alias",
			input:    "<#C123|general>",
			wantText: "#general",
		},
		{
			name:     "channel without alias",
			input:    "<#C123>",
			wantText: "#C123",
		},
		{
			name:     "subteam with alias",
			input:    "<!subteam^S01TMG8Q65R|@devs>",
			wantText: "@devs",
			wantMentions: []Mention{
				// DisplayName stores the clean handle (leading @ stripped from Slack alias).
				{Source: "slack_group", ExternalID: "S01TMG8Q65R", DisplayName: "devs"},
			},
		},
		{
			name:     "subteam without alias",
			input:    "<!subteam^S01TMG8Q65R>",
			wantText: "@S01TMG8Q65R",
			wantMentions: []Mention{
				{Source: "slack_group", ExternalID: "S01TMG8Q65R"},
			},
		},
		{
			name:     "broadcast here",
			input:    "<!here> standup time",
			wantText: "@here standup time",
		},
		{
			name:     "broadcast channel",
			input:    "<!channel> everyone",
			wantText: "@channel everyone",
		},
		{
			name:     "broadcast everyone",
			input:    "<!everyone> listen up",
			wantText: "@everyone listen up",
		},
		{
			name:     "URL with label",
			input:    "<https://example.com|click here>",
			wantText: "click here (https://example.com)",
		},
		{
			name:     "URL without label",
			input:    "<https://example.com>",
			wantText: "https://example.com",
		},
		{
			name:     "URL where label contains pipe - use last pipe as separator",
			input:    "<https://example.com/a|b|label with pipe>",
			wantText: "label with pipe (https://example.com/a|b)",
		},
		{
			name:     "HTML entity not a real token - unescaped last",
			input:    "&lt;not a real token&gt;",
			wantText: "<not a real token>",
		},
		{
			name:     "mixed entities and slack tokens",
			input:    "&lt;hello&gt; <@U1> &amp; <https://x.com|link>",
			wantText: "<hello> @Alice & link (https://x.com)",
			wantMentions: []Mention{
				{Source: "slack", ExternalID: "U1", DisplayName: "Alice"},
			},
		},
		{
			name:     "bold stripping",
			input:    "*hello world*",
			wantText: "hello world",
		},
		{
			name:     "italic stripping",
			input:    "_hello world_",
			wantText: "hello world",
		},
		{
			name:     "strike stripping",
			input:    "~hello world~",
			wantText: "hello world",
		},
		{
			name: "real fixture: Lei TRY-thread parent message",
			input: "<@U02FKR154T1> for TRY we need to split, cko return this error <https://wego.slack.com/archives/C08S954G2LX/p1779710863216389?thread_ts=1779709917.613979&amp;channel=C08S954G2LX&amp;message_ts=1779710863.216389|original>",
			wantText: "@Alexandre Morin for TRY we need to split, cko return this error original (https://wego.slack.com/archives/C08S954G2LX/p1779710863216389?thread_ts=1779709917.613979&channel=C08S954G2LX&message_ts=1779710863.216389)",
			wantMentions: []Mention{
				{Source: "slack", ExternalID: "U02FKR154T1", DisplayName: "Alexandre Morin"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := n.Normalize(ctx, []byte(tc.input), nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Text != tc.wantText {
				t.Errorf("Text:\n  got  %q\n  want %q", res.Text, tc.wantText)
			}
			if len(res.Mentions) != len(tc.wantMentions) {
				t.Errorf("Mentions count: got %d, want %d\n  got:  %+v\n  want: %+v",
					len(res.Mentions), len(tc.wantMentions), res.Mentions, tc.wantMentions)
				return
			}
			for i, m := range res.Mentions {
				wm := tc.wantMentions[i]
				if m.Source != wm.Source || m.ExternalID != wm.ExternalID {
					t.Errorf("Mention[%d]: got {%s %s}, want {%s %s}",
						i, m.Source, m.ExternalID, wm.Source, wm.ExternalID)
				}
				if wm.DisplayName != "" && m.DisplayName != wm.DisplayName {
					t.Errorf("Mention[%d].DisplayName: got %q, want %q", i, m.DisplayName, wm.DisplayName)
				}
			}
		})
	}
}
