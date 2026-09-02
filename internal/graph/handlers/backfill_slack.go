package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/identity"
	"github.com/agent-mem/agent-mem/internal/graph/ids"
	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// backfillSlackChannelPayload is the JSON payload for the backfill_slack_channel job type.
type backfillSlackChannelPayload struct {
	ChannelID string `json:"channel_id"`
	OldestTS  string `json:"oldest_ts"`
	Cursor    string `json:"cursor"`
}

// NewBackfillSlackChannelHandler returns a HandlerInfo for the "backfill_slack_channel" job type.
func NewBackfillSlackChannelHandler(deps Deps) jobs.Entry {
	return jobs.Entry{
		Handler:  backfillSlackChannelHandler(deps),
		Systems:  []string{"slack"},
		PoolSize: 2,
		Lease:    120 * time.Second,
	}
}

func backfillSlackChannelHandler(deps Deps) jobs.Handler {
	return func(ctx context.Context, payload []byte) error {
		var p backfillSlackChannelPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return fmt.Errorf("%w: backfill_slack_channel unmarshal: %v", jobs.ErrFatal, err)
		}
		if p.ChannelID == "" || p.OldestTS == "" {
			return fmt.Errorf("%w: backfill_slack_channel: channel_id and oldest_ts required", jobs.ErrFatal)
		}

		token := deps.SlackBotToken
		if token == "" {
			return fmt.Errorf("%w: backfill_slack_channel: SLACK_BOT_TOKEN not set", jobs.ErrFatal)
		}

		// Call conversations.history.
		histResp, err := fetchConversationsHistory(ctx, token, p.ChannelID, p.OldestTS, p.Cursor)
		if err != nil {
			return err
		}

		// Process each parent message.
		for _, msg := range histResp.Messages {
			// Skip messages with a subtype (joins, leaves, etc.) and bot self.
			automated := slackMessageAutomated(msg)
			alertDecision := decideAlertBot(ctx, deps, p.ChannelID, msg.Text, automated)
			if forceAlertThreadBackfill(msg, alertDecision) && alertThreadHasNonBotReply(ctx, deps, msg.ReplyUsers) {
				threadPayload := backfillSlackThreadPayload{
					ChannelID:        p.ChannelID,
					ThreadTs:         msg.ThreadTs,
					Cursor:           "",
					ForceAlertThread: true,
				}
				if threadPayload.ThreadTs == "" {
					threadPayload.ThreadTs = msg.Ts
				}
				if _, jErr := jobs.Enqueue(ctx, deps.DB, "backfill_slack_thread", threadPayload, jobs.EnqueueOptions{
					Priority:     5,
					TargetRunner: "vps",
					MachineID:    deps.MachineID,
				}); jErr != nil {
					deps.Logger.Warn().Err(jErr).
						Str("thread_ts", threadPayload.ThreadTs).
						Msg("backfill_slack_channel: enqueue forced alert thread backfill failed")
				}
			}
			if shouldSkipSlackMessageForAlertPolicy(msg, alertDecision, false) {
				continue
			}

			if err := ingestSlackMessage(ctx, deps, p.ChannelID, msg); err != nil {
				deps.Logger.Warn().Err(err).
					Str("channel_id", p.ChannelID).
					Str("ts", msg.Ts).
					Msg("backfill_slack_channel: ingest message failed; skipping")
			}

			// If this message is a thread parent with replies, enqueue thread backfill.
			if msg.ThreadTs == msg.Ts && msg.ReplyCount > 0 {
				threadPayload := backfillSlackThreadPayload{
					ChannelID: p.ChannelID,
					ThreadTs:  msg.ThreadTs,
					Cursor:    "",
				}
				if _, jErr := jobs.Enqueue(ctx, deps.DB, "backfill_slack_thread", threadPayload, jobs.EnqueueOptions{
					Priority:     5,
					TargetRunner: "vps",
					MachineID:    deps.MachineID,
				}); jErr != nil {
					deps.Logger.Warn().Err(jErr).
						Str("thread_ts", msg.ThreadTs).
						Msg("backfill_slack_channel: enqueue thread backfill failed")
				}
			}
		}

		// If there are more pages, re-enqueue self with the next cursor.
		if histResp.ResponseMetadata.NextCursor != "" {
			nextPayload := backfillSlackChannelPayload{
				ChannelID: p.ChannelID,
				OldestTS:  p.OldestTS,
				Cursor:    histResp.ResponseMetadata.NextCursor,
			}
			if _, jErr := jobs.Enqueue(ctx, deps.DB, "backfill_slack_channel", nextPayload, jobs.EnqueueOptions{
				Priority:     5,
				TargetRunner: "vps",
				MachineID:    deps.MachineID,
			}); jErr != nil {
				return fmt.Errorf("backfill_slack_channel: re-enqueue next page: %w", jErr)
			}
		}

		return nil
	}
}

// slackMessage represents a single Slack message from conversations.history / conversations.replies.
type slackMessage struct {
	Ts         string      `json:"ts"`
	ThreadTs   string      `json:"thread_ts"`
	User       string      `json:"user"`
	BotID      string      `json:"bot_id"`
	Subtype    string      `json:"subtype"`
	Text       string      `json:"text"`
	ReplyCount int         `json:"reply_count"`
	ReplyUsers []string    `json:"reply_users"`
	Files      []slackFile `json:"files"`
	Edited     *struct{}   `json:"edited"`
}

// slackFile is a file attachment in a Slack message.
type slackFile struct {
	ID         string `json:"id"`
	MimeType   string `json:"mimetype"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	URLPrivate string `json:"url_private"`
	Thumb360   string `json:"thumb_360"`
}

type slackHistoryResponse struct {
	OK               bool           `json:"ok"`
	Messages         []slackMessage `json:"messages"`
	Error            string         `json:"error"`
	ResponseMetadata struct {
		NextCursor string `json:"next_cursor"`
	} `json:"response_metadata"`
}

// fetchConversationsHistory calls Slack conversations.history with pagination support.
func fetchConversationsHistory(ctx context.Context, token, channelID, oldestTS, cursor string) (*slackHistoryResponse, error) {
	params := url.Values{}
	params.Set("channel", channelID)
	params.Set("oldest", oldestTS)
	params.Set("limit", "200")
	if cursor != "" {
		params.Set("cursor", cursor)
	}

	apiURL := "https://slack.com/api/conversations.history?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: backfill_slack_channel: build request: %v", jobs.ErrFatal, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: backfill_slack_channel: http: %v", jobs.ErrTransient, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: backfill_slack_channel: read body: %v", jobs.ErrTransient, err)
	}

	if resp.StatusCode == 429 || resp.StatusCode >= 500 {
		return nil, fmt.Errorf("%w: backfill_slack_channel: HTTP %d", jobs.ErrTransient, resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%w: backfill_slack_channel: HTTP %d", jobs.ErrFatal, resp.StatusCode)
	}

	var apiResp slackHistoryResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("%w: backfill_slack_channel: parse response: %v", jobs.ErrTransient, err)
	}
	if !apiResp.OK {
		if apiResp.Error == "ratelimited" {
			return nil, fmt.Errorf("%w: backfill_slack_channel: ratelimited", jobs.ErrTransient)
		}
		return nil, fmt.Errorf("%w: backfill_slack_channel: slack error: %s", jobs.ErrTransient, apiResp.Error)
	}

	return &apiResp, nil
}

// ingestSlackMessage calls ingestContent with the data for a single Slack message.
func ingestSlackMessage(ctx context.Context, deps Deps, channelID string, msg slackMessage) error {
	nodeID := ids.SlackMessage(channelID, msg.Ts)
	naturalKey, _ := ids.ParseNaturalKey(nodeID)
	nodeType, _ := ids.ParseType(nodeID)

	// Parse body_ts from Slack unix timestamp string.
	bodyTS := slackTsToTime(msg.Ts)

	// Build canonical URL (best-effort; backfill doesn't always have workspace slug).
	canonicalURL := fmt.Sprintf("https://slack.com/archives/%s/p%s", channelID, slackTsToP(msg.Ts))

	// Resolve author.
	var authorPersonID *int64
	if deps.Identity != nil && msg.User != "" {
		pid, _, idErr := deps.Identity.EnsurePerson(ctx, identity.Ref{
			Source:     "slack",
			ExternalID: msg.User,
		})
		if idErr != nil {
			deps.Logger.Warn().Err(idErr).Str("user", msg.User).Msg("ingestSlackMessage: EnsurePerson failed")
		} else {
			authorPersonID = &pid
		}
	}

	scope := "slack:" + channelID

	// Build metadata JSON matching ingestContentMetadata shape.
	threadTs := msg.ThreadTs
	if threadTs == "" {
		threadTs = msg.Ts
	}
	meta := ingestContentMetadata{
		Ts:        msg.Ts,
		BodyTS:    bodyTS.Format(time.RFC3339),
		ChannelID: channelID,
		ThreadTs:  threadTs,
		Scope:     scope,
	}
	if msg.User != "" {
		meta.Author = ingestAuthorRef{Ref: "slack_uid:" + msg.User}
	}
	// Attach files.
	for _, f := range msg.Files {
		meta.Files = append(meta.Files, ingestFileRef{
			ID:         f.ID,
			MimeType:   f.MimeType,
			Filename:   f.Name,
			Size:       f.Size,
			URLPrivate: f.URLPrivate,
			Thumb360:   f.Thumb360,
		})
	}

	// Normalize the raw Slack text: resolve <@U…>/<!subteam^S…> mentions to names
	// and <url|label> links, matching the live ingest path so stored/displayed
	// bodies never contain raw Slack ids. Extraction below runs on the normalized
	// text (the normalizer preserves URLs as "label (url)").
	text := msg.Text
	if sn, ok := deps.Normalizers.For("slack"); ok {
		if res, nErr := sn.Normalize(ctx, []byte(msg.Text), nil); nErr == nil {
			text = res.Text
			for _, m := range res.Mentions {
				tag := "slack_uid"
				if m.Source == "slack_group" {
					tag = "slack_group"
				}
				meta.Mentions = append(meta.Mentions, ingestMentionRef{Ref: tag + ":" + m.ExternalID, DisplayName: m.DisplayName})
			}
		}
	}

	metaJSON, _ := json.Marshal(meta)

	// For Slack the message post time is the canonical created_at.
	createdAt := bodyTS
	outcome, upsertErr := upsertNodeOutcome(
		ctx, deps,
		nodeID, string(nodeType), naturalKey,
		canonicalURL, "", text,
		bodyTS, &createdAt, authorPersonID, scope, metaJSON,
	)
	if upsertErr != nil {
		return fmt.Errorf("upsert node: %w", upsertErr)
	}

	// Upsert artifact_bodies.
	if _, abErr := deps.DB.Exec(ctx, `
		INSERT INTO graph.artifact_bodies (node_id, body_full, fetched_at, machine_id)
		VALUES ($1, $2, NOW(), $3)
		ON CONFLICT (node_id) DO UPDATE SET
			body_full  = EXCLUDED.body_full,
			fetched_at = NOW()`,
		nodeID, text, deps.MachineID,
	); abErr != nil {
		deps.Logger.Warn().Err(abErr).Str("node_id", nodeID).Msg("ingestSlackMessage: upsert artifact_bodies failed")
	}

	// Enqueue index_artifact so semantic search has embeddings.
	if text != "" {
		if _, jErr := jobs.Enqueue(ctx, deps.DB, "index_artifact", map[string]any{
			"node_id": nodeID,
			"force":   false,
		}, jobs.EnqueueOptions{
			Priority:  5,
			MachineID: deps.MachineID,
		}); jErr != nil {
			deps.Logger.Warn().Err(jErr).Str("node_id", nodeID).Msg("ingestSlackMessage: enqueue index_artifact failed")
		}
	}

	// Reconcile edges if extractor available.
	if deps.Extractor != nil && text != "" {
		extractResult, extErr := deps.Extractor.Extract(ctx, text)
		if extErr == nil {
			upsertedIDs, _ := reconcileEdges(ctx, deps, nodeID, extractResult.Findings)
			_ = pruneStaleEdges(ctx, deps, nodeID, upsertedIDs)
			// Enqueue fetch_body for referenced nodes with no body yet, so the
			// cross-source artifacts a message links to (Jira/PR/Confluence) get
			// enriched — not just left as title-less edge stubs.
			for _, fnd := range extractResult.Findings {
				enqueueFetchIfEmpty(ctx, deps, fnd.NodeID, fnd.Type)
			}
		}
	}

	// Enqueue fetch_body if new or updated.
	if outcome != "unchanged" {
		_, _ = jobs.Enqueue(ctx, deps.DB, "fetch_body", map[string]string{
			"node_id": nodeID,
			"source":  "slack",
		}, jobs.EnqueueOptions{
			Priority:     0,
			TargetRunner: "vps",
			MachineID:    deps.MachineID,
		})
	}

	// Process file attachments.
	for _, f := range msg.Files {
		attNodeID := ids.SlackFile(f.ID)
		attNK, _ := ids.ParseNaturalKey(attNodeID)
		attType, _ := ids.ParseType(attNodeID)

		if _, attErr := deps.DB.Exec(ctx, `
			INSERT INTO graph.nodes (id, type, natural_key, url, updated_at, machine_id)
			VALUES ($1, $2, $3, $4, NOW(), $5)
			ON CONFLICT (id) DO NOTHING`,
			attNodeID, string(attType), attNK, f.URLPrivate, deps.MachineID,
		); attErr != nil {
			deps.Logger.Warn().Err(attErr).Str("att_node_id", attNodeID).Msg("ingestSlackMessage: upsert attachment node failed")
		}
		if _, edgeErr := deps.DB.Exec(ctx, `
			INSERT INTO graph.edges (from_node_id, to_node_id, kind, source_msg_id, machine_id)
			VALUES ($1, $2, 'REFERENCES', $3, $4)
			ON CONFLICT (from_node_id, to_node_id, kind) DO UPDATE SET
				source_msg_id = EXCLUDED.source_msg_id`,
			nodeID, attNodeID, nodeID, deps.MachineID,
		); edgeErr != nil {
			deps.Logger.Warn().Err(edgeErr).Str("att_node_id", attNodeID).Msg("ingestSlackMessage: upsert attachment edge failed")
		}
		_, _ = jobs.Enqueue(ctx, deps.DB, "describe_attachment", map[string]string{
			"node_id":      attNodeID,
			"external_url": f.URLPrivate,
			"mime":         f.MimeType,
			"source":       "slack",
		}, jobs.EnqueueOptions{
			Priority:  5,
			MachineID: deps.MachineID,
		})
	}

	return nil
}

// slackTsToTime converts a Slack timestamp string ("1234567890.123456") to time.Time.
func slackTsToTime(ts string) time.Time {
	var sec, frac int64
	fmt.Sscanf(ts, "%d.%d", &sec, &frac)
	if sec == 0 {
		return time.Now().UTC()
	}
	return time.Unix(sec, 0).UTC()
}

// slackTsToP converts a Slack ts like "1234567890.123456" to the Slack archive p-format "1234567890123456".
func slackTsToP(ts string) string {
	// Remove the dot.
	result := ""
	for _, ch := range ts {
		if ch != '.' {
			result += string(ch)
		}
	}
	return result
}
