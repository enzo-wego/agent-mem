package graphmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	asker      string
}

// ErrAskerRequired is returned by the graph read methods when no asker is set.
var ErrAskerRequired = errors.New("agent-mem MCP needs AGENT_MEM_ASKER (Slack user id) set by the caller")

// SetAsker sets the Slack user id sent as X-Asker-User on graph reads. The
// client never sends the admin header.
func (c *Client) SetAsker(asker string) { c.asker = strings.TrimSpace(asker) }

// askerHeaders returns the per-request headers for a filtered read, or
// ErrAskerRequired without touching the network when no asker is set.
func (c *Client) askerHeaders() (http.Header, error) {
	if c.asker == "" {
		return nil, ErrAskerRequired
	}
	return http.Header{"X-Asker-User": []string{c.asker}}, nil
}

// ResolveRequest is the worker payload for /api/graph/resolve.
type ResolveRequest struct {
	Seeds         []string `json:"seeds"`
	Query         string   `json:"query"`
	Depth         int      `json:"depth"`
	BudgetTokens  int      `json:"budget_tokens"`
	IncludeBodies bool     `json:"include_bodies"`
	Epic          []string `json:"epic,omitempty"`
	Business      string   `json:"business,omitempty"`
}

// SearchRequest is the query for /api/graph/search.
type SearchRequest struct {
	Q        string
	Types    []string
	Limit    int
	Epic     []string
	Business string
	Since    string
	Until    string
	Arms     []string
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
func (c *Client) Search(ctx context.Context, req SearchRequest) (map[string]any, error) {
	headers, err := c.askerHeaders()
	if err != nil {
		return nil, err
	}
	values := url.Values{
		"q":     []string{req.Q},
		"limit": []string{strconv.Itoa(req.Limit)},
	}
	if len(req.Types) > 0 {
		values.Set("types", strings.Join(req.Types, ","))
	}
	for _, e := range req.Epic {
		values.Add("epic", e)
	}
	if req.Business != "" {
		values.Set("business", req.Business)
	}
	if req.Since != "" {
		values.Set("since", req.Since)
	}
	if req.Until != "" {
		values.Set("until", req.Until)
	}
	if len(req.Arms) > 0 {
		values.Set("arms", strings.Join(req.Arms, ","))
	}
	return c.doJSON(ctx, http.MethodGet, "/api/graph/search", values, nil, headers)
}

// Epic returns an epic's (or the business root's) members and window.
func (c *Client) Epic(ctx context.Context, key string) (map[string]any, error) {
	headers, err := c.askerHeaders()
	if err != nil {
		return nil, err
	}
	return c.doJSON(ctx, http.MethodGet, "/api/graph/epic/"+url.PathEscape(key), nil, nil, headers)
}

// Node fetches a graph node by ID or URL.
func (c *Client) Node(ctx context.Context, id, rawURL string) (map[string]any, error) {
	headers, err := c.askerHeaders()
	if err != nil {
		return nil, err
	}
	values := url.Values{}
	if id != "" {
		values.Set("id", id)
	} else {
		values.Set("url", rawURL)
	}
	return c.doJSON(ctx, http.MethodGet, "/api/graph/node", values, nil, headers)
}

// Neighbors traverses neighbors from a graph node.
func (c *Client) Neighbors(ctx context.Context, id string, depth int, kinds []string) (map[string]any, error) {
	headers, err := c.askerHeaders()
	if err != nil {
		return nil, err
	}
	values := url.Values{"depth": []string{strconv.Itoa(depth)}}
	for _, kind := range kinds {
		values.Add("kind", kind)
	}
	path := "/api/graph/node/" + url.PathEscape(id) + "/neighbors"
	return c.doJSON(ctx, http.MethodGet, path, values, nil, headers)
}

// Resolve resolves a set of graph seeds into a bounded context bundle.
func (c *Client) Resolve(ctx context.Context, request ResolveRequest) (map[string]any, error) {
	headers, err := c.askerHeaders()
	if err != nil {
		return nil, err
	}
	return c.doJSON(ctx, http.MethodPost, "/api/graph/resolve", nil, request, headers)
}

// Probe verifies that the worker API is reachable and authorized.
func (c *Client) Probe(ctx context.Context) error {
	_, err := c.doJSON(ctx, http.MethodGet, "/api/settings", nil, nil, nil)
	return err
}

func (c *Client) doJSON(
	ctx context.Context,
	method string,
	path string,
	query url.Values,
	body any,
	headers http.Header,
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
	request.Header.Set("X-Graph-Caller", "mcp")
	for key, values := range headers {
		request.Header[key] = values
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
