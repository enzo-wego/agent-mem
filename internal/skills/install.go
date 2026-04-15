package skills

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	ScopeUser    = "user"
	ScopeProject = "project"
)

type InstallOptions struct {
	SourceDir  string
	ProjectDir string
	Scope      string
}

type InstallResult struct {
	Name    string
	Source  string
	Target  string
	Changed bool
}

func Install(name string, opts InstallOptions) (InstallResult, error) {
	normalizedName, err := normalizeName(name)
	if err != nil {
		return InstallResult{}, err
	}

	sourceDir, err := resolveSourceDir(normalizedName, opts.SourceDir)
	if err != nil {
		return InstallResult{}, err
	}
	targetDir, err := resolveTargetDir(normalizedName, opts.Scope, opts.ProjectDir)
	if err != nil {
		return InstallResult{}, err
	}

	if err := validateSkillSource(sourceDir); err != nil {
		return InstallResult{}, err
	}

	if err := copyDir(sourceDir, targetDir); err != nil {
		return InstallResult{}, err
	}

	return InstallResult{
		Name:    normalizedName,
		Source:  sourceDir,
		Target:  targetDir,
		Changed: true,
	}, nil
}

func InstallFromFS(name string, sourceFS fs.FS, sourceDir string, opts InstallOptions) (InstallResult, error) {
	normalizedName, err := normalizeName(name)
	if err != nil {
		return InstallResult{}, err
	}

	if strings.TrimSpace(sourceDir) == "" {
		return InstallResult{}, fmt.Errorf("sourceDir is required for embedded skill install")
	}

	targetDir, err := resolveTargetDir(normalizedName, opts.Scope, opts.ProjectDir)
	if err != nil {
		return InstallResult{}, err
	}

	if err := validateSkillFS(sourceFS, sourceDir); err != nil {
		return InstallResult{}, err
	}

	if err := copyDirFromFS(sourceFS, sourceDir, targetDir); err != nil {
		return InstallResult{}, err
	}

	return InstallResult{
		Name:    normalizedName,
		Source:  sourceDir,
		Target:  targetDir,
		Changed: true,
	}, nil
}

func normalizeName(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", fmt.Errorf("skill name is required")
	}
	return trimmed, nil
}

func normalizeScope(scope string) string {
	return strings.ToLower(strings.TrimSpace(scope))
}

func resolveSourceDir(name, sourceDir string) (string, error) {
	if strings.TrimSpace(sourceDir) != "" {
		return sourceDir, nil
	}

	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(cwd, "plugin", "skills", name), nil
}

func resolveTargetDir(name, scope, projectDir string) (string, error) {
	switch normalizeScope(scope) {
	case "", ScopeUser:
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".codex", "skills", name), nil
	case ScopeProject:
		base := strings.TrimSpace(projectDir)
		if base == "" {
			cwd, err := os.Getwd()
			if err != nil {
				return "", err
			}
			base = cwd
		}
		return filepath.Join(base, ".codex", "skills", name), nil
	default:
		return "", fmt.Errorf("unsupported install scope %q", scope)
	}
}

func validateSkillSource(sourceDir string) error {
	info, err := os.Stat(sourceDir)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("skill source %s does not exist", sourceDir)
		}
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("skill source %s is not a directory", sourceDir)
	}
	skillFile := filepath.Join(sourceDir, "SKILL.md")
	if _, err := os.Stat(skillFile); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("skill source %s is missing SKILL.md", sourceDir)
		}
		return err
	}
	return nil
}

func validateSkillFS(sourceFS fs.FS, sourceDir string) error {
	info, err := fs.Stat(sourceFS, sourceDir)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("skill source %s does not exist", sourceDir)
		}
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("skill source %s is not a directory", sourceDir)
	}
	skillFile := filepath.ToSlash(filepath.Join(sourceDir, "SKILL.md"))
	if _, err := fs.Stat(sourceFS, skillFile); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("skill source %s is missing SKILL.md", sourceDir)
		}
		return err
	}
	return nil
}

func copyDir(sourceDir, targetDir string) error {
	if err := os.RemoveAll(targetDir); err != nil {
		return err
	}

	return filepath.Walk(sourceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}
		destPath := filepath.Join(targetDir, relPath)

		if info.IsDir() {
			return os.MkdirAll(destPath, 0o755)
		}

		return copyFile(path, destPath, info.Mode())
	})
}

func copyDirFromFS(sourceFS fs.FS, sourceDir, targetDir string) error {
	if err := os.RemoveAll(targetDir); err != nil {
		return err
	}

	return fs.WalkDir(sourceFS, sourceDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}
		destPath := filepath.Join(targetDir, relPath)

		if entry.IsDir() {
			return os.MkdirAll(destPath, 0o755)
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}
		return copyFileFromFS(sourceFS, path, destPath, info.Mode())
	})
}

func copyFile(sourcePath, targetPath string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return err
	}

	in, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(targetPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, normalizedFilePerm(mode))
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}

	return out.Close()
}

func copyFileFromFS(sourceFS fs.FS, sourcePath, targetPath string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return err
	}

	in, err := sourceFS.Open(sourcePath)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(targetPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, normalizedFilePerm(mode))
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}

	return out.Close()
}

func normalizedFilePerm(mode os.FileMode) os.FileMode {
	if perm := mode.Perm(); perm != 0 {
		return perm
	}
	return 0o644
}
