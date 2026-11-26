package hooks

import (
	_ "embed"
	"os"
	"path/filepath"
	"strings"
)

// ProviderOmO is the native OmO install target and hook adapter suffix.
const ProviderOmO = "omo"

const omoHookFileName = "agent-mem.ts"

//go:embed assets/omo/agent-mem.ts
var omoHookSource []byte

func installOmOHook(destPath string) (InstallResult, error) {
	return installHookModule(destPath, omoHookSource, []string{
		"session_start", "input", "input_disposition", "tool_result", "agent_end", "agent_settled",
	})
}

func defaultOmOHooksPath() (string, error) {
	// The 5.1.83 branded launcher picks the first nonblank override, without
	// changing project extension discovery. Don't run the launcher to install.
	for _, key := range []string{"OMO_CODING_AGENT_DIR", "SENPI_CODING_AGENT_DIR", "PI_CODING_AGENT_DIR"} {
		if dir := strings.TrimSpace(os.Getenv(key)); dir != "" {
			return filepath.Abs(filepath.Join(dir, "extensions", omoHookFileName))
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".omo", "agent", "extensions", omoHookFileName), nil
}
