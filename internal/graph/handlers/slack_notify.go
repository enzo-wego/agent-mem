package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

var slackHTTP = &http.Client{Timeout: 15 * time.Second}

// slackDM opens (or reuses) a DM channel with userID and posts text as the bot
// identity behind token (enzobot). Best-effort: the caller logs failures and
// moves on — a failed DM must not wedge the detect job.
func slackDM(ctx context.Context, token, userID, text string) error {
	if token == "" || userID == "" {
		return fmt.Errorf("slack dm: missing token or user id")
	}
	channelID, err := slackOpenIM(ctx, token, userID)
	if err != nil {
		return err
	}
	return slackPostMessage(ctx, token, channelID, text)
}

// slackOpenIM resolves the DM channel id for a user via conversations.open.
func slackOpenIM(ctx context.Context, token, userID string) (string, error) {
	var resp struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
	}
	if err := slackPostJSON(ctx, token, "https://slack.com/api/conversations.open",
		map[string]any{"users": userID}, &resp); err != nil {
		return "", err
	}
	if !resp.OK {
		return "", fmt.Errorf("conversations.open: %s", resp.Error)
	}
	return resp.Channel.ID, nil
}

// slackPostMessage posts text to a channel id via chat.postMessage.
func slackPostMessage(ctx context.Context, token, channelID, text string) error {
	_, err := slackPostMessageTS(ctx, token, channelID, text, "")
	return err
}

// slackPostMessageTS posts text to a channel and returns the message ts. When
// threadTS is non-empty the message is posted as a reply in that thread — this
// is how the hourly monitor keeps its 7-day run in a single DM thread.
func slackPostMessageTS(ctx context.Context, token, channelID, text, threadTS string) (string, error) {
	body := map[string]any{"channel": channelID, "text": text, "unfurl_links": false}
	if threadTS != "" {
		body["thread_ts"] = threadTS
	}
	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		TS    string `json:"ts"`
	}
	if err := slackPostJSON(ctx, token, "https://slack.com/api/chat.postMessage", body, &resp); err != nil {
		return "", err
	}
	if !resp.OK {
		return "", fmt.Errorf("chat.postMessage: %s", resp.Error)
	}
	return resp.TS, nil
}

// slackPostJSON does a JSON POST with a Bearer token and decodes into dst.
func slackPostJSON(ctx context.Context, token, url string, body any, dst any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	res, err := slackHTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	return json.NewDecoder(res.Body).Decode(dst)
}
