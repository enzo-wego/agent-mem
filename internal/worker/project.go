package worker

import (
	"path/filepath"
	"strings"

	"github.com/rs/zerolog/log"
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
		if contains(allowed, project) {
			return true
		}
		s.logFilteredOnce(project)
		return false
	}

	if s.config.IgnoredProjects != "" {
		ignored := splitList(s.config.IgnoredProjects)
		if contains(ignored, project) {
			s.logFilteredOnce(project)
			return false
		}
		return true
	}

	return true
}

// logFilteredOnce logs a rejected project at info level, once per Server.
func (s *Server) logFilteredOnce(project string) {
	if _, seen := s.filteredProjects.LoadOrStore(project, struct{}{}); !seen {
		log.Info().Str("project", project).
			Msg("Project not recorded: not allowed by allowed_projects/ignored_projects")
	}
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
