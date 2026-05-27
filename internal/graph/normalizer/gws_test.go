package normalizer

import (
	"context"
	"strings"
	"testing"
)

func TestGWSNormalizer(t *testing.T) {
	ctx := context.Background()
	n := NewGWSNormalizer()

	if n.Source() != "gws" {
		t.Fatalf("Source() = %q, want \"gws\"", n.Source())
	}

	tests := []struct {
		name         string
		input        string
		wantContains []string
		wantAbsent   []string
		wantMentions []Mention
	}{
		{
			name:  "empty",
			input: "",
		},
		{
			name: "docs JSON body",
			input: `{
				"title": "My Document",
				"body": {
					"content": [
						{
							"paragraph": {
								"elements": [
									{"textRun": {"content": "Hello world\n"}}
								]
							}
						},
						{
							"paragraph": {
								"elements": [
									{"textRun": {"content": "Second paragraph\n"}}
								]
							}
						}
					]
				}
			}`,
			wantContains: []string{"Hello world", "Second paragraph"},
		},
		{
			name: "raw HTML export",
			input: `<html><body><h1>Title</h1><p>Some content here.</p></body></html>`,
			wantContains: []string{"Title", "Some content here"},
			wantAbsent:   []string{"<h1>", "<p>"},
		},
		{
			name: "empty docs body",
			input: `{
				"title": "Empty",
				"body": {
					"content": []
				}
			}`,
			wantContains: []string{},
		},
		{
			name: "docs JSON with person mention",
			input: `{
				"body": {
					"content": [
						{
							"paragraph": {
								"elements": [
									{
										"textRun": {
											"content": "Review by ",
											"personProperties": {"email": "alice@example.com"}
										}
									}
								]
							}
						}
					]
				}
			}`,
			wantContains: []string{"Review by"},
			wantMentions: []Mention{
				{Source: "gws", ExternalID: "alice@example.com"},
			},
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
