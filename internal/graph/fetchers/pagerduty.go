package fetchers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/ids"
)

var (
	pdNodeRe = regexp.MustCompile(`^pagerduty:([A-Z0-9]+)$`)
	pdURLRe  = regexp.MustCompile(`\bpagerduty\.com/incidents/([A-Z0-9]+)\b`)
)

// pagerDutyFetcher retrieves PagerDuty incident bodies via the REST API.
type pagerDutyFetcher struct {
	cfg Config
	log zerolog.Logger
}

func newPagerDutyFetcher(cfg Config, log zerolog.Logger) *pagerDutyFetcher {
	return &pagerDutyFetcher{cfg: cfg, log: log}
}

func (f *pagerDutyFetcher) Source() string { return "pagerduty" }

// Matches returns true for pagerduty:<ID> node IDs or PagerDuty incident URLs.
func (f *pagerDutyFetcher) Matches(nodeIDorURL string) bool {
	if pdNodeRe.MatchString(nodeIDorURL) {
		return true
	}
	return pdURLRe.MatchString(nodeIDorURL)
}

type pdIncidentResponse struct {
	Incident pdIncident `json:"incident"`
}

type pdIncident struct {
	ID          string           `json:"id"`
	Title       string           `json:"title"`
	HTMLUrl     string           `json:"html_url"`
	LastStatusChangeAt string   `json:"last_status_change_at"`
	FirstTriggerLogEntry *pdLogEntry `json:"first_trigger_log_entry"`
}

type pdLogEntry struct {
	Agent *pdAgent `json:"agent"`
}

type pdAgent struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Fetch retrieves the PagerDuty incident.
func (f *pagerDutyFetcher) Fetch(ctx context.Context, node string) (FetchedBody, error) {
	incidentID, err := f.parseNode(node)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("pagerduty fetcher: %w", err)
	}

	base := strings.TrimRight(f.cfg.PagerDutyBaseURL, "/")
	apiURL := fmt.Sprintf("%s/incidents/%s", base, incidentID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("pagerduty fetcher: build request: %w", err)
	}
	req.Header.Set("Authorization", "Token token="+f.cfg.PagerDutyToken)
	req.Header.Set("Accept", "application/vnd.pagerduty+json;version=2")

	resp, err := f.cfg.HTTPClient.Do(req)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("pagerduty fetcher: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return FetchedBody{}, fmt.Errorf("pagerduty fetcher status %d: %s", resp.StatusCode, string(body))
	}

	rawBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("pagerduty fetcher: read body: %w", err)
	}

	var incResp pdIncidentResponse
	if err := json.Unmarshal(rawBytes, &incResp); err != nil {
		return FetchedBody{}, fmt.Errorf("pagerduty fetcher: decode response: %w", err)
	}

	inc := incResp.Incident
	bodyTS := time.Time{}
	if inc.LastStatusChangeAt != "" {
		if t, err := time.Parse(time.RFC3339, inc.LastStatusChangeAt); err == nil {
			bodyTS = t
		}
	}

	var author AuthorRef
	if inc.FirstTriggerLogEntry != nil && inc.FirstTriggerLogEntry.Agent != nil {
		a := inc.FirstTriggerLogEntry.Agent
		author = AuthorRef{
			Source:      "pagerduty",
			ExternalID:  a.ID,
			DisplayName: a.Name,
			Email:       a.Email,
		}
	}

	nodeID, _ := ids.PagerDuty(incidentID)
	return FetchedBody{
		NodeID:      nodeID,
		Type:        ids.TypePagerDuty,
		URL:         inc.HTMLUrl,
		Title:       inc.Title,
		Raw:         rawBytes,
		ContentType: "application/json",
		Author:      author,
		BodyTS:      bodyTS,
	}, nil
}

// parseNode extracts the incident ID from a node ID or PagerDuty URL.
func (f *pagerDutyFetcher) parseNode(node string) (string, error) {
	if m := pdNodeRe.FindStringSubmatch(node); m != nil {
		return m[1], nil
	}
	if m := pdURLRe.FindStringSubmatch(node); m != nil {
		return m[1], nil
	}
	return "", fmt.Errorf("cannot parse pagerduty node %q", node)
}
