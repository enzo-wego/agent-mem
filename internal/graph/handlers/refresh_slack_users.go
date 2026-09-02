package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// NewRefreshSlackUsersHandler returns a HandlerInfo for the "refresh_slack_users" job type.
func NewRefreshSlackUsersHandler(deps Deps) jobs.Entry {
	return jobs.Entry{
		Handler:  refreshSlackUsersHandler(deps),
		Systems:  []string{"slack"},
		PoolSize: 1,
		Lease:    300 * time.Second,
	}
}

func refreshSlackUsersHandler(deps Deps) jobs.Handler {
	return func(ctx context.Context, _ []byte) error {
		token := deps.SlackBotToken
		if token == "" {
			return fmt.Errorf("%w: refresh_slack_users: SLACK_BOT_TOKEN not set", jobs.ErrFatal)
		}

		users, err := fetchSlackUsers(ctx, token)
		if err != nil {
			return err
		}

		emailUsers := 0
		employeeLinks := 0
		upsertAttempts := 0
		upserted := 0
		for _, u := range users {
			name := slackBestName(u)
			if name == "" {
				continue
			}
			upsertAttempts++
			if _, e := deps.DB.Exec(ctx, `
				INSERT INTO graph.slack_users (slack_user_id, display_name, real_name, is_bot, refreshed_at, machine_id)
				VALUES ($1, $2, $3, $4, NOW(), $5)
				ON CONFLICT (slack_user_id) DO UPDATE SET
					display_name = EXCLUDED.display_name,
					real_name    = EXCLUDED.real_name,
					is_bot       = EXCLUDED.is_bot,
					refreshed_at = NOW()`,
				u.ID, name, strings.TrimSpace(u.Profile.RealName), u.IsBot, deps.MachineID,
			); e != nil {
				deps.Logger.Warn().Err(e).Str("uid", u.ID).Msg("refresh_slack_users: upsert slack_users failed")
				continue
			}
			upserted++
			email := strings.ToLower(strings.TrimSpace(u.Profile.Email))
			if email != "" {
				emailUsers++
				linked, linkErr := linkSlackPersonByEmail(ctx, deps, u.ID, email)
				if linkErr != nil {
					return fmt.Errorf("%w: refresh_slack_users: link %s by email: %v",
						jobs.ErrTransient, u.ID, linkErr)
				}
				if linked {
					employeeLinks++
				}
			}
			// Backfill people.display_name (used for author chips / scoring) when blank.
			if _, e := deps.DB.Exec(ctx, `
				UPDATE graph.people SET display_name = $2
				WHERE slack_user_id = $1 AND COALESCE(display_name, '') = ''`,
				u.ID, name,
			); e != nil {
				deps.Logger.Warn().Err(e).Str("uid", u.ID).Msg("refresh_slack_users: update people failed")
			}
			// Fill the person's email (the key that merges them with BambooHR/Jira).
			// Guard against the UNIQUE(email) constraint: only set if free.
			if email != "" {
				if _, e := deps.DB.Exec(ctx, `
					UPDATE graph.people SET email = $2
					WHERE slack_user_id = $1 AND email IS NULL
					  AND NOT EXISTS (SELECT 1 FROM graph.people WHERE email = $2)`,
					u.ID, email,
				); e != nil {
					deps.Logger.Warn().Err(e).Str("uid", u.ID).Msg("refresh_slack_users: update email failed")
				}
			}
		}

		if upsertAttempts > 0 && upserted == 0 {
			return fmt.Errorf("%w: refresh_slack_users: all %d user upserts failed",
				jobs.ErrTransient, upsertAttempts)
		}
		deps.Logger.Info().
			Int("count", len(users)).
			Int("upserted", upserted).
			Int("email_users", emailUsers).
			Int("employee_links", employeeLinks).
			Msg("refresh_slack_users: done")
		return nil
	}
}

// slackUser is the relevant slice of a Slack users.list member object.
type slackUser struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	IsBot   bool   `json:"is_bot"`
	Deleted bool   `json:"deleted"`
	Profile struct {
		DisplayName string `json:"display_name"`
		RealName    string `json:"real_name"`
		Email       string `json:"email"` // requires users:read.email scope; "" otherwise
	} `json:"profile"`
}

// slackBestName prefers profile.display_name, then real_name, then handle.
func slackBestName(u slackUser) string {
	if u.Profile.DisplayName != "" {
		return u.Profile.DisplayName
	}
	if u.Profile.RealName != "" {
		return u.Profile.RealName
	}
	return u.Name
}

// fetchSlackUsers calls users.list with cursor pagination and returns all members.
func fetchSlackUsers(ctx context.Context, token string) ([]slackUser, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	var out []slackUser
	cursor := ""
	for page := 0; page < 50; page++ { // hard cap: 50 pages × 200 = 10k users
		u := "https://slack.com/api/users.list?limit=200"
		if cursor != "" {
			u += "&cursor=" + url.QueryEscape(cursor)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, fmt.Errorf("%w: refresh_slack_users: build request: %v", jobs.ErrFatal, err)
		}
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("%w: refresh_slack_users: http: %v", jobs.ErrTransient, err)
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("%w: refresh_slack_users: read: %v", jobs.ErrTransient, readErr)
		}
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			return nil, fmt.Errorf("%w: refresh_slack_users: HTTP %d", jobs.ErrTransient, resp.StatusCode)
		}
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("%w: refresh_slack_users: HTTP %d", jobs.ErrFatal, resp.StatusCode)
		}

		var apiResp struct {
			OK      bool        `json:"ok"`
			Members []slackUser `json:"members"`
			Error   string      `json:"error"`
			Meta    struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := json.Unmarshal(body, &apiResp); err != nil {
			return nil, fmt.Errorf("%w: refresh_slack_users: parse: %v", jobs.ErrTransient, err)
		}
		if !apiResp.OK {
			return nil, fmt.Errorf("%w: refresh_slack_users: slack API error: %s", jobs.ErrTransient, apiResp.Error)
		}
		out = append(out, apiResp.Members...)
		cursor = apiResp.Meta.NextCursor
		if cursor == "" {
			break
		}
	}
	return out, nil
}

// linkSlackPersonByEmail attaches a Slack identity to the active BambooHR person row that
// owns the exact email. graph.people.email is UNIQUE, so the Slack-side row cannot first
// receive the duplicate email and then be passed to MergeByEmail: the UPDATE is rejected
// before a merge is possible.
//
// If the Slack identity already has a separate person row, that row is folded into the
// EEID row. If it has no person row yet, the Slack id is attached directly. Names are never
// used as identity evidence.
func linkSlackPersonByEmail(ctx context.Context, deps Deps, slackUserID, email string) (bool, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if slackUserID == "" || email == "" {
		return false, nil
	}

	var canonicalID int64
	var canonicalSlackID *string
	err := deps.DB.QueryRow(ctx, `
		SELECT id, slack_user_id
		FROM graph.people
		WHERE email = $1
		  AND eeid IS NOT NULL
		  AND merged_into IS NULL`,
		email,
	).Scan(&canonicalID, &canonicalSlackID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("find employee by email: %w", err)
	}
	if canonicalSlackID != nil && *canonicalSlackID != slackUserID {
		return false, fmt.Errorf("employee person %d already has Slack id %s, refusing %s",
			canonicalID, *canonicalSlackID, slackUserID)
	}

	changed := false
	var slackPersonID int64
	err = deps.DB.QueryRow(ctx, `
		SELECT id
		FROM graph.people
		WHERE slack_user_id = $1
		  AND merged_into IS NULL`,
		slackUserID,
	).Scan(&slackPersonID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		changed = canonicalSlackID == nil
		tag, updateErr := deps.DB.Exec(ctx, `
			UPDATE graph.people
			SET slack_user_id = $2
			WHERE id = $1
			  AND (slack_user_id IS NULL OR slack_user_id = $2)`,
			canonicalID, slackUserID,
		)
		if updateErr != nil {
			return false, fmt.Errorf("attach Slack id: %w", updateErr)
		}
		if tag.RowsAffected() != 1 {
			return false, fmt.Errorf("attach Slack id: employee person %d was not updated", canonicalID)
		}
	case err != nil:
		return false, fmt.Errorf("find Slack person: %w", err)
	case slackPersonID != canonicalID:
		changed = true
		if mergeErr := mergePersonInto(ctx, deps, canonicalID, slackPersonID, ""); mergeErr != nil {
			return false, fmt.Errorf("merge Slack person %d into employee %d: %w",
				slackPersonID, canonicalID, mergeErr)
		}
	}

	if _, err := deps.DB.Exec(ctx, `
		INSERT INTO graph.identity_map (source, external_id, person_id)
		VALUES ('slack', $1, $2)
		ON CONFLICT (source, external_id) DO UPDATE
		SET person_id = EXCLUDED.person_id`,
		slackUserID, canonicalID,
	); err != nil {
		return false, fmt.Errorf("bind Slack identity map: %w", err)
	}
	return changed, nil
}
