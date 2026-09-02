package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	"github.com/rs/zerolog"
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
		UsesLLM:  true,
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
		data, err := downloadWithAuth(
			ctx,
			p.ExternalURL,
			p.Source,
			deps.SlackBotToken,
			deps.JiraToken,
			deps.JiraEmail,
		)
		if err != nil {
			return fmt.Errorf("%w: describe_attachment download: %w", jobs.ErrTransient, err)
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
					if isNonResult(pageDesc, pageOCR) {
						// A single non-result page (err == nil, but the model said it
						// could not process it) must not poison the combined body — skip
						// it exactly like an errored page. The other pages still count,
						// so a lone bad page in a 40-page PDF never discards the rest.
						deps.Logger.Warn().Int("page", page.PageNumber).Str("node_id", p.NodeID).Msg("liteparse: page produced no usable content, skipping")
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
					return fmt.Errorf("describe_attachment Gemini.Describe: %w", err)
				}
			}

		case strings.HasPrefix(mime, "image/"):
			// downloadWithAuth only rejects HTTP status >= 400, but Slack/Jira serve
			// sign-in HTML with a 200. Sniff the bytes before the vision call so an
			// auth/error page never reaches Gemini as if it were an image.
			sniffed, sniffErr := sniffImageBytes(data, p.Mime)
			if sniffErr != nil {
				deps.Logger.Warn().
					Str("node_id", p.NodeID).
					Str("declared_mime", p.Mime).
					Str("sniffed", sniffed).
					Msg("describe_attachment: image bytes failed content sniff")
				return fmt.Errorf("describe_attachment: image content sniff: %w", sniffErr)
			}
			description, ocrText, entities, err = deps.Gemini.Describe(ctx, p.Mime, data, geminiDescribePrompt)
			if err != nil {
				return fmt.Errorf("describe_attachment Gemini.Describe: %w", err)
			}

		default:
			return fmt.Errorf("%w: describe_attachment: unsupported mime type %q", jobs.ErrFatal, p.Mime)
		}

		_ = entities

		// A vision call can return err == nil yet produce no usable content — the
		// model stating it could not process the input, or (for a document) every
		// page failing. Never persist that as knowledge: return a retryable error
		// before any write so a transient model blip recovers on its own and a
		// permanent failure terminates at MaxAttempts (worker.go:86) instead of
		// freezing junk into graph.nodes / artifact_bodies / artifact_index.
		if isNonResult(description, ocrText) {
			firstLine := strings.TrimSpace(description)
			if i := strings.IndexByte(firstLine, '\n'); i >= 0 {
				firstLine = firstLine[:i]
			}
			deps.Logger.Warn().
				Str("node_id", p.NodeID).
				Str("mime", p.Mime).
				Str("model_said", firstLine).
				Msg("describe_attachment: vision produced no usable content, refusing to persist")
			return fmt.Errorf("describe_attachment: no usable content for %s (mime %s)", p.NodeID, p.Mime)
		}

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
		embedding, err := deps.Gemini.EmbedWithOptions(ctx, description, graphEmbeddingOptions())
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
			p.NodeID, description, pgvector.NewVector(embedding), deps.MachineID,
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
func downloadWithAuth(ctx context.Context, url, source, slackToken, jiraToken, jiraEmail string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	switch source {
	case "slack":
		if slackToken != "" {
			req.Header.Set("Authorization", "Bearer "+slackToken)
		}
	case "jira", "confluence":
		if jiraToken != "" && jiraEmail != "" {
			req.SetBasicAuth(jiraEmail, jiraToken)
		} else if jiraToken != "" {
			req.Header.Set("Authorization", "Bearer "+jiraToken)
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
	case "wegohub":
		token := os.Getenv("AGENT_MEM_WEGOHUB_TOKEN")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
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

// nonResultMaxLen bounds how short a description may be before a failure marker
// counts as a non-result. A long, real description that merely discusses a
// failure (e.g. a screenshot of an error dialog) must never be discarded, so the
// marker rule only fires below this length.
const nonResultMaxLen = 200

// nonResultMarkers are phrases the vision model emits when it produced nothing
// usable. Matched case-insensitively, and only against a short description.
var nonResultMarkers = []string{
	"image processing failed",
	"unable to process the attachment",
	"no text could be extracted",
}

// isNonResult reports whether a vision call came back with no usable content —
// either empty, or the model explicitly saying it could not process the input.
// Conservative by design: a long, real description that happens to discuss a
// failure must NOT match (that is what the length guard protects).
func isNonResult(description, ocr string) bool {
	d := strings.TrimSpace(description)
	o := strings.TrimSpace(ocr)
	if d == "" && o == "" {
		return true
	}
	if len(d) < nonResultMaxLen {
		lower := strings.ToLower(d)
		for _, m := range nonResultMarkers {
			if strings.Contains(lower, m) {
				return true
			}
		}
	}
	return false
}

// sniffImageBytes validates that downloaded bytes plausibly match a declared
// image/* mime before they reach the vision model. It returns the sniffed type
// and a non-nil error for an empty download or bytes that sniff as HTML/plain
// text — the "auth/error page served with HTTP 200" class that downloadWithAuth
// cannot catch (it only rejects HTTP status >= 400, and Slack/Jira serve sign-in
// HTML with a 200). http.DetectContentType examines only the first 512 bytes.
func sniffImageBytes(data []byte, declaredMime string) (sniffed string, err error) {
	if len(data) == 0 {
		return "", fmt.Errorf("empty download for declared mime %q", declaredMime)
	}
	sniffed = http.DetectContentType(data)
	if strings.HasPrefix(sniffed, "text/html") || strings.HasPrefix(sniffed, "text/plain") {
		return sniffed, fmt.Errorf("declared %q but bytes sniffed as %q", declaredMime, sniffed)
	}
	return sniffed, nil
}

// backfillFailedAttachmentsDefaultLimit caps a single backfill invocation so a
// bulk LLM re-run stays bounded and monitored (silent truncation reads as
// "covered everything" when it did not).
const backfillFailedAttachmentsDefaultLimit = 50

// BackfillFailedAttachments re-enqueues describe_attachment for attachment nodes
// whose stored body is a persisted vision failure (agent-mem-16e) — rows that
// will never re-run on their own because the original job succeeded. It follows
// the shape of BackfillMissingThreadSummaries: a capped query + dedup'd enqueue
// loop. It is capped, deduped against already-queued/running jobs, and logs
// matched-vs-enqueued. Trigger it EXPLICITLY (admin endpoint) — never on startup,
// and per project policy never against production. Returns (matched, enqueued).
//
// The body_full markers are anchored at the start of the body: a persisted
// non-result begins with the failure phrase (the observed rows all match
// 'Image processing failed%'), while a legitimate long description never opens
// with one. Anchoring is the conservative choice that mirrors the evidence query
// and avoids re-enqueuing a real description that merely mentions these phrases.
func BackfillFailedAttachments(ctx context.Context, db *pgxpool.Pool, logger zerolog.Logger, limit int) (matched, enqueued int) {
	if limit <= 0 {
		limit = backfillFailedAttachmentsDefaultLimit
	}
	rows, err := db.Query(ctx, `
SELECT n.id, COALESCE(n.url, ''), COALESCE(n.mime_type, '')
FROM graph.artifact_bodies ab
JOIN graph.nodes n ON n.id = ab.node_id
WHERE n.deleted_at IS NULL
  AND n.type IN ('slack_file', 'jira_attachment')
  AND (
    ab.body_full ILIKE 'image processing failed%'
    OR ab.body_full ILIKE 'unable to process the attachment%'
  )
LIMIT $1`, limit)
	if err != nil {
		logger.Warn().Err(err).Msg("backfill_failed_attachments: query failed")
		return 0, 0
	}
	type row struct{ id, url, mime string }
	var todo []row
	for rows.Next() {
		var r row
		if scanErr := rows.Scan(&r.id, &r.url, &r.mime); scanErr != nil {
			rows.Close()
			logger.Warn().Err(scanErr).Msg("backfill_failed_attachments: scan failed")
			return len(todo), enqueued
		}
		todo = append(todo, r)
	}
	rows.Close()
	matched = len(todo)
	for _, r := range todo {
		mime := r.mime
		if mime == "" {
			// Attachment nodes don't persist mime_type (ingest_content.go:319
			// stores only id/type/url), and every poisoned row is an image
			// failure, so derive an image mime from the URL to route the re-run
			// back through the image branch.
			mime = imageMimeFromURL(r.url)
		}
		if enqueueDescribeAttachment(ctx, db, r.id, r.url, mime, nodeSourceFromID(r.id)) {
			enqueued++
		}
	}
	logger.Info().Int("matched", matched).Int("enqueued", enqueued).Int("limit", limit).Msg("backfill_failed_attachments: done")
	return matched, enqueued
}

// enqueueDescribeAttachment enqueues a describe_attachment job for nodeID unless
// one is already queued/running for the same node — cheap dedup mirroring
// enqueueSummarizeThread, so a backfill can't pile duplicate LLM jobs on a node.
// Returns true if a job was enqueued.
func enqueueDescribeAttachment(ctx context.Context, db *pgxpool.Pool, nodeID, externalURL, mime, source string) bool {
	if nodeID == "" || externalURL == "" {
		return false
	}
	var exists bool
	_ = db.QueryRow(ctx, `
SELECT EXISTS(
  SELECT 1 FROM graph.jobs
  WHERE type='describe_attachment' AND status IN ('queued','running')
    AND payload->>'node_id'=$1)`, nodeID).Scan(&exists)
	if exists {
		return false
	}
	_, err := jobs.Enqueue(ctx, db, "describe_attachment", describeAttachmentPayload{
		NodeID:      nodeID,
		ExternalURL: externalURL,
		Mime:        mime,
		Source:      source,
	}, jobs.EnqueueOptions{Priority: 5})
	return err == nil
}

// imageMimeFromURL infers an image mime from a URL's file extension, defaulting
// to image/png. Used by the backfill when a node has no stored mime_type.
func imageMimeFromURL(rawURL string) string {
	u := rawURL
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	switch strings.ToLower(path.Ext(u)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	case ".tif", ".tiff":
		return "image/tiff"
	case ".heic":
		return "image/heic"
	default:
		return "image/png"
	}
}

// nodeSourceFromID maps an attachment node id prefix to the download source used
// by downloadWithAuth's auth switch.
func nodeSourceFromID(nodeID string) string {
	if strings.HasPrefix(nodeID, "jira_attachment:") {
		return "jira"
	}
	return "slack"
}
