package codexinstall

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDiscoverPluginSkillsFindsSortedSkillDirs(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	mustWriteFile(t, filepath.Join(baseDir, "zeta", "SKILL.md"), []byte("zeta"))
	mustWriteFile(t, filepath.Join(baseDir, "alpha", "SKILL.md"), []byte("alpha"))
	mustWriteFile(t, filepath.Join(baseDir, "notes.txt"), []byte("ignore"))

	got, err := discoverPluginSkills(baseDir)
	if err != nil {
		t.Fatalf("discoverPluginSkills returned error: %v", err)
	}

	want := []string{"alpha", "zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("skills = %#v, want %#v", got, want)
	}
}

func TestInstallProjectScopeInstallsHooksAndAllPluginSkills(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	pluginSkillsDir := filepath.Join(projectDir, "plugin", "skills")
	mustWriteFile(t, filepath.Join(pluginSkillsDir, "mem-search", "SKILL.md"), []byte("mem-search"))
	mustWriteFile(t, filepath.Join(pluginSkillsDir, "other-skill", "SKILL.md"), []byte("other"))

	result, err := Install(InstallOptions{
		Scope:           skillsScopeProject(),
		ProjectDir:      projectDir,
		PluginSkillsDir: pluginSkillsDir,
	})
	if err != nil {
		t.Fatalf("Install returned error: %v", err)
	}

	if result.Hooks.Path != filepath.Join(projectDir, ".codex", "hooks.json") {
		t.Fatalf("hooks path = %q", result.Hooks.Path)
	}
	if len(result.Skills) != 2 {
		t.Fatalf("skills len = %d, want 2", len(result.Skills))
	}

	assertFileExists(t, filepath.Join(projectDir, ".codex", "skills", "mem-search", "SKILL.md"))
	assertFileExists(t, filepath.Join(projectDir, ".codex", "skills", "other-skill", "SKILL.md"))
}

func TestInstallRejectsMissingPluginSkills(t *testing.T) {
	t.Parallel()

	_, err := Install(InstallOptions{
		Scope:           skillsScopeProject(),
		ProjectDir:      t.TempDir(),
		PluginSkillsDir: filepath.Join(t.TempDir(), "missing"),
	})
	if err == nil {
		t.Fatal("expected missing plugin skills error")
	}
}

func TestDiscoverPluginSkillsUsesEmbeddedSkillsByDefault(t *testing.T) {
	t.Parallel()

	got, err := discoverPluginSkills("")
	if err != nil {
		t.Fatalf("discoverPluginSkills returned error: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("expected at least one embedded skill")
	}
	if got[0] != "mem-search" {
		t.Fatalf("first embedded skill = %q, want %q", got[0], "mem-search")
	}
}

func skillsScopeProject() string {
	return "project"
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

func assertFileExists(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
}
