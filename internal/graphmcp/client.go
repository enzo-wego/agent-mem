package graphmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxResponseBytes  = 8 << 20
	maxErrorBodyBytes = 4 << 10
)

// Client proxies graph requests to a running agent-mem worker.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// ResolveRequest is the worker payload for /api/graph/resolve.
type ResolveRequest struct {
	Seeds         []string `json:"seeds"`
	Query         string   `json:"query"`
	Depth         int      `json:"depth"`
	BudgetTokens  int      `json:"budget_tokens"`
	IncludeBodies bool     `json:"include_bodies"`
}

// NewClient creates a worker API client.
func NewClient(rawBaseURL, apiKey string, httpClient *http.Client) (*Client, error) {
	rawBaseURL = strings.TrimSpace(rawBaseURL)
	parsed, err := url.Parse(rawBaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("invalid worker URL %q", rawBaseURL)
	}

	if httpClient == nil {
		httpClient = &http.Client{Timeout: 90 * time.Second}
	} else if httpClient.Timeout == 0 {
		cloned := *httpClient
		cloned.Timeout = 90 * time.Second
		httpClient = &cloned
	}

	return &Client{
		baseURL:    strings.TrimRight(rawBaseURL, "/"),
		apiKey:     apiKey,
		httpClient: httpClient,
	}, nil
}

// Search searches graph nodes.
func (c *Client) Search(ctx context.Context, query string, types []string, limit int) (map[string]any, error) {
	values := url.Values{
		"q":     []string{query},
		"limit": []string{strconv.Itoa(limit)},
	}
	if len(types) > 0 {
		values.Set("types", strings.Join(types, ","))
	}
	return c.doJSON(ctx, http.MethodGet, "/api/graph/search", values, nil)
}

// Node fetches a graph node by ID or URL.
func (c *Client) Node(ctx context.Context, id, rawURL string) (map[string]any, error) {
	values := url.Values{}
	if id != "" {
		values.Set("id", id)
	} else {
		values.Set("url", rawURL)
	}
	return c.doJSON(ctx, http.MethodGet, "/api/graph/node", values, nil)
}

// Neighbors traverses neighbors from a graph node.
func (c *Client) Neighbors(ctx context.Context, id string, depth int, kinds []string) (map[string]any, error) {
	values := url.Values{"depth": []string{strconv.Itoa(depth)}}
	for _, kind := range kinds {
		values.Add("kind", kind)
	}
	path := "/api/graph/node/" + url.PathEscape(id) + "/neighbors"
	return c.doJSON(ctx, http.MethodGet, path, values, nil)
}

// ClusterSummary returns a generated summary around a graph node.
func (c *Client) ClusterSummary(ctx context.Context, node string, depth int) (map[string]any, error) {
	values := url.Values{
		"node":  []string{node},
		"depth": []string{strconv.Itoa(depth)},
	}
	return c.doJSON(ctx, http.MethodGet, "/api/graph/cluster/summary", values, nil)
}

// Person looks up a person profile by name, email, employee id, or Slack id.
func (c *Client) Person(ctx context.Context, query string, limit int) (map[string]any, error) {
	values := url.Values{
		"q":     []string{query},
		"limit": []string{strconv.Itoa(limit)},
	}
	return c.doJSON(ctx, http.MethodGet, "/api/graph/person", values, nil)
}

// Resolve resolves a set of graph seeds into a bounded context bundle.
func (c *Client) Resolve(ctx context.Context, request ResolveRequest) (map[string]any, error) {
	return c.doJSON(ctx, http.MethodPost, "/api/graph/resolve", nil, request)
}

// Probe verifies that the worker API is reachable and authorized.
func (c *Client) Probe(ctx context.Context) error {
	_, err := c.doJSON(ctx, http.MethodGet, "/api/settings", nil, nil)
	return err
}

func (c *Client) doJSON(
	ctx context.Context,
	method string,
	path string,
	query url.Values,
	body any,
) (map[string]any, error) {
	var requestBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode worker request: %w", err)
		}
		requestBody = bytes.NewReader(encoded)
	}

	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, requestBody)
	if err != nil {
		return nil, fmt.Errorf("create worker request: %w", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call worker: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		message, readErr := io.ReadAll(io.LimitReader(response.Body, maxErrorBodyBytes+1))
		if readErr != nil {
			return nil, fmt.Errorf("worker returned %s", response.Status)
		}
		if len(message) > maxErrorBodyBytes {
			message = message[:maxErrorBodyBytes]
		}
		return nil, fmt.Errorf("worker returned %s: %s", response.Status, strings.TrimSpace(string(message)))
	}

	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read worker response: %w", err)
	}
	if len(payload) > maxResponseBytes {
		return nil, fmt.Errorf("worker response exceeds %d bytes", maxResponseBytes)
	}

	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, fmt.Errorf("decode worker response: %w", err)
	}
	return decoded, nil
}
