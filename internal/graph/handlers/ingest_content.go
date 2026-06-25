package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/identity"
	"github.com/agent-mem/agent-mem/internal/graph/ids"
	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// validSources is the whitelist of allowed source values for ingest/content.
var validSources = map[string]bool{
	"slack":      true,
	"jira":       true,
	"github":     true,
	"confluence": true,
	"pagerduty":  true,
	"datadog":    true,
	"sentry":     true,
	"gws":        true,
}

// ingestContentRequest is the request body for POST /api/graph/ingest/content.
type ingestContentRequest struct {
	Source       string                `json:"source"`
	CanonicalURL string                `json:"canonical_url"`
	Body         string                `json:"body"`
	Metadata     ingestContentMetadata `json:"metadata"`
}

type ingestContentMetadata struct {
	Author   ingestAuthorRef    `json:"author"`
	Mentions []ingestMentionRef `json:"mentions"`
	Ts       string             `json:"ts"`
	BodyTS   string             `json:"body_ts"`

	// Slack-specific
	ChannelID string  `json:"channel_id"`
	ThreadTs  string  `json:"thread_ts"`
	Subtype   *string `json:"subtype"`
	Edited    bool    `json:"edited"`
	Deleted   bool    `json:"deleted"`
	Files     []ingestFileRef `json:"files"`
	Scope     string          `json:"scope"`

	// Jira fields
	Key        string `json:"key"`
	ProjectKey string `json:"project_key"`

	// GitHub fields
	Repo   string `json:"repo"`
	Number int    `json:"number"`

	// Confluence fields
	PageID   int64  `json:"page_id"`
	SpaceKey string `json:"space_key"`

	// PagerDuty fields
	IncidentID string `json:"incident_id"`

	// Datadog fields
	ObjectType string `json:"object_type"`
	ObjectID   int64  `json:"object_id"`

	// Sentry fields
	IssueID string `json:"issue_id"`

	// GWS fields
	DriveID string `json:"drive_id"`
}

type ingestAuthorRef struct {
	Ref         string `json:"ref"`
	DisplayName string `json:"display_name"`
	IsBot       bool   `json:"is_bot"`
}

type ingestMentionRef struct {
	Ref         string `json:"ref"`
	DisplayName string `json:"display_name"`
}

type ingestFileRef struct {
	ID         string `json:"id"`
	MimeType   string `json:"mimetype"`
	Filename   string `json:"filename"`
	Size       int64  `json:"size"`
	URLPrivate string `json:"url_private"`
	Thumb360   string `json:"thumb_360"`
}

// ingestResponse is the response body for POST /api/graph/ingest/content.
type ingestResponse struct {
	NodeID                string                     `json:"node_id"`
	Outcome               string                     `json:"outcome"`
	Extracted             extractedView              `json:"extracted"`
	EdgesCreated          int                        `json:"edges_created"`
	AttachmentsRegistered []attachmentRegisteredView `json:"attachments_registered,omitempty"`
	JobsEnqueued          []jobEnqueuedView          `json:"jobs_enqueued,omitempty"`
}

type extractedView struct {
	URLs     []string `json:"urls"`
	Entities []string `json:"entities"`
	JiraKeys []string `json:"jira_keys"`
	GHPRs    []string `json:"gh_prs"`
	People   []string `json:"people"`
}

type attachmentRegisteredView struct {
	NodeID  string `json:"node_id"`
	Outcome string `json:"outcome"`
}

type jobEnqueuedView struct {
	ID       int64  `json:"id"`
	Type     string `json:"type"`
	Priority int16  `json:"priority"`
}

// writeJSON writes v as JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// NewIngestContentHandler returns an http.Handler for POST /api/graph/ingest/content.
func NewIngestContentHandler(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ingestContentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}

		if !validSources[req.Source] {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown source %q; valid: slack,jira,github,confluence,pagerduty,datadog,sentry,gws", req.Source))
			return
		}

		ctx := r.Context()

		// Compute node_id from source + metadata.
		nodeID, err := buildNodeID(req.Source, req.CanonicalURL, req.Metadata)
		if err != nil {
			writeError(w, http.StatusBadRequest, "cannot derive node_id: "+err.Error())
			return
		}

		// Deletion: the source message was deleted upstream — soft-delete our copy
		// (sets deleted_at so it stops showing) instead of upserting content.
		if req.Metadata.Deleted || (req.Metadata.Subtype != nil && *req.Metadata.Subtype == "message_deleted") {
			if _, delErr := deps.DB.Exec(ctx,
				`UPDATE graph.nodes SET deleted_at = NOW(), updated_at = NOW() WHERE id = $1 AND deleted_at IS NULL`,
				nodeID); delErr != nil {
				writeError(w, http.StatusInternalServerError, "soft-delete: "+delErr.Error())
				return
			}
			writeJSON(w, http.StatusOK, ingestResponse{NodeID: nodeID, Outcome: "deleted"})
			return
		}

		// Derive natural key and type prefix.
		naturalKey, _ := ids.ParseNaturalKey(nodeID)
		nodeType, _ := ids.ParseType(nodeID)

		// Parse body_ts.
		var bodyTS time.Time
		if req.Metadata.BodyTS != "" {
			bodyTS, _ = time.Parse(time.RFC3339, req.Metadata.BodyTS)
		}
		if bodyTS.IsZero() {
			bodyTS = time.Now().UTC()
		}

		// Resolve author via identity service.
		var authorPersonID *int64
		if deps.Identity != nil && req.Metadata.Author.Ref != "" {
			authorSource, authorExternalID := parseRef(req.Metadata.Author.Ref)
			if authorSource != "" && authorExternalID != "" {
				pid, _, idErr := deps.Identity.EnsurePerson(ctx, identity.Ref{
					Source:      authorSource,
					ExternalID:  authorExternalID,
					DisplayName: req.Metadata.Author.DisplayName,
					IsBot:       req.Metadata.Author.IsBot,
				})
				if idErr != nil {
					deps.Logger.Warn().Err(idErr).Msg("ingest_content: EnsurePerson failed; proceeding without author")
				} else {
					authorPersonID = &pid
				}
			}
		}

		// Build scope.
		scope := buildScope(req.Source, req.Metadata)

		// Build metadata JSONB.
		metaJSON, _ := json.Marshal(req.Metadata)

		// Canonical created_at = the source artifact's event time, if the sender
		// provided one (nil otherwise → stays NULL, falls back to first_seen_at).
		createdAt := eventTimeFromMeta(req.Metadata)

		// Upsert graph.nodes. Track outcome.
		outcome, upsertErr := upsertNodeOutcome(ctx, deps, nodeID, string(nodeType), naturalKey, req.CanonicalURL, "", req.Body, bodyTS, createdAt, authorPersonID, scope, metaJSON)
		if upsertErr != nil {
			deps.Logger.Error().Err(upsertErr).Str("node_id", nodeID).Msg("ingest_content: upsert node failed")
			writeError(w, http.StatusInternalServerError, "upsert node: "+upsertErr.Error())
			return
		}

		// Upsert graph.artifact_bodies.
		_, abErr := deps.DB.Exec(ctx, `
			INSERT INTO graph.artifact_bodies (node_id, body_full, fetched_at, machine_id)
			VALUES ($1, $2, NOW(), $3)
			ON CONFLICT (node_id) DO UPDATE SET
				body_full  = EXCLUDED.body_full,
				fetched_at = NOW()`,
			nodeID, req.Body, deps.MachineID,
		)
		if abErr != nil {
			deps.Logger.Warn().Err(abErr).Msg("ingest_content: upsert artifact_bodies failed")
		}

		// Extract findings and reconcile edges.
		var edgesCreated int
		var extractedURLs, extractedEntities, extractedJira, extractedGHPRs, extractedPeople []string

		if deps.Extractor != nil {
			extractResult, extErr := deps.Extractor.Extract(ctx, req.Body)
			if extErr != nil {
				deps.Logger.Warn().Err(extErr).Msg("ingest_content: extractor failed; skipping edge reconciliation")
			} else {
				upsertedEdgeIDs, edgeErr := reconcileEdges(ctx, deps, nodeID, extractResult.Findings)
				if edgeErr != nil {
					deps.Logger.Warn().Err(edgeErr).Msg("ingest_content: reconcileEdges failed")
				} else {
					edgesCreated = len(upsertedEdgeIDs)
					if err := pruneStaleEdges(ctx, deps, nodeID, upsertedEdgeIDs); err != nil {
						deps.Logger.Warn().Err(err).Msg("ingest_content: pruneStaleEdges failed")
					}
				}

				extractedURLs = extractResult.URLs
				extractedEntities = extractResult.Entities
				extractedJira = extractResult.JiraKeys
				extractedGHPRs = extractResult.GHPRs
			}
		}

		// Collect mention people refs.
		for _, m := range req.Metadata.Mentions {
			if m.Ref != "" {
				extractedPeople = append(extractedPeople, m.Ref)
			}
		}

		// Process file attachments.
		var attachmentsRegistered []attachmentRegisteredView
		var enqueuedJobs []jobEnqueuedView

		for _, f := range req.Metadata.Files {
			attNodeID := ids.SlackFile(f.ID)
			attNK, _ := ids.ParseNaturalKey(attNodeID)
			attType, _ := ids.ParseType(attNodeID)

			_, attErr := deps.DB.Exec(ctx, `
				INSERT INTO graph.nodes (id, type, natural_key, url, updated_at, machine_id)
				VALUES ($1, $2, $3, $4, NOW(), $5)
				ON CONFLICT (id) DO NOTHING`,
				attNodeID, string(attType), attNK, f.URLPrivate, deps.MachineID,
			)
			if attErr != nil {
				deps.Logger.Warn().Err(attErr).Str("att_node_id", attNodeID).Msg("ingest_content: upsert attachment node failed")
				continue
			}

			_, edgeErr := deps.DB.Exec(ctx, `
				INSERT INTO graph.edges (from_node_id, to_node_id, kind, source_msg_id, machine_id)
				VALUES ($1, $2, 'REFERENCES', $3, $4)
				ON CONFLICT (from_node_id, to_node_id, kind) DO UPDATE SET
					source_msg_id = EXCLUDED.source_msg_id`,
				nodeID, attNodeID, nodeID, deps.MachineID,
			)
			if edgeErr != nil {
				deps.Logger.Warn().Err(edgeErr).Str("att_node_id", attNodeID).Msg("ingest_content: upsert attachment edge failed")
			}

			// Enqueue describe_attachment job.
			descPayload := map[string]string{
				"node_id":      attNodeID,
				"external_url": f.URLPrivate,
				"mime":         f.MimeType,
				"source":       req.Source,
			}
			jid, jErr := jobs.Enqueue(ctx, deps.DB, "describe_attachment", descPayload, jobs.EnqueueOptions{
				Priority:  5,
				MachineID: deps.MachineID,
			})
			if jErr != nil {
				deps.Logger.Warn().Err(jErr).Str("att_node_id", attNodeID).Msg("ingest_content: enqueue describe_attachment failed")
			} else {
				attachmentsRegistered = append(attachmentsRegistered, attachmentRegisteredView{
					NodeID:  attNodeID,
					Outcome: "queued_for_describe",
				})
				enqueuedJobs = append(enqueuedJobs, jobEnqueuedView{
					ID:       jid,
					Type:     "describe_attachment",
					Priority: 5,
				})
			}
		}

		// Enqueue fetch_body (priority 0) if outcome is not unchanged.
		if outcome != "unchanged" {
			fbPayload := map[string]string{
				"node_id": nodeID,
				"source":  req.Source,
			}
			fbID, fbErr := jobs.Enqueue(ctx, deps.DB, "fetch_body", fbPayload, jobs.EnqueueOptions{
				Priority:  0,
				MachineID: deps.MachineID,
			})
			if fbErr != nil {
				deps.Logger.Warn().Err(fbErr).Str("node_id", nodeID).Msg("ingest_content: enqueue fetch_body failed")
			} else {
				enqueuedJobs = append(enqueuedJobs, jobEnqueuedView{
					ID:       fbID,
					Type:     "fetch_body",
					Priority: 0,
				})
			}
		}

		// If this is a thread reply and we don't have the thread's root yet, fetch
		// the whole thread once (root + all replies) so the thread is complete.
		if req.Source == "slack" && req.Metadata.ThreadTs != "" && req.Metadata.ThreadTs != req.Metadata.Ts {
			rootID := ids.SlackMessage(req.Metadata.ChannelID, req.Metadata.ThreadTs)
			var rootExists bool
			_ = deps.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM graph.nodes WHERE id=$1)`, rootID).Scan(&rootExists)
			if !rootExists {
				btID, btErr := jobs.Enqueue(ctx, deps.DB, "backfill_slack_thread", map[string]string{
					"channel_id": req.Metadata.ChannelID,
					"thread_ts":  req.Metadata.ThreadTs,
				}, jobs.EnqueueOptions{Priority: 5, TargetRunner: "vps"})
				if btErr != nil {
					deps.Logger.Warn().Err(btErr).Msg("ingest_content: enqueue backfill_slack_thread failed")
				} else {
					enqueuedJobs = append(enqueuedJobs, jobEnqueuedView{ID: btID, Type: "backfill_slack_thread", Priority: 5})
				}
			}
		}

		// Keep the thread's topic summary fresh (background; deduped). Covers both
		// replies (thread_ts = root) and roots-with-replies (thread_ts = own ts).
		if req.Source == "slack" && req.Metadata.ThreadTs != "" && outcome != "unchanged" {
			enqueueSummarizeThread(ctx, deps.DB, req.Metadata.ChannelID, req.Metadata.ThreadTs)
		}

		// Derive a feature entity from newly-ingested/updated Jira tickets so the
		// extractor can auto-link Slack messages mentioning the feature.
		if req.Source == "jira" && outcome != "unchanged" {
			enqueueDeriveFeatureEntity(ctx, deps.DB, nodeID)
		}

		// Enqueue resolve_identity if author has no email yet.
		if authorPersonID != nil && deps.Identity != nil {
			var emailVal *string
			_ = deps.DB.QueryRow(ctx, `SELECT email FROM graph.people WHERE id = $1`, *authorPersonID).Scan(&emailVal)
			if emailVal == nil {
				authorSource, authorExternalID := parseRef(req.Metadata.Author.Ref)
				riPayload := map[string]any{
					"person_id":   *authorPersonID,
					"source":      authorSource,
					"external_id": authorExternalID,
				}
				riID, riErr := jobs.Enqueue(ctx, deps.DB, "resolve_identity", riPayload, jobs.EnqueueOptions{
					Priority:  5,
					MachineID: deps.MachineID,
				})
				if riErr != nil {
					deps.Logger.Warn().Err(riErr).Msg("ingest_content: enqueue resolve_identity failed")
				} else {
					enqueuedJobs = append(enqueuedJobs, jobEnqueuedView{
						ID:       riID,
						Type:     "resolve_identity",
						Priority: 5,
					})
				}
			}
		}

		resp := ingestResponse{
			NodeID:  nodeID,
			Outcome: outcome,
			Extracted: extractedView{
				URLs:     nullStrSlice(extractedURLs),
				Entities: nullStrSlice(extractedEntities),
				JiraKeys: nullStrSlice(extractedJira),
				GHPRs:    nullStrSlice(extractedGHPRs),
				People:   nullStrSlice(extractedPeople),
			},
			EdgesCreated:          edgesCreated,
			AttachmentsRegistered: attachmentsRegistered,
			JobsEnqueued:          enqueuedJobs,
		}
		writeJSON(w, http.StatusOK, resp)
	})
}

// upsertNodeOutcome upserts into graph.nodes and returns "created", "updated", or "unchanged".
// eventTimeFromMeta returns the source artifact's creation/event time from the
// ingest metadata. Slack sends a float epoch ts ("1782355843.155339"); other
// sources may send RFC3339. Returns nil when unknown so created_at stays NULL.
func eventTimeFromMeta(m ingestContentMetadata) *time.Time {
	if m.Ts == "" {
		return nil
	}
	if f, err := strconv.ParseFloat(m.Ts, 64); err == nil && f > 0 {
		sec := int64(f)
		t := time.Unix(sec, int64((f-float64(sec))*1e9)).UTC()
		return &t
	}
	if t, err := time.Parse(time.RFC3339, m.Ts); err == nil {
		return &t
	}
	return nil
}

func upsertNodeOutcome(
	ctx context.Context,
	deps Deps,
	nodeID, nodeType, naturalKey, url, title, body string,
	bodyTS time.Time,
	createdAt *time.Time,
	authorPersonID *int64,
	scope string,
	metaJSON []byte,
) (string, error) {
	// Check existing body_ts to determine outcome.
	var existingBodyTS *time.Time
	err := deps.DB.QueryRow(ctx,
		`SELECT body_ts FROM graph.nodes WHERE id = $1`, nodeID,
	).Scan(&existingBodyTS)

	isNew := (err != nil) // pgx.ErrNoRows or missing — treat as new

	_, execErr := deps.DB.Exec(ctx, `
		INSERT INTO graph.nodes
			(id, type, natural_key, url, title, body, body_revision, body_ts,
			 created_at, author_person_id, scope, metadata, updated_at, machine_id)
		VALUES
			($1, $2, $3, $4, $5, $6, 1, $7,
			 $8, $9, $10, $11, NOW(), $12)
		ON CONFLICT (id) DO UPDATE SET
			url              = EXCLUDED.url,
			title            = COALESCE(NULLIF(EXCLUDED.title,''), graph.nodes.title),
			body             = EXCLUDED.body,
			body_revision    = graph.nodes.body_revision + 1,
			body_ts          = EXCLUDED.body_ts,
			created_at       = COALESCE(graph.nodes.created_at, EXCLUDED.created_at),
			author_person_id = COALESCE(EXCLUDED.author_person_id, graph.nodes.author_person_id),
			scope            = EXCLUDED.scope,
			metadata         = EXCLUDED.metadata,
			updated_at       = NOW(),
			machine_id       = EXCLUDED.machine_id
		WHERE EXCLUDED.body_ts >= graph.nodes.body_ts`,
		nodeID,
		nodeType,
		naturalKey,
		url,
		title,
		body,
		bodyTS,
		createdAt,
		authorPersonID,
		scope,
		metaJSON,
		deps.MachineID,
	)
	if execErr != nil {
		return "", execErr
	}

	if isNew {
		return "created", nil
	}
	if existingBodyTS != nil && !bodyTS.After(*existingBodyTS) {
		return "unchanged", nil
	}
	return "updated", nil
}

// buildNodeID derives the canonical node ID from source + canonical_url + metadata.
func buildNodeID(source, canonicalURL string, meta ingestContentMetadata) (string, error) {
	switch source {
	case "slack":
		ch := meta.ChannelID
		ts := meta.Ts
		if ch == "" || ts == "" {
			return "", fmt.Errorf("slack: channel_id and ts are required in metadata")
		}
		return ids.SlackMessage(ch, ts), nil

	case "jira":
		key := meta.Key
		if key == "" {
			key = extractJiraKey(canonicalURL)
		}
		if key == "" {
			return "", fmt.Errorf("jira: key required in metadata or canonical_url")
		}
		return ids.Jira(key)

	case "github":
		repo := meta.Repo
		num := meta.Number
		if repo == "" || num == 0 {
			return "", fmt.Errorf("github: repo and number required in metadata")
		}
		return ids.GHPR(repo, num)

	case "confluence":
		if meta.PageID != 0 {
			return ids.CFPage(meta.PageID), nil
		}
		return "", fmt.Errorf("confluence: page_id required in metadata")

	case "pagerduty":
		id := meta.IncidentID
		if id == "" {
			return "", fmt.Errorf("pagerduty: incident_id required in metadata")
		}
		return ids.PagerDuty(id)

	case "datadog":
		if meta.ObjectType == "" || meta.ObjectID == 0 {
			return "", fmt.Errorf("datadog: object_type and object_id required in metadata")
		}
		return ids.Datadog(meta.ObjectType, meta.ObjectID)

	case "sentry":
		id := meta.IssueID
		if id == "" {
			return "", fmt.Errorf("sentry: issue_id required in metadata")
		}
		return ids.Sentry(id)

	case "gws":
		id := meta.DriveID
		if id == "" {
			return "", fmt.Errorf("gws: drive_id required in metadata")
		}
		return ids.GWSDoc(id), nil

	default:
		return "", fmt.Errorf("unknown source %q", source)
	}
}

// extractJiraKey parses a Jira key from a URL like .../browse/PAY-2128.
func extractJiraKey(u string) string {
	const browse = "/browse/"
	idx := strings.Index(u, browse)
	if idx < 0 {
		return ""
	}
	rest := u[idx+len(browse):]
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

// buildScope derives the scope string from the source and ingest metadata.
func buildScope(source string, meta ingestContentMetadata) string {
	if meta.Scope != "" {
		return meta.Scope
	}
	switch source {
	case "slack":
		if meta.ChannelID != "" {
			return "slack:" + meta.ChannelID
		}
	case "jira":
		if meta.ProjectKey != "" {
			return "jira:" + meta.ProjectKey
		}
	case "github":
		if meta.Repo != "" {
			return "github:" + meta.Repo
		}
	case "confluence":
		if meta.SpaceKey != "" {
			return "confluence:" + meta.SpaceKey
		}
	}
	return source
}

// parseRef splits a ref like "slack_uid:UUK3WPNNQ" into (source, externalID).
func parseRef(ref string) (source, externalID string) {
	colon := strings.Index(ref, ":")
	if colon < 0 {
		return "", ref
	}
	tag := ref[:colon]
	ext := ref[colon+1:]
	switch tag {
	case "slack_uid":
		return "slack", ext
	case "jira_uid":
		return "jira", ext
	case "gh_login":
		return "github", ext
	default:
		return tag, ext
	}
}

// nullStrSlice returns an empty slice (not nil) when s is nil, so JSON encodes as [].
func nullStrSlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
