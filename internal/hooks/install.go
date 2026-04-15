package hooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

type hookConfigFile struct {
	Hooks map[string][]hookGroup `json:"hooks"`
}

type hookGroup struct {
	Matcher string        `json:"matcher,omitempty"`
	Hooks   []commandHook `json:"hooks"`
}

type commandHook struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

type InstallResult struct {
	Path          string
	Created       bool
	Changed       bool
	ChangedEvents []string
}

type InstallOptions struct {
	HooksPath  string
	ProjectDir string
	Scope      string
}

var codexHookEventOrder = []string{
	"SessionStart",
	"UserPromptSubmit",
	"PostToolUse",
	"Stop",
}

var codexHookCommands = map[string]string{
	"SessionStart":     "agent-mem hook session-start codex",
	"UserPromptSubmit": "agent-mem hook prompt-submit codex",
	"PostToolUse":      "agent-mem hook post-tool-use codex",
	"Stop":             "agent-mem hook stop codex",
}

// InstallHooks merges the managed agent-mem hooks into the selected provider's
// config file, creating the file if it does not exist yet.
func InstallHooks(provider, hooksPath string) (InstallResult, error) {
	resolvedProvider, err := normalizeInstallProvider(provider)
	if err != nil {
		return InstallResult{}, err
	}

	if hooksPath == "" {
		hooksPath, err = defaultHooksPath(resolvedProvider)
		if err != nil {
			return InstallResult{}, err
		}
	}

	cfg, created, err := loadHookConfig(hooksPath)
	if err != nil {
		return InstallResult{}, err
	}

	changedEvents := mergeCodexHooks(cfg)
	changed := created || len(changedEvents) > 0
	if changed {
		if err := writeHookConfig(hooksPath, cfg); err != nil {
			return InstallResult{}, err
		}
	}

	return InstallResult{
		Path:          hooksPath,
		Created:       created,
		Changed:       changed,
		ChangedEvents: changedEvents,
	}, nil
}

// InstallHooksWithOptions resolves the target hooks file from scope/path
// options and installs the managed agent-mem hooks there.
func InstallHooksWithOptions(provider string, opts InstallOptions) (InstallResult, error) {
	hooksPath, err := resolveInstallPath(provider, opts)
	if err != nil {
		return InstallResult{}, err
	}
	return InstallHooks(provider, hooksPath)
}

func normalizeInstallProvider(provider string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "", ProviderCodex:
		return ProviderCodex, nil
	default:
		return "", fmt.Errorf("install-hooks currently supports %q only", ProviderCodex)
	}
}

func resolveInstallPath(provider string, opts InstallOptions) (string, error) {
	resolvedProvider, err := normalizeInstallProvider(provider)
	if err != nil {
		return "", err
	}

	if strings.TrimSpace(opts.HooksPath) != "" {
		return opts.HooksPath, nil
	}

	switch normalizeInstallScope(opts.Scope) {
	case "", "user":
		return defaultHooksPath(resolvedProvider)
	case "project":
		return projectHooksPath(opts.ProjectDir)
	default:
		return "", fmt.Errorf("unsupported install scope %q", opts.Scope)
	}
}

func normalizeInstallScope(scope string) string {
	return strings.ToLower(strings.TrimSpace(scope))
}

func defaultHooksPath(provider string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	switch provider {
	case ProviderCodex:
		return filepath.Join(home, ".codex", "hooks.json"), nil
	default:
		return "", fmt.Errorf("no default hooks path for provider %q", provider)
	}
}

func projectHooksPath(projectDir string) (string, error) {
	base := strings.TrimSpace(projectDir)
	if base == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		base = cwd
	}
	return filepath.Join(base, ".codex", "hooks.json"), nil
}

func loadHookConfig(path string) (*hookConfigFile, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &hookConfigFile{Hooks: make(map[string][]hookGroup)}, true, nil
		}
		return nil, false, err
	}

	var cfg hookConfigFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, false, fmt.Errorf("parse hooks config %s: %w", path, err)
	}
	if cfg.Hooks == nil {
		cfg.Hooks = make(map[string][]hookGroup)
	}
	return &cfg, false, nil
}

func mergeCodexHooks(cfg *hookConfigFile) []string {
	if cfg.Hooks == nil {
		cfg.Hooks = make(map[string][]hookGroup)
	}

	changedEvents := make([]string, 0, len(codexHookEventOrder))
	for _, event := range codexHookEventOrder {
		command := codexHookCommands[event]
		desired := desiredCodexHookGroup(event, command)
		next, changed := mergeManagedHookGroup(cfg.Hooks[event], command, desired)
		cfg.Hooks[event] = next
		if changed {
			changedEvents = append(changedEvents, event)
		}
	}
	return changedEvents
}

func desiredCodexHookGroup(event, command string) hookGroup {
	group := hookGroup{
		Hooks: []commandHook{
			{
				Type:    "command",
				Command: command,
				Timeout: desiredTimeout(event),
			},
		},
	}
	if event == "PostToolUse" {
		group.Matcher = "*"
	}
	return group
}

func desiredTimeout(event string) int {
	switch event {
	case "SessionStart", "Stop":
		return 30
	default:
		return 10
	}
}

func mergeManagedHookGroup(groups []hookGroup, managedCommand string, desired hookGroup) ([]hookGroup, bool) {
	next := make([]hookGroup, 0, len(groups)+1)
	foundManaged := false
	changed := false

	for _, group := range groups {
		if !isManagedAgentMemGroup(group, managedCommand) {
			next = append(next, group)
			continue
		}

		if foundManaged {
			changed = true
			continue
		}

		foundManaged = true
		if !reflect.DeepEqual(group, desired) {
			changed = true
		}
		next = append(next, desired)
	}

	if !foundManaged {
		next = append(next, desired)
		changed = true
	}

	return next, changed
}

func isManagedAgentMemGroup(group hookGroup, managedCommand string) bool {
	if len(group.Hooks) == 0 {
		return false
	}
	for _, hook := range group.Hooks {
		if strings.TrimSpace(hook.Command) != managedCommand {
			return false
		}
	}
	return true
}

func writeHookConfig(path string, cfg *hookConfigFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}
