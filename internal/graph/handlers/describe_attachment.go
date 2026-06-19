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

// liteParseRichTextThreshold is the minimum total character count across all pages
// for a LiteParse extraction to be considered "rich" (i.e. worth skipping Gemini multimodal).
const liteParseRichTextThreshold = 200

// describeAttachmentPayload is the JSON payload for the describe_attachment job type.
type describeAttachmentPayload struct {
	NodeID      string `json:"node_id"`
	ExternalURL string `json:"external_url"`
	Mime        string `json:"mime"`
	Source      string `json:"source"`
}

// NewDescribeAttachmentHandler returns a HandlerInfo for the "describe_attachment" job type.
func NewDescribeAttachmentHandler(deps Deps) jobs.Entry {
	return jobs.Entry{
		Handler:  describeAttachmentHandler(deps),
		Systems:  []string{"gemini"},
		PoolSize: 4,
		Lease:    120 * time.Second,
	}
}

// isDocumentMime returns true for PDF and office document MIME types handled by LiteParse.
func isDocumentMime(mime string) bool {
	switch mime {
	case "application/pdf",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation":
		return true
	}
	return false
}

func describeAttachmentHandler(deps Deps) jobs.Handler {
	const geminiDescribePrompt = "Describe this attachment in detail. Extract any visible text (OCR). List key entities mentioned."

	return func(ctx context.Context, payload []byte) error {
		var p describeAttachmentPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return fmt.Errorf("%w: describe_attachment unmarshal: %v", jobs.ErrFatal, err)
		}

		if p.ExternalURL == "" {
			return fmt.Errorf("%w: describe_attachment: external_url is empty", jobs.ErrFatal)
		}

		// Step 2: download the bytes with source-appropriate auth.
		data, err := downloadWithAuth(ctx, p.ExternalURL, p.Source)
		if err != nil {
			return fmt.Errorf("%w: describe_attachment download: %v", jobs.ErrTransient, err)
		}

		// Step 3: branch on mime.
		mime := strings.ToLower(p.Mime)
		var description, ocrText string
		var entities []string

		switch {
		case strings.HasPrefix(mime, "text/"):
			description = string(data)

		case isDocumentMime(mime):
			lp, _ := ParseDocument(ctx, deps.LiteParse, data, mime, deps.Logger)

			if lp.Available && lp.TotalTextLen >= liteParseRichTextThreshold {
				// Tier 2a: rich text extraction — skip Gemini multimodal entirely.
				description = combinePageTexts(lp.Pages)

			} else if lp.Available {
				// Tier 2b: thin extraction — run Gemini Vision on per-page screenshots.
				for _, page := range lp.Pages {
					if len(page.ScreenshotBytes) == 0 {
						continue
					}
					pageDesc, pageOCR, pageEnts, descErr := deps.Gemini.Describe(ctx, "image/png", page.ScreenshotBytes, geminiDescribePrompt)
					if descErr != nil {
						deps.Logger.Warn().Err(descErr).Int("page", page.PageNumber).Msg("liteparse: Gemini screenshot describe")
						continue
					}
					description += pageDesc + "\n\n"
					ocrText += pageOCR + "\n\n"
					entities = append(entities, pageEnts...)
				}
				description = strings.TrimSpace(description)
				ocrText = strings.TrimSpace(ocrText)

			} else {
				// LiteParse unavailable — fall back to current behaviour.
				deps.Logger.Warn().Str("reason", lp.FailureReason).Msg("liteparse: unavailable, falling back to Gemini multimodal")
				description, ocrText, entities, err = deps.Gemini.Describe(ctx, p.Mime, data, geminiDescribePrompt)
				if err != nil {
					return fmt.Errorf("%w: describe_attachment Gemini.Describe: %v", jobs.ErrTransient, err)
				}
			}

		case strings.HasPrefix(mime, "image/"):
			description, ocrText, entities, err = deps.Gemini.Describe(ctx, p.Mime, data, geminiDescribePrompt)
			if err != nil {
				return fmt.Errorf("%w: describe_attachment Gemini.Describe: %v", jobs.ErrTransient, err)
			}

		default:
			return fmt.Errorf("%w: describe_attachment: unsupported mime type %q", jobs.ErrFatal, p.Mime)
		}

		_ = entities

		// Step 4: UPDATE graph.nodes body for this media node.
		_, err = deps.DB.Exec(ctx, `
			UPDATE graph.nodes SET body = $2, updated_at = NOW()
			WHERE id = $1`,
			p.NodeID, description,
		)
		if err != nil {
			return fmt.Errorf("describe_attachment: update node body: %w", err)
		}

		// Step 5: UPSERT graph.artifact_bodies.
		fullBody := description
		if ocrText != "" {
			fullBody = description + "\n\nOCR:\n" + ocrText
		}
		_, err = deps.DB.Exec(ctx, `
			INSERT INTO graph.artifact_bodies (node_id, body_full, fetched_at, machine_id)
			VALUES ($1, $2, NOW(), $3)
			ON CONFLICT (node_id) DO UPDATE SET
				body_full  = EXCLUDED.body_full,
				fetched_at = NOW()`,
			p.NodeID, fullBody, deps.MachineID,
		)
		if err != nil {
			return fmt.Errorf("describe_attachment: upsert artifact_bodies: %w", err)
		}

		// Step 6: UPSERT graph.artifact_index with embedding.
		embedding, err := deps.Gemini.Embed(ctx, description)
		if err != nil {
			return fmt.Errorf("%w: describe_attachment embed: %v", jobs.ErrTransient, err)
		}

		_, err = deps.DB.Exec(ctx, `
			INSERT INTO graph.artifact_index (node_id, summary, summary_kind, embedding, refreshed_at, machine_id)
			VALUES ($1, $2, 'gemini', $3, NOW(), $4)
			ON CONFLICT (node_id) DO UPDATE SET
				summary      = EXCLUDED.summary,
				summary_kind = EXCLUDED.summary_kind,
				embedding    = EXCLUDED.embedding,
				refreshed_at = NOW()`,
			p.NodeID, description, embedding, deps.MachineID,
		)
		if err != nil {
			return fmt.Errorf("describe_attachment: upsert artifact_index: %w", err)
		}

		// Step 7: re-run extractor on description + ocr and reconcile edges.
		extractBody := description
		if ocrText != "" {
			extractBody = description + " " + ocrText
		}
		extractResult, err := deps.Extractor.Extract(ctx, extractBody)
		if err != nil {
			deps.Logger.Warn().Err(err).Str("node_id", p.NodeID).Msg("describe_attachment: extractor failed")
			return nil
		}
		_, edgeErr := reconcileEdges(ctx, deps, p.NodeID, extractResult.Findings)
		if edgeErr != nil {
			deps.Logger.Warn().Err(edgeErr).Str("node_id", p.NodeID).Msg("describe_attachment: reconcileEdges failed")
		}

		return nil
	}
}

// downloadWithAuth downloads bytes from url, injecting the appropriate auth header.
func downloadWithAuth(ctx context.Context, url, source string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	switch source {
	case "slack":
		token := os.Getenv("SLACK_BOT_TOKEN")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	case "jira", "confluence":
		token := os.Getenv("JIRA_TOKEN")
		email := os.Getenv("JIRA_EMAIL")
		if token != "" && email != "" {
			req.SetBasicAuth(email, token)
		} else if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	case "github":
		token := os.Getenv("GH_TOKEN")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	case "pagerduty":
		token := os.Getenv("PAGERDUTY_TOKEN")
		if token != "" {
			req.Header.Set("Authorization", "Token token="+token)
		}
	}

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 || resp.StatusCode == 429 {
		return nil, fmt.Errorf("download HTTP %d: %s", resp.StatusCode, url)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%w: download HTTP %d: %s", jobs.ErrFatal, resp.StatusCode, url)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return data, nil
}
