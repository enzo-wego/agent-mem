package hooks

import (
	"bytes"
	_ "embed"
	"os"
	"path/filepath"
)

// ProviderOMP is the oh-my-pi (omp) install target.
//
// Unlike Codex and Gemini, whose hooks are JSON entries merged into a shared
// config file, omp hooks are code modules auto-discovered from a directory.
// Installing for omp therefore drops an embedded TypeScript hook that routes
// omp's lifecycle events through the same `agent-mem hook <event>` adapter the
// other providers use, rather than merging a hookGroup into a config.
const ProviderOMP = "omp"

// ompHookFileName is the on-disk name of the dropped hook. omp auto-discovers
// every module in its hooks directory, so the file name is not load-bearing;
// keeping it stable makes reinstalls idempotent.
const ompHookFileName = "agent-mem.ts"

//go:embed assets/omp/agent-mem.ts
var ompHookSource []byte

// installOMPHook writes the embedded omp hook to destPath, creating parent
// directories as needed. It is idempotent: an existing file with identical
// content reports no change; differing content is overwritten atomically.
func installOMPHook(destPath string) (InstallResult, error) {
	existing, err := os.ReadFile(destPath)
	switch {
	case err == nil:
		if bytes.Equal(existing, ompHookSource) {
			return InstallResult{Path: destPath, Created: false, Changed: false}, nil
		}
	case !os.IsNotExist(err):
		return InstallResult{}, err
	}
	created := os.IsNotExist(err)

	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return InstallResult{}, err
	}

	tmpPath := destPath + ".tmp"
	if err := os.WriteFile(tmpPath, ompHookSource, 0o644); err != nil {
		return InstallResult{}, err
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		_ = os.Remove(tmpPath)
		return InstallResult{}, err
	}

	return InstallResult{
		Path:          destPath,
		Created:       created,
		Changed:       true,
		ChangedEvents: ompHookEvents(),
	}, nil
}

// ompHookEvents lists the omp events the dropped hook subscribes to, in the
// order they fire, mirroring the Claude/Codex/Gemini event ordering for
// consistent install output.
func ompHookEvents() []string {
	return []string{"session_start", "before_agent_start", "tool_result", "agent_end"}
}
