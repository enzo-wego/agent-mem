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
	// datadog:monitor:133274814
	ddNodeRe = regexp.MustCompile(`^datadog:(monitor|dashboard|log):(\d+)$`)
	ddURLRe  = regexp.MustCompile(`\bapp\.datadoghq\.com/monitors/(\d+)\b`)
)

// datadogFetcher retrieves Datadog monitor bodies via the REST API.
type datadogFetcher struct {
	cfg Config
	log zerolog.Logger
}

func newDatadogFetcher(cfg Config, log zerolog.Logger) *datadogFetcher {
	return &datadogFetcher{cfg: cfg, log: log}
}

func (f *datadogFetcher) Source() string { return "datadog" }

// Matches returns true for datadog:<type>:<id> node IDs or Datadog monitor URLs.
func (f *datadogFetcher) Matches(nodeIDorURL string) bool {
	if ddNodeRe.MatchString(nodeIDorURL) {
		return true
	}
	return ddURLRe.MatchString(nodeIDorURL)
}

type ddMonitorResponse struct {
	ID       int64     `json:"id"`
	Name     string    `json:"name"`
	Modified time.Time `json:"modified"`
	Creator  *ddCreator `json:"creator"`
}

type ddCreator struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Fetch retrieves the Datadog monitor.
func (f *datadogFetcher) Fetch(ctx context.Context, node string) (FetchedBody, error) {
	objectType, monitorID, err := f.parseNode(node)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("datadog fetcher: %w", err)
	}

	base := strings.TrimRight(f.cfg.DatadogBaseURL, "/")
	// v1 supports /api/v1/monitor/<id>; only "monitor" type is currently supported here.
	apiURL := fmt.Sprintf("%s/api/v1/monitor/%s", base, monitorID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("datadog fetcher: build request: %w", err)
	}
	req.Header.Set("DD-API-KEY", f.cfg.DatadogAPIKey)
	req.Header.Set("DD-APPLICATION-KEY", f.cfg.DatadogAppKey)

	resp, err := f.cfg.HTTPClient.Do(req)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("datadog fetcher: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return FetchedBody{}, fmt.Errorf("datadog fetcher status %d: %s", resp.StatusCode, string(body))
	}

	rawBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("datadog fetcher: read body: %w", err)
	}

	var monitor ddMonitorResponse
	if err := json.Unmarshal(rawBytes, &monitor); err != nil {
		return FetchedBody{}, fmt.Errorf("datadog fetcher: decode response: %w", err)
	}

	var author AuthorRef
	if monitor.Creator != nil {
		author = AuthorRef{
			Source:      "datadog",
			DisplayName: monitor.Creator.Name,
			Email:       monitor.Creator.Email,
		}
	}

	nodeID, _ := ids.Datadog(objectType, monitor.ID)
	return FetchedBody{
		NodeID:      nodeID,
		Type:        ids.TypeDatadog,
		URL:         fmt.Sprintf("https://app.datadoghq.com/monitors/%s", monitorID),
		Title:       monitor.Name,
		Raw:         rawBytes,
		ContentType: "application/json",
		Author:      author,
		BodyTS:      monitor.Modified,
	}, nil
}

// parseNode extracts the object type and ID from a node ID or Datadog URL.
func (f *datadogFetcher) parseNode(node string) (objectType, id string, err error) {
	if m := ddNodeRe.FindStringSubmatch(node); m != nil {
		return m[1], m[2], nil
	}
	if m := ddURLRe.FindStringSubmatch(node); m != nil {
		return "monitor", m[1], nil
	}
	return "", "", fmt.Errorf("cannot parse datadog node %q", node)
}
