package fetchers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/ids"
)

var (
	// wegoHubNodeRe matches "wegohub:<slug>".
	wegoHubNodeRe = regexp.MustCompile(`^wegohub:([a-z0-9][a-z0-9-]*)$`)
	// wegoHubURLRe matches a served slug URL, anchored to the exact scheme+host
	// so a spoofed URL (e.g. https://evil.com/?x=internal.wego.com/hub/apps/foo)
	// is not claimed and mapped to an unrelated slug:
	// https://internal.wego.com/hub/apps/<slug>[/<file>].
	wegoHubURLRe = regexp.MustCompile(`^https://internal\.wego\.com/hub/apps/([a-z0-9][a-z0-9-]*)\b`)
)

// wegoHubFetcher retrieves a published Wego Hub slug: it reads slug metadata
// (description, owner, file list) from the read API and downloads the primary
// served file (index.html, else the first file) as the artifact body.
//
// Served files are internal-public (any signed-in @wego.com user can read);
// the Bearer token is only needed for the metadata/file-list API.
type wegoHubFetcher struct {
	cfg Config
	log zerolog.Logger
}

func newWegoHubFetcher(cfg Config, log zerolog.Logger) *wegoHubFetcher {
	return &wegoHubFetcher{cfg: cfg, log: log}
}

func (f *wegoHubFetcher) Source() string { return "wegohub" }

// Matches returns true for wegohub:<slug> node IDs or Wego Hub app URLs.
func (f *wegoHubFetcher) Matches(nodeIDorURL string) bool {
	return wegoHubNodeRe.MatchString(nodeIDorURL) || wegoHubURLRe.MatchString(nodeIDorURL)
}

// wegoHubEnvelope is the consistent { "status": "...", "data": {...} } wrapper.
type wegoHubEnvelope struct {
	Status string          `json:"status"`
	Data   json.RawMessage `json:"data"`
}

// wegoHubMeta is the slug metadata returned by GET /api/files/:slug.
// Fields are best-effort — the fetcher degrades gracefully if any are absent.
type wegoHubMeta struct {
	Slug        string   `json:"slug"`
	Description string   `json:"description"`
	Owner       string   `json:"owner"`
	Files       []string `json:"files"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Fetch retrieves the Wego Hub slug.
func (f *wegoHubFetcher) Fetch(ctx context.Context, node string) (FetchedBody, error) {
	slug, err := f.parseNode(node)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("wegohub fetcher: %w", err)
	}

	base := strings.TrimRight(f.cfg.WegoHubBaseURL, "/")
	if base == "" {
		base = "https://internal.wego.com/hub"
	}

	// Best-effort metadata: description, owner, file list.
	meta := f.fetchMeta(ctx, base, slug)

	// Pick the primary file: index.html if present, else the first file.
	primary := "index.html"
	if len(meta.Files) > 0 && !slices.Contains(meta.Files, "index.html") {
		primary = meta.Files[0]
	}

	// Download the served file (public, no auth).
	fileURL := fmt.Sprintf("%s/apps/%s/%s", base, slug, primary)
	raw, ctype, err := f.downloadFile(ctx, fileURL)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("wegohub fetcher: download %s: %w", fileURL, err)
	}

	title := meta.Description
	if title == "" {
		title = slug
	}

	nodeID, err := ids.WegoHub(slug)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("wegohub fetcher: %w", err)
	}

	return FetchedBody{
		NodeID:      nodeID,
		Type:        ids.TypeWegoHub,
		URL:         fmt.Sprintf("%s/apps/%s", base, slug),
		Title:       title,
		Raw:         raw,
		ContentType: ctype,
		Author: AuthorRef{
			Source: "wegohub",
			Email:  meta.Owner, // owner is an @wego.com address; resolves by email
		},
		BodyTS: meta.UpdatedAt,
		Metadata: map[string]any{
			"slug":        slug,
			"description": meta.Description,
			"owner":       meta.Owner,
			"files":       meta.Files,
		},
	}, nil
}

// fetchMeta reads slug metadata; returns a zero-value meta (with the slug set)
// on any failure so the caller can still fetch the served file.
func (f *wegoHubFetcher) fetchMeta(ctx context.Context, base, slug string) wegoHubMeta {
	out := wegoHubMeta{Slug: slug}
	apiURL := fmt.Sprintf("%s/api/files/%s", base, slug)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return out
	}
	if f.cfg.WegoHubToken != "" {
		req.Header.Set("Authorization", "Bearer "+f.cfg.WegoHubToken)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := f.cfg.HTTPClient.Do(req)
	if err != nil {
		f.log.Warn().Err(err).Str("slug", slug).Msg("wegohub: metadata fetch failed; using served file only")
		return out
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out
	}

	var env wegoHubEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return out
	}
	// data may be the metadata object, or { "files": [...] } — decode leniently.
	_ = json.Unmarshal(env.Data, &out)
	out.Slug = slug
	return out
}

// downloadFile GETs a served file and returns its bytes + content type.
func (f *wegoHubFetcher) downloadFile(ctx context.Context, url string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := f.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, "", fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20)) // 10 MB per-file cap (Hub limit)
	if err != nil {
		return nil, "", err
	}
	ctype := resp.Header.Get("Content-Type")
	if ctype == "" {
		ctype = "text/html"
	}
	return data, ctype, nil
}

// parseNode extracts the slug from a node ID or Wego Hub URL.
func (f *wegoHubFetcher) parseNode(node string) (string, error) {
	if m := wegoHubNodeRe.FindStringSubmatch(node); m != nil {
		return m[1], nil
	}
	if m := wegoHubURLRe.FindStringSubmatch(node); m != nil {
		return m[1], nil
	}
	return "", fmt.Errorf("cannot parse wegohub node %q", node)
}
