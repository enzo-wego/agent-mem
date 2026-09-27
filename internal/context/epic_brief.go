package context

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// defaultBusinessProject mirrors the seed of graph.business_root_project.
const defaultBusinessProject = "PAY"

// gitBranch returns the checked-out branch of the repository at cwd by reading
// .git/HEAD (following a worktree's `gitdir:` file). "" for a detached HEAD,
// a non-repo, or any read error — the caller skips silently.
func gitBranch(cwd string) string {
	gitPath := filepath.Join(cwd, ".git")
	info, err := os.Stat(gitPath)
	if err != nil {
		return ""
	}
	if !info.IsDir() {
		// Worktree / submodule: `.git` is a file "gitdir: <path>".
		raw, err := os.ReadFile(gitPath)
		if err != nil {
			return ""
		}
		dir := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(raw)), "gitdir:"))
		if dir == "" {
			return ""
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(cwd, dir)
		}
		gitPath = dir
	}
	head, err := os.ReadFile(filepath.Join(gitPath, "HEAD"))
	if err != nil {
		return ""
	}
	const prefix = "ref: refs/heads/"
	line := strings.TrimSpace(string(head))
	if !strings.HasPrefix(line, prefix) {
		return ""
	}
	return strings.TrimPrefix(line, prefix)
}

// issueKeyInBranch returns the first "<project>-<n>" key in the branch name
// (case-insensitive, upper-cased), or "".
func issueKeyInBranch(branch, project string) string {
	re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(project) + `-(\d+)\b`)
	m := re.FindStringSubmatch(branch)
	if m == nil {
		return ""
	}
	return strings.ToUpper(project) + "-" + m[1]
}

// epicBriefForCwd is the round-3 session-start port: when the repo at cwd is
// on a branch naming a business-root issue (PAY-1234), resolve its epic via
// graph.jira_epic_map and return that epic's standing brief as a markdown
// block. Pure DB read; "" whenever any step has nothing (no repo, no key, no
// mapping, no brief yet).
func epicBriefForCwd(ctx context.Context, pool *pgxpool.Pool, cwd string) string {
	if pool == nil || cwd == "" {
		return ""
	}
	branch := gitBranch(cwd)
	if branch == "" {
		return ""
	}
	project := defaultBusinessProject
	var v string
	if err := pool.QueryRow(ctx, `SELECT value FROM settings WHERE key='graph.business_root_project'`).Scan(&v); err == nil {
		if v = strings.ToUpper(strings.TrimSpace(v)); v != "" {
			project = v
		}
	}
	issue := issueKeyInBranch(branch, project)
	if issue == "" {
		return ""
	}
	var epicKey, epicSummary, brief string
	err := pool.QueryRow(ctx, `
SELECT em.epic_key, em.epic_summary, b.brief
FROM graph.jira_epic_map em
JOIN graph.epic_briefs b ON b.epic_key = em.epic_key
WHERE em.issue_key = $1 AND em.epic_key <> '' AND b.brief <> ''`, issue).Scan(&epicKey, &epicSummary, &brief)
	if err != nil {
		return ""
	}
	return renderEpicBrief(branch, issue, epicKey, epicSummary, brief)
}

// renderEpicBrief formats the brief block prepended to the session context.
func renderEpicBrief(branch, issue, epicKey, epicSummary, brief string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Epic brief: %s", epicKey)
	if s := strings.TrimSpace(epicSummary); s != "" {
		fmt.Fprintf(&b, " — %s", s)
	}
	fmt.Fprintf(&b, "\n\n_Branch `%s` → %s_\n\n%s\n\n", branch, issue, strings.TrimSpace(brief))
	return b.String()
}
