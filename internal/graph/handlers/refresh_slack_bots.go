package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// NewRefreshSlackBotsHandler returns a HandlerInfo for the "refresh_slack_bots" job type.
func NewRefreshSlackBotsHandler(deps Deps) jobs.Entry {
	return jobs.Entry{
		Handler:  refreshSlackBotsHandler(deps),
		Systems:  []string{"slack"},
		PoolSize: 1,
		Lease:    300 * time.Second,
	}
}

// refreshSlackBotsHandler resolves Slack bot_id authors (B…) to their bot names.
//
// Bot ids never appear in users.list (that endpoint is keyed by user ids, U…), so
// refresh_slack_users can't touch them — a bot author lands in graph.people with
// its raw bot_id as the display_name. This job calls bots.info?bot=B… for each
// such person and fills in the real name (e.g. "GitHub", "PagerDuty").
func refreshSlackBotsHandler(deps Deps) jobs.Handler {
	return func(ctx context.Context, _ []byte) error {
		token := os.Getenv("SLACK_BOT_TOKEN")
		if token == "" {
			return fmt.Errorf("%w: refresh_slack_bots: SLACK_BOT_TOKEN not set", jobs.ErrFatal)
		}

		// Bot people whose name is still the raw bot_id. The bot_id lives in
		// identity_map (source "bot" has no dedicated people column), and the
		// "Claude […]" agent sessions share source="bot" but already carry a
		// real name, so match the raw-id shape on both sides to skip them.
		rows, err := deps.DB.Query(ctx, `
			SELECT im.external_id, p.id
			FROM graph.identity_map im
			JOIN graph.people p ON p.id = im.person_id
			WHERE im.source = 'bot'
			  AND im.external_id ~ '^B[A-Z0-9]{6,}$'
			  AND COALESCE(p.display_name, '') ~ '^B[A-Z0-9]{6,}$'`)
		if err != nil {
			return fmt.Errorf("%w: refresh_slack_bots: query bots: %v", jobs.ErrTransient, err)
		}
		type botRow struct {
			BotID    string
			PersonID int64
		}
		var bots []botRow
		for rows.Next() {
			var b botRow
			if e := rows.Scan(&b.BotID, &b.PersonID); e != nil {
				rows.Close()
				return fmt.Errorf("%w: refresh_slack_bots: scan: %v", jobs.ErrTransient, e)
			}
			bots = append(bots, b)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("%w: refresh_slack_bots: rows: %v", jobs.ErrTransient, err)
		}

		resolved := 0
		for _, b := range bots {
			name, e := fetchSlackBotName(ctx, token, b.BotID)
			if e != nil {
				deps.Logger.Warn().Err(e).Str("bot", b.BotID).Msg("refresh_slack_bots: bots.info failed")
				continue
			}
			if name == "" || name == b.BotID {
				continue
			}
			if _, e := deps.DB.Exec(ctx,
				`UPDATE graph.people SET display_name = $2 WHERE id = $1`,
				b.PersonID, name,
			); e != nil {
				deps.Logger.Warn().Err(e).Str("bot", b.BotID).Msg("refresh_slack_bots: update people failed")
				continue
			}
			resolved++
		}

		deps.Logger.Info().Int("candidates", len(bots)).Int("resolved", resolved).Msg("refresh_slack_bots: done")
		return nil
	}
}

// fetchSlackBotName calls bots.info?bot=B… and returns the bot's display name.
// Empty name means the API had no name for it (caller keeps the raw id).
func fetchSlackBotName(ctx context.Context, token, botID string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	u := "https://slack.com/api/bots.info?bot=" + url.QueryEscape(botID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("%w: build request: %v", jobs.ErrFatal, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: http: %v", jobs.ErrTransient, err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		return "", fmt.Errorf("%w: read: %v", jobs.ErrTransient, readErr)
	}
	if resp.StatusCode == 429 || resp.StatusCode >= 500 {
		return "", fmt.Errorf("%w: HTTP %d", jobs.ErrTransient, resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("%w: HTTP %d", jobs.ErrFatal, resp.StatusCode)
	}

	var apiResp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Bot   struct {
			Name string `json:"name"`
		} `json:"bot"`
	}
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return "", fmt.Errorf("%w: parse: %v", jobs.ErrTransient, err)
	}
	if !apiResp.OK {
		// bot_not_found is permanent for a stale id; treat as "no name", not an error.
		if apiResp.Error == "bot_not_found" {
			return "", nil
		}
		return "", fmt.Errorf("%w: slack API error: %s", jobs.ErrTransient, apiResp.Error)
	}
	return strings.TrimSpace(apiResp.Bot.Name), nil
}
