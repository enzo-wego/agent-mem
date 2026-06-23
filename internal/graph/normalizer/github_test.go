package normalizer

import (
	"context"
	"strings"
	"testing"
)

func TestGitHubNormalizer(t *testing.T) {
	ctx := context.Background()
	n := NewGitHubNormalizer()

	if n.Source() != "github" {
		t.Fatalf("Source() = %q, want \"github\"", n.Source())
	}

	tests := []struct {
		name         string
		input        string
		wantContains []string
		wantAbsent   []string
		wantMentions []Mention
	}{
		{
			name:  "empty body",
			input: "",
		},
		{
			name: "real PR body fragment",
			input: `## Summary

This PR adds retry logic for transient errors.

Fixes #123, closes #456.

cc @alice and @bob-dev`,
			wantContains: []string{"Summary", "retry logic", "cc"},
			wantMentions: []Mention{
				{Source: "github", ExternalID: "alice"},
				{Source: "github", ExternalID: "bob-dev"},
			},
		},
		{
			name: "CodeRabbit walkthrough strips HTML comment",
			input: `## Changes
<!-- coderabbit:state hidden data here -->
Some real content`,
			wantContains: []string{"Some real content"},
			wantAbsent:   []string{"coderabbit:state"},
		},
		{
			name: "front-matter stripped",
			input: `---
title: My PR
author: enzo
---
## Body here`,
			wantContains: []string{"Body here"},
			wantAbsent:   []string{"title: My PR"},
		},
		{
			name: "mention inside fenced code block not extracted",
			input: "normal @realuser\n```\n@not-a-mention\n```",
			wantMentions: []Mention{
				{Source: "github", ExternalID: "realuser"},
			},
		},
		{
			name: "CRLF normalised",
			input: "line one\r\nline two\r\n",
			wantContains: []string{"line one\nline two"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := n.Normalize(ctx, []byte(tc.input), nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(res.Text, want) {
					t.Errorf("Text missing %q\n  got: %q", want, res.Text)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(res.Text, absent) {
					t.Errorf("Text should not contain %q\n  got: %q", absent, res.Text)
				}
			}
			if len(res.Mentions) != len(tc.wantMentions) {
				t.Errorf("Mentions count: got %d want %d\n  got:  %+v\n  want: %+v",
					len(res.Mentions), len(tc.wantMentions), res.Mentions, tc.wantMentions)
				return
			}
			for i, m := range res.Mentions {
				wm := tc.wantMentions[i]
				if m.Source != wm.Source || m.ExternalID != wm.ExternalID {
					t.Errorf("Mention[%d]: got %+v, want %+v", i, m, wm)
				}
			}
		})
	}
}
