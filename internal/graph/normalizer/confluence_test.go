package normalizer

import (
	"context"
	"strings"
	"testing"
)

func TestConfluenceNormalizer(t *testing.T) {
	ctx := context.Background()
	n := NewConfluenceNormalizer()

	if n.Source() != "confluence" {
		t.Fatalf("Source() = %q, want \"confluence\"", n.Source())
	}

	tests := []struct {
		name         string
		input        string
		wantContains []string
		wantAbsent   []string
	}{
		{
			name:  "empty",
			input: "",
		},
		{
			name:         "simple paragraph",
			input:        `<p>Hello <strong>world</strong></p>`,
			wantContains: []string{"Hello", "world"},
		},
		{
			name: "unordered list",
			input: `<ul><li>Item one</li><li>Item two</li></ul>`,
			wantContains: []string{"- Item one", "- Item two"},
		},
		{
			name: "code macro",
			input: `<ac:structured-macro ac:name="code"><ac:plain-text-body><![CDATA[fmt.Println("hi")]]></ac:plain-text-body></ac:structured-macro>`,
			wantContains: []string{"```"},
		},
		{
			name:         "malformed XML fallback",
			input:        `<p>Hello <b>unclosed`,
			wantContains: []string{"Hello"},
		},
		{
			name:         "style tag content stripped",
			input:        `<p>visible</p><style>body { color: red; }</style>`,
			wantContains: []string{"visible"},
			wantAbsent:   []string{"color"},
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
		})
	}
}
