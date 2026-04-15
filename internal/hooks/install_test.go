package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestInstallHooksCreatesCodexConfig(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), ".codex", "hooks.json")

	result, err := InstallHooks(ProviderCodex, path)
	if err != nil {
		t.Fatalf("InstallHooks returned error: %v", err)
	}

	if !result.Created {
		t.Fatal("expected created result")
	}
	if !result.Changed {
		t.Fatal("expected changed result")
	}
	if !reflect.DeepEqual(result.ChangedEvents, codexHookEventOrder) {
		t.Fatalf("changed events = %#v, want %#v", result.ChangedEvents, codexHookEventOrder)
	}

	cfg := readHookConfigForTest(t, path)
	for _, event := range codexHookEventOrder {
		groups := cfg.Hooks[event]
		if len(groups) != 1 {
			t.Fatalf("%s groups len = %d, want 1", event, len(groups))
		}
		if len(groups[0].Hooks) != 1 {
			t.Fatalf("%s hooks len = %d, want 1", event, len(groups[0].Hooks))
		}
		if groups[0].Hooks[0].Command != codexHookCommands[event] {
			t.Fatalf("%s command = %q, want %q", event, groups[0].Hooks[0].Command, codexHookCommands[event])
		}
	}
}

func TestInstallHooksPreservesUnrelatedEntriesAndUpdatesManagedOnes(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "hooks.json")
	initial := hookConfigFile{
		Hooks: map[string][]hookGroup{
			"SessionStart": {
				{
					Matcher: "startup|resume",
					Hooks: []commandHook{
						{Type: "command", Command: "custom session hook"},
					},
				},
				{
					Hooks: []commandHook{
						{Type: "command", Command: "agent-mem hook session-start codex", Timeout: 5},
					},
				},
			},
			"UserPromptSubmit": {
				{
					Hooks: []commandHook{
						{Type: "command", Command: "agent-mem hook prompt-submit codex", Timeout: 10},
					},
				},
			},
			"PostToolUse": {
				{
					Matcher: "Read",
					Hooks: []commandHook{
						{Type: "command", Command: "custom post-tool hook"},
					},
				},
				{
					Matcher: "*",
					Hooks: []commandHook{
						{Type: "command", Command: "agent-mem hook post-tool-use codex", Timeout: 5},
					},
				},
				{
					Matcher: "*",
					Hooks: []commandHook{
						{Type: "command", Command: "agent-mem hook post-tool-use codex", Timeout: 5},
					},
				},
			},
		},
	}
	writeHookConfigForTest(t, path, &initial)

	result, err := InstallHooks(ProviderCodex, path)
	if err != nil {
		t.Fatalf("InstallHooks returned error: %v", err)
	}

	wantChanged := []string{"SessionStart", "PostToolUse", "Stop"}
	if !reflect.DeepEqual(result.ChangedEvents, wantChanged) {
		t.Fatalf("changed events = %#v, want %#v", result.ChangedEvents, wantChanged)
	}

	cfg := readHookConfigForTest(t, path)

	sessionStart := cfg.Hooks["SessionStart"]
	if len(sessionStart) != 2 {
		t.Fatalf("SessionStart groups len = %d, want 2", len(sessionStart))
	}
	if sessionStart[0].Hooks[0].Command != "custom session hook" {
		t.Fatalf("custom SessionStart hook was not preserved: %#v", sessionStart[0])
	}
	if !reflect.DeepEqual(sessionStart[1], desiredCodexHookGroup("SessionStart", codexHookCommands["SessionStart"])) {
		t.Fatalf("SessionStart managed hook = %#v", sessionStart[1])
	}

	postToolUse := cfg.Hooks["PostToolUse"]
	if len(postToolUse) != 2 {
		t.Fatalf("PostToolUse groups len = %d, want 2", len(postToolUse))
	}
	if postToolUse[0].Hooks[0].Command != "custom post-tool hook" {
		t.Fatalf("custom PostToolUse hook was not preserved: %#v", postToolUse[0])
	}
	if !reflect.DeepEqual(postToolUse[1], desiredCodexHookGroup("PostToolUse", codexHookCommands["PostToolUse"])) {
		t.Fatalf("PostToolUse managed hook = %#v", postToolUse[1])
	}

	stop := cfg.Hooks["Stop"]
	if len(stop) != 1 {
		t.Fatalf("Stop groups len = %d, want 1", len(stop))
	}
	if !reflect.DeepEqual(stop[0], desiredCodexHookGroup("Stop", codexHookCommands["Stop"])) {
		t.Fatalf("Stop managed hook = %#v", stop[0])
	}
}

func TestInstallHooksRejectsUnsupportedInstallProvider(t *testing.T) {
	t.Parallel()

	if _, err := InstallHooks(ProviderClaude, filepath.Join(t.TempDir(), "hooks.json")); err == nil {
		t.Fatal("expected unsupported provider error")
	}
}

func TestInstallHooksWithProjectScopeUsesProjectLocalCodexFile(t *testing.T) {
	t.Parallel()

	projectDir := filepath.Join(t.TempDir(), "project")
	result, err := InstallHooksWithOptions(ProviderCodex, InstallOptions{
		Scope:      "project",
		ProjectDir: projectDir,
	})
	if err != nil {
		t.Fatalf("InstallHooksWithOptions returned error: %v", err)
	}

	wantPath := filepath.Join(projectDir, ".codex", "hooks.json")
	if result.Path != wantPath {
		t.Fatalf("result path = %q, want %q", result.Path, wantPath)
	}

	cfg := readHookConfigForTest(t, wantPath)
	if len(cfg.Hooks["Stop"]) != 1 {
		t.Fatalf("Stop groups len = %d, want 1", len(cfg.Hooks["Stop"]))
	}
}

func TestInstallHooksWithOptionsRejectsUnsupportedScope(t *testing.T) {
	t.Parallel()

	if _, err := InstallHooksWithOptions(ProviderCodex, InstallOptions{Scope: "all"}); err == nil {
		t.Fatal("expected unsupported scope error")
	}
}

func TestInstallHooksWithOptionsHonorsExplicitHooksFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "custom-hooks.json")
	result, err := InstallHooksWithOptions(ProviderCodex, InstallOptions{
		Scope:     "project",
		HooksPath: path,
	})
	if err != nil {
		t.Fatalf("InstallHooksWithOptions returned error: %v", err)
	}
	if result.Path != path {
		t.Fatalf("result path = %q, want %q", result.Path, path)
	}
}

func readHookConfigForTest(t *testing.T, path string) hookConfigFile {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}

	var cfg hookConfigFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return cfg
}

func writeHookConfigForTest(t *testing.T, path string, cfg *hookConfigFile) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent: %v", err)
	}
	data = append(data, '\n')

	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}
