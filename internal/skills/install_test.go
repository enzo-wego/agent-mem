package skills

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestInstallProjectScopeCopiesEntireSkillDir(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	sourceDir := filepath.Join(projectDir, "plugin", "skills", "mem-search")
	mustWriteFile(t, filepath.Join(sourceDir, "SKILL.md"), []byte("name: mem-search\n"))
	mustWriteFile(t, filepath.Join(sourceDir, "refs", "example.txt"), []byte("example"))

	result, err := Install("mem-search", InstallOptions{
		Scope:      ScopeProject,
		SourceDir:  sourceDir,
		ProjectDir: projectDir,
	})
	if err != nil {
		t.Fatalf("Install returned error: %v", err)
	}

	wantTarget := filepath.Join(projectDir, ".codex", "skills", "mem-search")
	if result.Target != wantTarget {
		t.Fatalf("target = %q, want %q", result.Target, wantTarget)
	}

	assertFileContent(t, filepath.Join(wantTarget, "SKILL.md"), "name: mem-search\n")
	assertFileContent(t, filepath.Join(wantTarget, "refs", "example.txt"), "example")
}

func TestInstallRejectsMissingSkillSource(t *testing.T) {
	t.Parallel()

	_, err := Install("mem-search", InstallOptions{
		Scope:     ScopeProject,
		SourceDir: filepath.Join(t.TempDir(), "missing"),
	})
	if err == nil {
		t.Fatal("expected missing source error")
	}
}

func TestInstallRejectsUnsupportedScope(t *testing.T) {
	t.Parallel()

	sourceDir := filepath.Join(t.TempDir(), "mem-search")
	mustWriteFile(t, filepath.Join(sourceDir, "SKILL.md"), []byte("name: mem-search\n"))

	_, err := Install("mem-search", InstallOptions{
		Scope:     "all",
		SourceDir: sourceDir,
	})
	if err == nil {
		t.Fatal("expected unsupported scope error")
	}
}

func TestInstallOverwritesExistingTarget(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	sourceDir := filepath.Join(projectDir, "plugin", "skills", "mem-search")
	targetDir := filepath.Join(projectDir, ".codex", "skills", "mem-search")

	mustWriteFile(t, filepath.Join(sourceDir, "SKILL.md"), []byte("new\n"))
	mustWriteFile(t, filepath.Join(targetDir, "SKILL.md"), []byte("old\n"))
	mustWriteFile(t, filepath.Join(targetDir, "stale.txt"), []byte("remove me"))

	if _, err := Install("mem-search", InstallOptions{
		Scope:      ScopeProject,
		SourceDir:  sourceDir,
		ProjectDir: projectDir,
	}); err != nil {
		t.Fatalf("Install returned error: %v", err)
	}

	assertFileContent(t, filepath.Join(targetDir, "SKILL.md"), "new\n")
	if _, err := os.Stat(filepath.Join(targetDir, "stale.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected stale.txt to be removed, got err=%v", err)
	}
}

func TestInstallFromFSProjectScopeCopiesEntireSkillDir(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	sourceFS := fstest.MapFS{
		"skills/mem-search/SKILL.md":         &fstest.MapFile{Data: []byte("mem-search")},
		"skills/mem-search/refs/example.txt": &fstest.MapFile{Data: []byte("example")},
	}

	result, err := InstallFromFS("mem-search", sourceFS, "skills/mem-search", InstallOptions{
		Scope:      ScopeProject,
		ProjectDir: projectDir,
	})
	if err != nil {
		t.Fatalf("InstallFromFS returned error: %v", err)
	}

	wantTarget := filepath.Join(projectDir, ".codex", "skills", "mem-search")
	if result.Target != wantTarget {
		t.Fatalf("target = %q, want %q", result.Target, wantTarget)
	}
	assertFileContent(t, filepath.Join(wantTarget, "SKILL.md"), "mem-search")
	assertFileContent(t, filepath.Join(wantTarget, "refs", "example.txt"), "example")
}

func TestInstallFromFSRejectsMissingSkillFile(t *testing.T) {
	t.Parallel()

	sourceFS := fstest.MapFS{
		"skills/mem-search/README.md": &fstest.MapFile{Data: []byte("no skill")},
	}

	_, err := InstallFromFS("mem-search", sourceFS, "skills/mem-search", InstallOptions{
		Scope: ScopeProject,
	})
	if err == nil {
		t.Fatal("expected missing SKILL.md error")
	}
}

func mustWriteFile(t *testing.T, path string, content []byte) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, string(data), want)
	}
}
