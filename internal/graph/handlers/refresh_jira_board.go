package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// refresh_jira_board fills graph.jira_epic_map: for every PAY issue key already
// referenced in the graph (graph.nodes type='jira'), fetch its parent epic from
// Jira and upsert the mapping. Powers the 📌 PINS panel's board section (threads
// grouped by epic). Self-reschedules every 6h, like detect_hot_topics.
//
// ponytail: board = project PAY (board 193 is the PAY board). Make it a setting
// when a second board matters.
const jiraBoardProject = "PAY"

const refreshJiraBoardInterval = 6 * time.Hour

// NewRefreshJiraBoardHandler returns the job entry for "refresh_jira_board".
func NewRefreshJiraBoardHandler(deps Deps) jobs.Entry {
	return jobs.Entry{
		Handler:  refreshJiraBoardHandler(deps),
		Systems:  []string{"jira"},
		PoolSize: 1,
		Lease:    300 * time.Second,
	}
}

// epicRow is one parsed issue→epic mapping from a Jira search page.
type epicRow struct {
	IssueKey     string
	IssueSummary string
	IssueStatus  string
	EpicKey      string
	EpicSummary  string
	EpicStatus   string
}

// jiraSearchResp is the minimal shape of POST /rest/api/3/search/jql.
type jiraSearchResp struct {
	Issues []struct {
		Key    string `json:"key"`
		Fields struct {
			Summary string `json:"summary"`
			Status  struct {
				Name string `json:"name"`
			} `json:"status"`
			Parent *struct {
				Key    string `json:"key"`
				Fields struct {
					Summary string `json:"summary"`
					Status  struct {
						Name string `json:"name"`
					} `json:"status"`
				} `json:"fields"`
			} `json:"parent"`
		} `json:"fields"`
	} `json:"issues"`
	NextPageToken string `json:"nextPageToken"`
}

// parseJiraEpicRows extracts issue→epic rows and the next page token from a
// Jira search response body.
func parseJiraEpicRows(body []byte) ([]epicRow, string, error) {
	var resp jiraSearchResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, "", fmt.Errorf("decode jira search: %w", err)
	}
	rows := make([]epicRow, 0, len(resp.Issues))
	for _, is := range resp.Issues {
		r := epicRow{
			IssueKey:     is.Key,
			IssueSummary: is.Fields.Summary,
			IssueStatus:  is.Fields.Status.Name,
		}
		if p := is.Fields.Parent; p != nil {
			r.EpicKey = p.Key
			r.EpicSummary = p.Fields.Summary
			r.EpicStatus = p.Fields.Status.Name
		}
		rows = append(rows, r)
	}
	return rows, resp.NextPageToken, nil
}

func refreshJiraBoardHandler(deps Deps) jobs.Handler {
	return func(ctx context.Context, _ []byte) error {
		// Always reschedule the next tick, even on failure — same contract as
		// detect_hot_topics: one bad Jira day must not kill the loop.
		defer func() {
			if _, err := jobs.Enqueue(ctx, deps.DB, "refresh_jira_board", map[string]any{},
				jobs.EnqueueOptions{
					AvailableAt:  time.Now().Add(refreshJiraBoardInterval),
					TargetRunner: deps.Runner,
					MachineID:    deps.MachineID,
				}); err != nil {
				deps.Logger.Warn().Err(err).Msg("refresh_jira_board: reschedule failed")
			}
		}()

		baseURL := strings.TrimRight(os.Getenv("AGENT_MEM_JIRA_BASE_URL"), "/")
		email := os.Getenv("AGENT_MEM_JIRA_EMAIL")
		token := os.Getenv("AGENT_MEM_JIRA_TOKEN")
		if baseURL == "" || email == "" || token == "" {
			deps.Logger.Info().Msg("refresh_jira_board: jira creds not configured; skipping")
			return nil
		}

		// The graph is the source of which issues matter: only keys some Slack
		// thread (or other artifact) actually references.
		krows, err := deps.DB.Query(ctx,
			`SELECT DISTINCT natural_key FROM graph.nodes
			 WHERE type='jira' AND natural_key LIKE $1 AND deleted_at IS NULL`,
			jiraBoardProject+"-%")
		if err != nil {
			return fmt.Errorf("load jira keys: %w", err)
		}
		var keys []string
		for krows.Next() {
			var k string
			if krows.Scan(&k) == nil && k != "" {
				keys = append(keys, k)
			}
		}
		krows.Close()
		if len(keys) == 0 {
			return nil
		}

		client := &http.Client{Timeout: 30 * time.Second}
		total := 0
		// Batched JQL: `key in (…)` in chunks — one HTTP call per ~50 issues.
		for start := 0; start < len(keys); start += 50 {
			end := start + 50
			if end > len(keys) {
				end = len(keys)
			}
			jql := "key in (" + strings.Join(keys[start:end], ",") + ")"
			pageToken := ""
			for {
				rows, next, err := jiraSearchPage(ctx, client, baseURL, email, token, jql, pageToken)
				if err != nil {
					return fmt.Errorf("jira search: %w", err)
				}
				for _, r := range rows {
					if _, err := deps.DB.Exec(ctx, `
INSERT INTO graph.jira_epic_map (issue_key, issue_summary, issue_status, epic_key, epic_summary, epic_status, refreshed_at, machine_id)
VALUES ($1,$2,$3,$4,$5,$6,NOW(),$7)
ON CONFLICT (issue_key) DO UPDATE SET
  issue_summary=EXCLUDED.issue_summary, issue_status=EXCLUDED.issue_status,
  epic_key=EXCLUDED.epic_key, epic_summary=EXCLUDED.epic_summary,
  epic_status=EXCLUDED.epic_status, refreshed_at=NOW(), machine_id=EXCLUDED.machine_id`,
						r.IssueKey, r.IssueSummary, r.IssueStatus, r.EpicKey, r.EpicSummary, r.EpicStatus, deps.MachineID); err != nil {
						return fmt.Errorf("upsert %s: %w", r.IssueKey, err)
					}
					total++
				}
				if next == "" {
					break
				}
				pageToken = next
			}
		}
		deps.Logger.Info().Int("issues", total).Msg("refresh_jira_board: epic map refreshed")
		return nil
	}
}

// jiraSearchPage runs one page of POST /rest/api/3/search/jql (the non-deprecated
// Jira Cloud search endpoint; GET /rest/api/3/search was removed by Atlassian).
func jiraSearchPage(ctx context.Context, client *http.Client, baseURL, email, token, jql, pageToken string) ([]epicRow, string, error) {
	payload := map[string]any{
		"jql":        jql,
		"fields":     []string{"summary", "status", "parent"},
		"maxResults": 100,
	}
	if pageToken != "" {
		payload["nextPageToken"] = pageToken
	}
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/rest/api/3/search/jql", strings.NewReader(string(raw)))
	if err != nil {
		return nil, "", err
	}
	req.SetBasicAuth(email, token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("jira search %d: %s", resp.StatusCode, firstLine(string(body), 200))
	}
	return parseJiraEpicRows(body)
}
