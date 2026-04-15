package codexinstall

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agent-mem/agent-mem/internal/hooks"
	"github.com/agent-mem/agent-mem/internal/skills"
	pluginassets "github.com/agent-mem/agent-mem/plugin"
)

const ProviderCodex = "codex"

type InstallOptions struct {
	Scope           string
	ProjectDir      string
	HooksPath       string
	PluginSkillsDir string
}

type InstallResult struct {
	Hooks  hooks.InstallResult
	Skills []skills.InstallResult
}

func Install(opts InstallOptions) (InstallResult, error) {
	skillNames, err := discoverPluginSkills(opts.PluginSkillsDir)
	if err != nil {
		return InstallResult{}, err
	}

	hookResult, err := hooks.InstallHooksWithOptions(ProviderCodex, hooks.InstallOptions{
		Scope:      defaultScope(opts.Scope),
		ProjectDir: opts.ProjectDir,
		HooksPath:  opts.HooksPath,
	})
	if err != nil {
		return InstallResult{}, err
	}

	skillResults := make([]skills.InstallResult, 0, len(skillNames))
	for _, name := range skillNames {
		result, err := installSkill(name, opts)
		if err != nil {
			return InstallResult{}, err
		}
		skillResults = append(skillResults, result)
	}

	return InstallResult{
		Hooks:  hookResult,
		Skills: skillResults,
	}, nil
}

func defaultScope(scope string) string {
	trimmed := strings.TrimSpace(scope)
	if trimmed == "" {
		return skills.ScopeProject
	}
	return trimmed
}

func discoverPluginSkills(pluginSkillsDir string) ([]string, error) {
	if strings.TrimSpace(pluginSkillsDir) != "" {
		return discoverPluginSkillsFromDir(pluginSkillsDir)
	}
	return discoverEmbeddedPluginSkills()
}

func installSkill(name string, opts InstallOptions) (skills.InstallResult, error) {
	if strings.TrimSpace(opts.PluginSkillsDir) != "" {
		sourceDir := filepath.Join(opts.PluginSkillsDir, name)
		return skills.Install(name, skills.InstallOptions{
			Scope:      defaultScope(opts.Scope),
			ProjectDir: opts.ProjectDir,
			SourceDir:  sourceDir,
		})
	}

	return skills.InstallFromFS(name, pluginassets.SkillFS, filepath.ToSlash(filepath.Join("skills", name)), skills.InstallOptions{
		Scope:      defaultScope(opts.Scope),
		ProjectDir: opts.ProjectDir,
	})
}

func discoverPluginSkillsFromDir(baseDir string) ([]string, error) {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("plugin skills directory %s does not exist", baseDir)
		}
		return nil, err
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		skillFile := filepath.Join(baseDir, entry.Name(), "SKILL.md")
		if _, err := os.Stat(skillFile); err == nil {
			names = append(names, entry.Name())
		}
	}

	if len(names) == 0 {
		return nil, fmt.Errorf("no plugin skills found in %s", baseDir)
	}

	sort.Strings(names)
	return names, nil
}

func discoverEmbeddedPluginSkills() ([]string, error) {
	entries, err := fs.ReadDir(pluginassets.SkillFS, "skills")
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillFile := filepath.ToSlash(filepath.Join("skills", entry.Name(), "SKILL.md"))
		if _, err := fs.Stat(pluginassets.SkillFS, skillFile); err == nil {
			names = append(names, entry.Name())
		}
	}

	if len(names) == 0 {
		return nil, fmt.Errorf("no embedded plugin skills found")
	}

	sort.Strings(names)
	return names, nil
}
