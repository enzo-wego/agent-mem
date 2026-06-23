package fetchers

import (
	"strings"
	"testing"
)

func TestClaudeArtifactFetcher_Matches(t *testing.T) {
	f := &claudeArtifactFetcher{}
	cases := []struct {
		input string
		want  bool
	}{
		{"claude_artifact:2599ee45-a789-4d1e-9a91-c1e10a651966", true},
		{"https://claude.ai/public/artifacts/2599ee45-a789-4d1e-9a91-c1e10a651966", true},
		{"https://claude.ai/code/artifact/2599ee45-a789-4d1e-9a91-c1e10a651966", true},
		{"wegohub:q4-report", false},
		{"https://example.com/artifacts/abcdefgh", false},
		{"claude_artifact:short", false}, // < 8 chars
		// SSRF attempts: claude.ai appears only as a path/substring, not the host.
		{"https://evil.com/claude.ai/public/artifacts/abcdefgh", false},
		{"https://evil.com/code/artifact/abcdefgh", false},
		{"https://claude.ai.evil.com/public/artifacts/abcdefgh", false},
		{"http://claude.ai/public/artifacts/abcdefgh", false}, // non-https
		{"https://169.254.169.254/code/artifact/abcdefgh", false},
		{"https://claude.ai@evil.com/public/artifacts/abcdefgh", false}, // userinfo trick
	}
	for _, tc := range cases {
		if got := f.Matches(tc.input); got != tc.want {
			t.Errorf("Matches(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestClaudeArtifactFetcher_ParseNode(t *testing.T) {
	const id = "2599ee45-a789-4d1e-9a91-c1e10a651966"
	f := &claudeArtifactFetcher{}

	// Node ID → reconstructs the public-share URL.
	gotID, gotURL, err := f.parseNode("claude_artifact:" + id)
	if err != nil {
		t.Fatalf("parseNode(node id): %v", err)
	}
	if gotID != id || gotURL != "https://claude.ai/public/artifacts/"+id {
		t.Errorf("parseNode(node id) = %q,%q", gotID, gotURL)
	}

	// Full URL → used as-is.
	in := "https://claude.ai/code/artifact/" + id
	gotID, gotURL, err = f.parseNode(in)
	if err != nil {
		t.Fatalf("parseNode(url): %v", err)
	}
	if gotID != id || gotURL != in {
		t.Errorf("parseNode(url) = %q,%q", gotID, gotURL)
	}

	if _, _, err := f.parseNode("not-an-artifact"); err == nil {
		t.Error("expected error for non-artifact input")
	}
}

func TestClaudeArtifactTitleExtraction(t *testing.T) {
	raw := []byte("<html><head><title>Graph Memory — Overview</title></head><body>x</body></html>")
	m := htmlTitleRe.FindSubmatch(raw)
	if m == nil {
		t.Fatal("title regex did not match")
	}
	if got := strings.TrimSpace(string(m[1])); got != "Graph Memory — Overview" {
		t.Errorf("title = %q", got)
	}
}
