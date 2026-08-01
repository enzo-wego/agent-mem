// Package llmgateway is an HTTP client for llm-gateway, the single egress point
// for every LLM call agent-mem makes. It implements the same surface as
// gemini.Client (generate, cheap generate, embed, describe), so it can stand in
// for that client wholesale rather than being wired call-site by call-site.
//
// Why route everything through one service: a metered API key has no spend
// ceiling, and a summarize_thread amplification bug once pushed ~$11/hour
// through one. Behind the gateway, generation runs on a Claude subscription seat
// that rate-limits instead of charging, and every call — whichever provider
// ultimately serves it — passes one place that can meter, alert and fail over.
//
// Callers ask for an intent tier ("summary" / "cheap"), never a model name, so
// model choice is a systemd restart on the gateway rather than a Go deploy here.
//
// Embeddings are the honest exception: Anthropic has no embeddings API, so the
// gateway proxies /embed straight to OpenRouter. Routing them here buys one key,
// one retry policy and one alerting path — not a cheaper provider.
package llmgateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/agent-mem/agent-mem/internal/gemini"
)

// RequestTimeout must sit ABOVE the gateway's own LLM_GATEWAY_CLAUDE_TIMEOUT_S
// (180s by default) and BELOW the job lease of every handler that calls us.
// The ordering is load-bearing:
//
//	gateway 180s  <  client 200s  <  lease 240s
//
// Too low and we cut off calls the gateway would have answered. Too high and the
// lease expires mid-flight, the janitor reclaims the job, and a second worker
// redoes the same work — duplicate LLM calls, which is precisely the
// amplification pattern that made this rewrite necessary. Change one, check all
// three; see handlers.SummaryLease.
const RequestTimeout = 200 * time.Second

// ErrUnreachable reports that the gateway could not be contacted at all, as
// opposed to answering with an error. The distinction decides whether work is
// retryable: a gateway that replies 400 will reply 400 forever, while a gateway
// that is down will come back. Callers that would otherwise mark a unit of work
// permanently failed must requeue on this one — see worker.isTransientLLMError.
var ErrUnreachable = errors.New("llm-gateway unreachable")

// StatusError is a non-200 answer from the gateway. It carries the code so
// callers can separate "try again later" (429, 503, 5xx) from "this will never
// work" (400, 401), instead of pattern-matching error strings.
type StatusError struct {
	Path string
	Code int
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("llm-gateway: %s returned %d: %s", e.Path, e.Code, e.Body)
}

// Retryable reports whether waiting could plausibly change the outcome.
//
// 429 and 503 are the ones that matter in practice: 503 is the seat's quota
// window, which reopens on a timer, and both mean the work is still valid. A
// 400 or 401 means the request or the key is wrong and will stay wrong.
func (e *StatusError) Retryable() bool {
	switch e.Code {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// IsRetryable reports whether err is worth retrying: the gateway was
// unreachable, or answered with a status that can change on its own.
//
// This is what stands between a gateway outage and permanent data loss.
// pending_messages has no retry path — MarkMessageFailed is terminal and
// ClaimPendingMessage only ever picks 'pending' — so treating a transient
// failure as permanent silently discards the observation.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrUnreachable) {
		return true
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Retryable()
	}
	return false
}

// Client talks to one llm-gateway instance. It satisfies the same method set as
// gemini.Client for the calls agent-mem makes, so it can replace that client
// rather than sit beside it.
type Client struct {
	baseURL string
	apiKey  string
	dims    int // default embedding dimensionality: 768 flat, 3072 graph
	http    *http.Client
}

// New returns a client for baseURL (e.g. "http://172.18.0.1:8750"). dims is the
// default embedding width for this caller and MUST match the destination column:
// observations.embedding is vector(768) while the graph uses halfvec(3072).
// Getting it wrong fails every insert with "expected 768 dimensions, not 3072",
// which looks like a partial outage rather than a config error.
func New(baseURL, apiKey string, dims int) *Client {
	if dims <= 0 {
		dims = 3072
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		dims:    dims,
		http:    &http.Client{Timeout: RequestTimeout},
	}
}

// post sends a JSON body and decodes a JSON response. A transport failure is
// wrapped in ErrUnreachable; an HTTP error status is not, because the gateway
// answered and its answer is authoritative.
func (c *Client) post(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("llm-gateway: marshal %s: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("llm-gateway: build %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: post %s: %v", ErrUnreachable, path, err)
	}
	defer resp.Body.Close()

	// Cap the read: a wedged proxy can stream indefinitely and no legitimate
	// response is near this size. Embeddings are the largest: 3072 float64s in
	// JSON is roughly 60 KB, so 8 MB leaves generous headroom for a batch.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("%w: read %s: %v", ErrUnreachable, path, err)
	}
	if resp.StatusCode != http.StatusOK {
		// 503 carries the seat reset time, which is the detail that makes the
		// failure actionable. Keep the body.
		return &StatusError{
			Path: path,
			Code: resp.StatusCode,
			Body: strings.TrimSpace(truncate(string(raw), 300)),
		}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("llm-gateway: decode %s: %w", path, err)
	}
	return nil
}

type generateRequest struct {
	System string `json:"system"`
	User   string `json:"user"`
	Tier   string `json:"tier"`
}

type textResponse struct {
	Backend string `json:"backend"`
	Text    string `json:"text"`
}

// generate is the shared body of Generate and GenerateCheap.
//
// An error is always returned rather than an empty string: callers cache "" as
// "the LLM had nothing to say", so a failure that returned "" would poison the
// summary cache with blanks that never regenerate.
func (c *Client) generate(ctx context.Context, tier, systemPrompt, userMessage string) (string, error) {
	var out textResponse
	if err := c.post(ctx, "/generate",
		generateRequest{System: systemPrompt, User: userMessage, Tier: tier}, &out); err != nil {
		return "", err
	}
	if strings.TrimSpace(out.Text) == "" {
		return "", fmt.Errorf("llm-gateway: /generate returned no text (tier=%s backend=%s)", tier, out.Backend)
	}
	return out.Text, nil
}

// Generate runs the "summary" tier — prose worth the better model.
func (c *Client) Generate(ctx context.Context, systemPrompt, userMessage string) (string, error) {
	return c.generate(ctx, "summary", systemPrompt, userMessage)
}

// GenerateCheap runs the "cheap" tier. The topic-link confirm gate is
// high-volume (~15 calls per node) and already receives a cosine shortlist, so
// it must never share a tier with summaries — that volume on the expensive model
// is what a seat's five-hour window cannot absorb.
func (c *Client) GenerateCheap(ctx context.Context, systemPrompt, userMessage string) (string, error) {
	return c.generate(ctx, "cheap", systemPrompt, userMessage)
}

type embedRequest struct {
	Texts []string `json:"texts"`
	Dims  int      `json:"dims"`
}

type embedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
	Model      string      `json:"model"`
	Dims       int         `json:"dims"`
}

// Embed returns one vector at the client's configured dimensionality.
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	return c.EmbedWithOptions(ctx, text, gemini.EmbedOptions{OutputDimensionality: c.dims})
}

// EmbedWithOptions returns one vector, honouring an explicit dimensionality.
//
// Title and TaskType from gemini.EmbedOptions are deliberately ignored: the
// gateway calls gemini-embedding-001 with no task_type, and stored vectors were
// produced that way. Sending one now would place queries in a different vector
// space than the corpus — search would quietly return worse results rather than
// fail, which is the hardest kind of regression to notice.
func (c *Client) EmbedWithOptions(ctx context.Context, text string, opts gemini.EmbedOptions) ([]float32, error) {
	dims := opts.OutputDimensionality
	if dims <= 0 {
		dims = c.dims
	}
	var out embedResponse
	if err := c.post(ctx, "/embed", embedRequest{Texts: []string{text}, Dims: dims}, &out); err != nil {
		return nil, err
	}
	if len(out.Embeddings) != 1 {
		return nil, fmt.Errorf("llm-gateway: /embed returned %d vectors, want 1", len(out.Embeddings))
	}
	v := out.Embeddings[0]
	// Guard the dimensionality here rather than letting pgvector reject the
	// insert: the DB error surfaces far from the cause and reads like a schema
	// problem instead of a gateway one.
	if len(v) != dims {
		return nil, fmt.Errorf("llm-gateway: /embed returned %d dims, want %d", len(v), dims)
	}
	return v, nil
}

// EmbedBatch returns one vector per input, in the same order.
//
// The gateway's /embed takes a list natively, so a batch is one round trip
// rather than N. Order is guaranteed by the gateway, which reorders OpenRouter's
// response by its index field before returning — silently misaligned vectors
// would attach each summary to the wrong node, which no error would reveal.
func (c *Client) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	var out embedResponse
	if err := c.post(ctx, "/embed", embedRequest{Texts: texts, Dims: c.dims}, &out); err != nil {
		return nil, err
	}
	if len(out.Embeddings) != len(texts) {
		return nil, fmt.Errorf("llm-gateway: /embed returned %d vectors for %d texts",
			len(out.Embeddings), len(texts))
	}
	for i, v := range out.Embeddings {
		if len(v) != c.dims {
			return nil, fmt.Errorf("llm-gateway: /embed vector %d has %d dims, want %d", i, len(v), c.dims)
		}
	}
	return out.Embeddings, nil
}

type describeRequest struct {
	Prompt  string `json:"prompt"`
	Mime    string `json:"mime"`
	DataB64 string `json:"data_b64"`
}

// Describe asks for a multimodal description and returns (description, ocr,
// entities). The JSON instruction and the parse mirror gemini.Client.Describe
// exactly so callers cannot tell the two apart — the gateway hands back the
// model's raw text either way.
func (c *Client) Describe(ctx context.Context, mimeType string, data []byte, prompt string) (string, string, []string, error) {
	promptJSON := prompt + `\nRespond as JSON only: {"description":"...","ocr":"verbatim visible text","entities":["..."]}`
	var out textResponse
	err := c.post(ctx, "/describe", describeRequest{
		Prompt:  promptJSON,
		Mime:    mimeType,
		DataB64: base64.StdEncoding.EncodeToString(data),
	}, &out)
	if err != nil {
		return "", "", nil, err
	}
	var parsed struct {
		Description string   `json:"description"`
		OCR         string   `json:"ocr"`
		Entities    []string `json:"entities"`
	}
	if err := json.Unmarshal([]byte(out.Text), &parsed); err != nil {
		return "", "", nil, fmt.Errorf("llm-gateway: describe: parse JSON: %w", err)
	}
	return parsed.Description, parsed.OCR, parsed.Entities, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
