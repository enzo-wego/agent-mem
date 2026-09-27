package context

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIssueKeyInBranch(t *testing.T) {
	cases := map[string]string{
		"feat/PAY-2307-gst-invoice": "PAY-2307",
		"pay-12":                    "PAY-12",
		"PAYX-12":                   "",
		"main":                      "",
		"fix/OPS-4-PAY-9":           "PAY-9",
	}
	for branch, want := range cases {
		if got := issueKeyInBranch(branch, "PAY"); got != want {
			t.Errorf("issueKeyInBranch(%q) = %q, want %q", branch, got, want)
		}
	}
}

func TestGitBranch(t *testing.T) {
	dir := t.TempDir()
	if gitBranch(dir) != "" {
		t.Fatal("non-repo must yield no branch")
	}
	gitDir := filepath.Join(dir, ".git")
	if err := os.Mkdir(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/feat/PAY-1-x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := gitBranch(dir); got != "feat/PAY-1-x" {
		t.Fatalf("gitBranch = %q", got)
	}
	// Worktree: .git is a file pointing at the real gitdir.
	wt := t.TempDir()
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := gitBranch(wt); got != "feat/PAY-1-x" {
		t.Fatalf("worktree gitBranch = %q", got)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("0123abcd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if gitBranch(dir) != "" {
		t.Fatal("detached HEAD must yield no branch")
	}
}
