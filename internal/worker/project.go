package worker

import (
	"path/filepath"
	"strings"
)

// extractProject derives the project name from a working directory path.
func extractProject(cwd string) string {
	if cwd == "" {
		return ""
	}
	return filepath.Base(cwd)
}

// isProjectAllowed checks whether a project passes the whitelist/blacklist filter.
// Whitelist takes precedence. If both are empty, all projects are allowed.
func (s *Server) isProjectAllowed(project string) bool {
	if project == "" {
		return false
	}

	if s.config.AllowedProjects != "" {
		allowed := splitList(s.config.AllowedProjects)
		return contains(allowed, project)
	}

	if s.config.IgnoredProjects != "" {
		ignored := splitList(s.config.IgnoredProjects)
		return !contains(ignored, project)
	}

	return true
}

// isToolSkipped checks whether a tool name is in the skip list.
func (s *Server) isToolSkipped(toolName string) bool {
	if toolName == "" || s.config.SkipTools == "" {
		return false
	}
	skipped := splitList(s.config.SkipTools)
	return contains(skipped, toolName)
}

func splitList(s string) []string {
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

func contains(list []string, item string) bool {
	for _, v := range list {
		if strings.EqualFold(v, item) {
			return true
		}
	}
	return false
}
