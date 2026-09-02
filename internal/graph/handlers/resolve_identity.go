package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// resolveIdentityPayload is the JSON payload for the resolve_identity job type.
type resolveIdentityPayload struct {
	PersonID   int64  `json:"person_id"`
	Source     string `json:"source"`
	ExternalID string `json:"external_id"`
}

// NewResolveIdentityHandler returns a HandlerInfo for the "resolve_identity" job type.
func NewResolveIdentityHandler(deps Deps) jobs.Entry {
	return jobs.Entry{
		Handler:  resolveIdentityHandler(deps),
		Systems:  []string{}, // source resolved at runtime
		PoolSize: 4,
		Lease:    30 * time.Second,
	}
}

func resolveIdentityHandler(deps Deps) jobs.Handler {
	return func(ctx context.Context, payload []byte) error {
		var p resolveIdentityPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return fmt.Errorf("%w: resolve_identity unmarshal: %v", jobs.ErrFatal, err)
		}
		if p.PersonID == 0 {
			return fmt.Errorf("%w: resolve_identity: person_id is required", jobs.ErrFatal)
		}

		// Step 1: check if email is already set.
		var currentEmail *string
		err := deps.DB.QueryRow(ctx,
			`SELECT email FROM graph.people WHERE id = $1`, p.PersonID,
		).Scan(&currentEmail)
		if err != nil {
			return fmt.Errorf("%w: resolve_identity: people lookup: %v", jobs.ErrFatal, err)
		}

		// If email is already known, skip the external API call.
		if currentEmail != nil && *currentEmail != "" {
			// Attempt merge in case a duplicate exists.
			if _, mergeErr := deps.Identity.MergeByEmail(ctx, *currentEmail); mergeErr != nil {
				deps.Logger.Warn().Err(mergeErr).Str("email", *currentEmail).Msg("resolve_identity: MergeByEmail failed")
			}
			return nil
		}

		// Step 1b: query source user-info API.
		email, displayName, err := fetchUserInfo(ctx, p.Source, p.ExternalID, deps.SlackBotToken)
		if err != nil {
			return err // already wrapped as ErrTransient or ErrFatal
		}

		if email == "" {
			// Some sources don't return email; treat as non-fatal and skip.
			deps.Logger.Warn().Str("source", p.Source).Str("external_id", p.ExternalID).
				Msg("resolve_identity: no email from user-info API")
			return nil
		}

		// Step 2: UPDATE only if email is still NULL (race-safe).
		_, err = deps.DB.Exec(ctx, `
			UPDATE graph.people
			SET email = $2, display_name = COALESCE(NULLIF($3, ''), display_name), identity_resolved_at = NOW()
			WHERE id = $1 AND email IS NULL`,
			p.PersonID, email, displayName,
		)
		if err != nil {
			return fmt.Errorf("resolve_identity: update people: %w", err)
		}

		// Step 3: merge if another row already has this email.
		if _, mergeErr := deps.Identity.MergeByEmail(ctx, email); mergeErr != nil {
			deps.Logger.Warn().Err(mergeErr).Str("email", email).Msg("resolve_identity: MergeByEmail failed")
		}

		return nil
	}
}

// fetchUserInfo calls the source's user-info API and returns (email, displayName, error).
func fetchUserInfo(ctx context.Context, source, externalID, slackToken string) (string, string, error) {
	switch source {
	case "slack":
		return fetchSlackUserInfo(ctx, externalID, slackToken)
	case "jira", "confluence":
		return fetchAtlassianUserInfo(ctx, externalID)
	case "github":
		return fetchGitHubUserInfo(ctx, externalID)
	case "pagerduty":
		return fetchPagerDutyUserInfo(ctx, externalID)
	default:
		// datadog, sentry, gws — email must already be in payload
		return "", "", fmt.Errorf("%w: resolve_identity: source %q does not support user-info lookup", jobs.ErrFatal, source)
	}
}

// fetchSlackUserInfo calls users.info and extracts email + display_name.
func fetchSlackUserInfo(ctx context.Context, userID, token string) (string, string, error) {
	if token == "" {
		return "", "", fmt.Errorf("%w: resolve_identity: SLACK_BOT_TOKEN not set", jobs.ErrFatal)
	}

	url := "https://slack.com/api/users.info?user=" + userID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", fmt.Errorf("%w: resolve_identity: build slack request: %v", jobs.ErrFatal, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	data, err := doAPIRequest(req)
	if err != nil {
		return "", "", err
	}

	var resp struct {
		OK   bool `json:"ok"`
		User struct {
			Profile struct {
				Email       string `json:"email"`
				DisplayName string `json:"display_name"`
				RealName    string `json:"real_name"`
			} `json:"profile"`
		} `json:"user"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", "", fmt.Errorf("%w: resolve_identity: parse slack response: %v", jobs.ErrTransient, err)
	}
	if !resp.OK {
		return "", "", fmt.Errorf("%w: resolve_identity: slack users.info error: %s", jobs.ErrTransient, resp.Error)
	}

	name := resp.User.Profile.DisplayName
	if name == "" {
		name = resp.User.Profile.RealName
	}
	return resp.User.Profile.Email, name, nil
}

// fetchAtlassianUserInfo calls the Jira/Confluence user API.
func fetchAtlassianUserInfo(ctx context.Context, accountID string) (string, string, error) {
	baseURL := os.Getenv("JIRA_BASE_URL")
	if baseURL == "" {
		baseURL = "https://wegomushi.atlassian.net"
	}
	email := os.Getenv("JIRA_EMAIL")
	token := os.Getenv("JIRA_TOKEN")
	if token == "" {
		return "", "", fmt.Errorf("%w: resolve_identity: JIRA_TOKEN not set", jobs.ErrFatal)
	}

	url := baseURL + "/rest/api/3/user?accountId=" + accountID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", fmt.Errorf("%w: resolve_identity: build atlassian request: %v", jobs.ErrFatal, err)
	}
	if email != "" {
		req.SetBasicAuth(email, token)
	} else {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	data, err := doAPIRequest(req)
	if err != nil {
		return "", "", err
	}

	var resp struct {
		EmailAddress string `json:"emailAddress"`
		DisplayName  string `json:"displayName"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", "", fmt.Errorf("%w: resolve_identity: parse atlassian response: %v", jobs.ErrTransient, err)
	}
	return resp.EmailAddress, resp.DisplayName, nil
}

// fetchGitHubUserInfo calls the GitHub users API.
func fetchGitHubUserInfo(ctx context.Context, login string) (string, string, error) {
	token := os.Getenv("GH_TOKEN")
	baseURL := os.Getenv("GH_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}

	url := baseURL + "/users/" + login
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", fmt.Errorf("%w: resolve_identity: build github request: %v", jobs.ErrFatal, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	data, err := doAPIRequest(req)
	if err != nil {
		return "", "", err
	}

	var resp struct {
		Email string `json:"email"`
		Name  string `json:"name"`
		Login string `json:"login"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", "", fmt.Errorf("%w: resolve_identity: parse github response: %v", jobs.ErrTransient, err)
	}
	name := resp.Name
	if name == "" {
		name = resp.Login
	}
	return resp.Email, name, nil
}

// fetchPagerDutyUserInfo calls the PagerDuty users API.
func fetchPagerDutyUserInfo(ctx context.Context, userID string) (string, string, error) {
	token := os.Getenv("PAGERDUTY_TOKEN")
	if token == "" {
		return "", "", fmt.Errorf("%w: resolve_identity: PAGERDUTY_TOKEN not set", jobs.ErrFatal)
	}
	baseURL := os.Getenv("PAGERDUTY_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.pagerduty.com"
	}

	url := baseURL + "/users/" + userID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", "", fmt.Errorf("%w: resolve_identity: build pagerduty request: %v", jobs.ErrFatal, err)
	}
	req.Header.Set("Authorization", "Token token="+token)
	req.Header.Set("Accept", "application/vnd.pagerduty+json;version=2")

	data, err := doAPIRequest(req)
	if err != nil {
		return "", "", err
	}

	var resp struct {
		User struct {
			Email string `json:"email"`
			Name  string `json:"name"`
		} `json:"user"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", "", fmt.Errorf("%w: resolve_identity: parse pagerduty response: %v", jobs.ErrTransient, err)
	}
	return resp.User.Email, resp.User.Name, nil
}

// doAPIRequest executes a prepared HTTP request and returns the body bytes.
// Maps status codes to ErrTransient / ErrFatal appropriately.
func doAPIRequest(req *http.Request) ([]byte, error) {
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve_identity: http request: %v", jobs.ErrTransient, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve_identity: read response: %v", jobs.ErrTransient, err)
	}

	if resp.StatusCode == 429 || resp.StatusCode >= 500 {
		return nil, fmt.Errorf("%w: resolve_identity: HTTP %d", jobs.ErrTransient, resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%w: resolve_identity: HTTP %d", jobs.ErrFatal, resp.StatusCode)
	}
	return body, nil
}
