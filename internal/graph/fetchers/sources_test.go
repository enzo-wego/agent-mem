package fetchers

import "testing"

func TestCFBase(t *testing.T) {
	tests := []struct {
		name        string
		cfBaseURL   string
		jiraBaseURL string
	}{
		{name: "bare host", cfBaseURL: "https://x"},
		{name: "wiki host", cfBaseURL: "https://x/wiki"},
		{name: "trailing slash", cfBaseURL: "https://x/"},
		{name: "Jira fallback", jiraBaseURL: "https://x"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Registry{cfg: Config{
				CFBaseURL:   tt.cfBaseURL,
				JiraBaseURL: tt.jiraBaseURL,
			}}
			if got := r.cfBase(); got != "https://x/wiki" {
				t.Errorf("cfBase() = %q, want %q", got, "https://x/wiki")
			}
		})
	}
}

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
