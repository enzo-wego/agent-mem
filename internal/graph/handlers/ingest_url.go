package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/ids"
	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// ingestURLRequest is the request body for POST /api/graph/ingest/url.
type ingestURLRequest struct {
	URL       string `json:"url"`
	ScopeHint string `json:"scope_hint"`
}

// ingestURLResponse is the response body for POST /api/graph/ingest/url.
type ingestURLResponse struct {
	NodeID       string            `json:"node_id"`
	Outcome      string            `json:"outcome"`
	JobsEnqueued []jobEnqueuedView `json:"jobs_enqueued,omitempty"`
}

// defaultBodyTTL is the freshness window used when no source-specific setting exists.
const defaultBodyTTL = time.Hour

// NewIngestURLHandler returns an http.Handler for POST /api/graph/ingest/url.
func NewIngestURLHandler(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ingestURLRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}

		if req.URL == "" {
			writeError(w, http.StatusBadRequest, "url is required")
			return
		}

		ctx := r.Context()

		// Find the fetcher that claims the URL.
		fetcher, ok := deps.Fetchers.For(req.URL)
		if !ok {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("no fetcher available for URL %q", req.URL))
			return
		}

		// Derive canonical node_id from the URL.
		nodeID := nodeIDFromURL(req.URL, fetcher.Source())
		if nodeID == "" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("cannot derive node_id from URL %q", req.URL))
			return
		}

		nodeType, _ := ids.ParseType(nodeID)
		naturalKey, _ := ids.ParseNaturalKey(nodeID)

		// Upsert a placeholder graph.nodes row (ignore conflict — row may already exist).
		_, err := deps.DB.Exec(ctx, `
			INSERT INTO graph.nodes (id, type, natural_key, url, body_revision, updated_at, machine_id)
			VALUES ($1, $2, $3, $4, 0, NOW(), $5)
			ON CONFLICT (id) DO NOTHING`,
			nodeID, string(nodeType), naturalKey, req.URL, deps.MachineID,
		)
		if err != nil {
			deps.Logger.Error().Err(err).Str("node_id", nodeID).Msg("ingest_url: upsert placeholder node failed")
			writeError(w, http.StatusInternalServerError, "upsert node: "+err.Error())
			return
		}

		// Check freshness: if artifact_bodies.fetched_at is within TTL → already_fresh.
		ttl := bodyTTLForSource(ctx, deps, fetcher.Source())
		var fetchedAt *time.Time
		_ = deps.DB.QueryRow(ctx,
			`SELECT fetched_at FROM graph.artifact_bodies WHERE node_id = $1`, nodeID,
		).Scan(&fetchedAt)

		if fetchedAt != nil && time.Since(*fetchedAt) < ttl {
			resp := ingestURLResponse{
				NodeID:  nodeID,
				Outcome: "already_fresh",
			}
			writeJSON(w, http.StatusAccepted, resp)
			return
		}

		// Enqueue fetch_body (priority 0) and index_artifact (priority 5).
		var enqueuedJobs []jobEnqueuedView

		fbID, fbErr := jobs.Enqueue(ctx, deps.DB, "fetch_body", map[string]string{
			"node_id": nodeID,
			"url":     req.URL,
			"source":  fetcher.Source(),
		}, jobs.EnqueueOptions{
			Priority:  0,
			MachineID: deps.MachineID,
		})
		if fbErr != nil {
			deps.Logger.Warn().Err(fbErr).Str("node_id", nodeID).Msg("ingest_url: enqueue fetch_body failed")
		} else {
			enqueuedJobs = append(enqueuedJobs, jobEnqueuedView{
				ID:       fbID,
				Type:     "fetch_body",
				Priority: 0,
			})
		}

		iaID, iaErr := jobs.Enqueue(ctx, deps.DB, "index_artifact", map[string]any{
			"node_id": nodeID,
			"force":   false,
		}, jobs.EnqueueOptions{
			Priority:  5,
			MachineID: deps.MachineID,
		})
		if iaErr != nil {
			deps.Logger.Warn().Err(iaErr).Str("node_id", nodeID).Msg("ingest_url: enqueue index_artifact failed")
		} else {
			enqueuedJobs = append(enqueuedJobs, jobEnqueuedView{
				ID:       iaID,
				Type:     "index_artifact",
				Priority: 5,
			})
		}

		resp := ingestURLResponse{
			NodeID:       nodeID,
			Outcome:      "queued_for_fetch",
			JobsEnqueued: enqueuedJobs,
		}
		writeJSON(w, http.StatusAccepted, resp)
	})
}

// nodeIDFromURL derives the canonical node ID for a URL given the fetcher source.
func nodeIDFromURL(rawURL, source string) string {
	switch source {
	case "slack":
		m := slackURLPattern.FindStringSubmatch(rawURL)
		if m != nil {
			channel := m[1]
			ts := slackTSFromRawP(m[2])
			return ids.SlackMessage(channel, ts)
		}
	case "jira":
		key := extractJiraKey(rawURL)
		if key != "" {
			nodeID, err := ids.Jira(key)
			if err == nil {
				return nodeID
			}
		}
	case "github":
		m := ghPRURLPattern.FindStringSubmatch(rawURL)
		if m != nil {
			repo := m[1]
			var num int
			fmt.Sscanf(m[2], "%d", &num)
			nodeID, err := ids.GHPR(repo, num)
			if err == nil {
				return nodeID
			}
		}
	case "confluence":
		m := cfPageURLPattern.FindStringSubmatch(rawURL)
		if m != nil {
			var id int64
			fmt.Sscanf(m[1], "%d", &id)
			if id > 0 {
				return ids.CFPage(id)
			}
		}
	case "pagerduty":
		m := pdIncidentURLPattern.FindStringSubmatch(rawURL)
		if m != nil {
			nodeID, err := ids.PagerDuty(m[1])
			if err == nil {
				return nodeID
			}
		}
	case "datadog":
		m := ddMonitorURLPattern.FindStringSubmatch(rawURL)
		if m != nil {
			var id int64
			fmt.Sscanf(m[1], "%d", &id)
			nodeID, err := ids.Datadog("monitor", id)
			if err == nil {
				return nodeID
			}
		}
	case "sentry":
		m := sentryIssueURLPattern.FindStringSubmatch(rawURL)
		if m != nil {
			nodeID, err := ids.Sentry(m[1])
			if err == nil {
				return nodeID
			}
		}
	case "gws":
		m := gwsDocURLPattern.FindStringSubmatch(rawURL)
		if m != nil {
			return ids.GWSDoc(m[1])
		}
	}
	return ""
}

// bodyTTLForSource returns the freshness TTL for a source, checking the
// graph.settings table for "graph.body_ttl.<source>". Falls back to 1h.
func bodyTTLForSource(ctx context.Context, deps Deps, source string) time.Duration {
	key := "graph.body_ttl." + source
	var val *string
	err := deps.DB.QueryRow(ctx, `SELECT value FROM graph.settings WHERE key = $1`, key).Scan(&val)
	if err != nil || val == nil {
		return defaultBodyTTL
	}
	d, parseErr := time.ParseDuration(*val)
	if parseErr != nil {
		return defaultBodyTTL
	}
	return d
}

// slackTSFromRawP converts a raw p-timestamp (no dot) to dotted format.
func slackTSFromRawP(raw string) string {
	if len(raw) <= 6 {
		return raw
	}
	return raw[:len(raw)-6] + "." + raw[len(raw)-6:]
}
