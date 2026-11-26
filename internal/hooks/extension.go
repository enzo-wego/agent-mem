package hooks

import (
	"bytes"
	"os"
	"path/filepath"
)

func installHookModule(destPath string, source []byte, events []string) (InstallResult, error) {
	existing, err := os.ReadFile(destPath)
	switch {
	case err == nil:
		if bytes.Equal(existing, source) {
			return InstallResult{Path: destPath}, nil
		}
	case !os.IsNotExist(err):
		return InstallResult{}, err
	}
	created := os.IsNotExist(err)

	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return InstallResult{}, err
	}

	staging, err := os.MkdirTemp(filepath.Dir(destPath), ".agent-mem-hook-*")
	if err != nil {
		return InstallResult{}, err
	}
	defer os.RemoveAll(staging)
	tmpPath := filepath.Join(staging, "module")
	if err := os.WriteFile(tmpPath, source, 0o644); err != nil {
		return InstallResult{}, err
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		return InstallResult{}, err
	}

	return InstallResult{Path: destPath, Created: created, Changed: true, ChangedEvents: events}, nil
}
