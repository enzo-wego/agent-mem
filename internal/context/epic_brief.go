package context

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/config"
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
// on a branch naming a business-root issue (PAY-1234), resolve its epic and
// return that epic's standing brief as a markdown block. The local DB is
// tried first (graph.jira_epic_map ⋈ graph.epic_briefs); when it has nothing
// and a sync URL is configured, the hub is asked (epicBriefFromHub), since a
// laptop's tables are not synced. "" whenever any step has nothing; never an
// error to the hook.
func epicBriefForCwd(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config, cwd string) string {
	if cwd == "" {
		return ""
	}
	branch := gitBranch(cwd)
	if branch == "" {
		return ""
	}
	project := defaultBusinessProject
	if pool != nil {
		var v string
		if err := pool.QueryRow(ctx, `SELECT value FROM settings WHERE key='graph.business_root_project'`).Scan(&v); err == nil {
			if v = strings.ToUpper(strings.TrimSpace(v)); v != "" {
				project = v
			}
		}
	}
	issue := issueKeyInBranch(branch, project)
	if issue == "" {
		return ""
	}
	if pool != nil {
		var epicKey, epicSummary, brief string
		err := pool.QueryRow(ctx, `
SELECT em.epic_key, em.epic_summary, b.brief
FROM graph.jira_epic_map em
JOIN graph.epic_briefs b ON b.epic_key = em.epic_key
WHERE em.issue_key = $1 AND em.epic_key <> '' AND b.brief <> ''`, issue).Scan(&epicKey, &epicSummary, &brief)
		if err == nil {
			return renderEpicBrief(branch, issue, epicKey, epicSummary, brief)
		}
	}
	if cfg == nil || strings.TrimSpace(cfg.SyncURL) == "" {
		return ""
	}
	epicKey, epicSummary, brief := epicBriefFromHub(ctx, cfg, issue)
	if brief == "" {
		return ""
	}
	return renderEpicBrief(branch, issue, epicKey, epicSummary, brief)
}

// hubFallbackTimeout bounds the whole hub lookup (dial, request, body read):
// session start must not stall on a slow hub.
const hubFallbackTimeout = 1500 * time.Millisecond

// epicBriefFromHub asks the hub for the issue's epic: GET
// <SyncURL>/api/graph/epic/<issue> with the machine's API key and no
// X-Asker-User (the machine owner's own session, same trust as the sync
// engine). Any error, timeout, non-200 or empty brief yields "".
func epicBriefFromHub(ctx context.Context, cfg *config.Config, issue string) (epicKey, title, brief string) {
	ctx, cancel := context.WithTimeout(ctx, hubFallbackTimeout)
	defer cancel()
	u := strings.TrimRight(strings.TrimSpace(cfg.SyncURL), "/") + "/api/graph/epic/" + url.PathEscape(issue)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", "", ""
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", ""
	}
	var body struct {
		EpicKey string `json:"epic_key"`
		Title   string `json:"title"`
		Brief   string `json:"brief"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", "", ""
	}
	return body.EpicKey, body.Title, body.Brief
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
