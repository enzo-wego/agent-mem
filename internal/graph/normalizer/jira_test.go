package normalizer

import (
	"context"
	"strings"
	"testing"
)

func TestJiraNormalizer(t *testing.T) {
	ctx := context.Background()
	n := NewJiraNormalizer()

	if n.Source() != "jira" {
		t.Fatalf("Source() = %q, want \"jira\"", n.Source())
	}

	tests := []struct {
		name         string
		input        string
		wantContains []string
		wantMentions []Mention
	}{
		{
			name:         "empty",
			input:        "",
			wantContains: []string{},
		},
		{
			name: "PAY-2128 real fixture",
			input: `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"Tabby authorizations failing due to missing installments_count in API response"}]}]}`,
			wantContains: []string{"Tabby authorizations failing"},
		},
		{
			name: "simple paragraph with bold and link",
			input: `{"type":"doc","content":[{"type":"paragraph","content":[
				{"type":"text","text":"Hello ","marks":[]},
				{"type":"text","text":"world","marks":[{"type":"strong"}]},
				{"type":"text","text":" see ","marks":[]},
				{"type":"text","text":"docs","marks":[{"type":"link","attrs":{"href":"https://example.com"}}]}
			]}]}`,
			wantContains: []string{"Hello world", "docs (https://example.com)"},
		},
		{
			name: "codeBlock",
			input: `{"type":"doc","content":[{"type":"codeBlock","content":[{"type":"text","text":"fmt.Println(\"hi\")"}]}]}`,
			wantContains: []string{"```", "fmt.Println"},
		},
		{
			name: "mention",
			input: `{"type":"doc","content":[{"type":"paragraph","content":[
				{"type":"mention","attrs":{"id":"5e7b8c1a2d3f4e5","text":"Jane Doe"}}
			]}]}`,
			wantContains: []string{"@Jane Doe"},
			wantMentions: []Mention{
				{Source: "jira", ExternalID: "5e7b8c1a2d3f4e5", DisplayName: "Jane Doe"},
			},
		},
		{
			name:         "malformed JSON falls back to raw",
			input:        "not json at all",
			wantContains: []string{"not json at all"},
		},
		{
			name: "bullet list",
			input: `{"type":"doc","content":[{"type":"bulletList","content":[
				{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"item one"}]}]},
				{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"item two"}]}]}
			]}]}`,
			wantContains: []string{"- item one", "- item two"},
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
			if len(res.Mentions) != len(tc.wantMentions) {
				t.Errorf("Mentions count: got %d, want %d", len(res.Mentions), len(tc.wantMentions))
				return
			}
			for i, m := range res.Mentions {
				wm := tc.wantMentions[i]
				if m.Source != wm.Source || m.ExternalID != wm.ExternalID || m.DisplayName != wm.DisplayName {
					t.Errorf("Mention[%d]: got %+v, want %+v", i, m, wm)
				}
			}
		})
	}
}
