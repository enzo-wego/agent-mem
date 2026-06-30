package fetchers

import "testing"

func TestIsMarkdown(t *testing.T) {
	for _, p := range []string{"README.md", "docs/Design.MARKDOWN", "a/b/c.md"} {
		if !isMarkdown(p) {
			t.Errorf("isMarkdown(%q) = false, want true", p)
		}
	}
	for _, p := range []string{
		"main.go", "notes.txt", "Makefile", "x.mdx",
		".claude/agents/foo.md", "node_modules/pkg/readme.md", "vendor/x/y.md",
		".github/PULL_REQUEST_TEMPLATE.md", "sub/.omc/notes.md",
	} {
		if isMarkdown(p) {
			t.Errorf("isMarkdown(%q) = true, want false", p)
		}
	}
}

func TestCursorFromNext(t *testing.T) {
	got := cursorFromNext("/wiki/api/v2/pages/123/descendants?limit=250&cursor=abc123")
	if got != "abc123" {
		t.Errorf("cursor = %q, want abc123", got)
	}
	if cursorFromNext("") != "" || cursorFromNext("/no/query") != "" {
		t.Errorf("expected empty cursor for empty/no-query links")
	}
}
