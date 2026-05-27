package fetchers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/ids"
)

var (
	gwsNodeRe    = regexp.MustCompile(`^gws_doc:([\w-]+)$`)
	gwsDocsURLRe = regexp.MustCompile(`\bdocs\.google\.com/document/d/([\w-]+)\b`)
	gwsDriveURLRe = regexp.MustCompile(`\bdrive\.google\.com/file/d/([\w-]+)\b`)
)

// gwsFetcher retrieves Google Workspace document bodies.
// Production use requires a service-account bearer token set in GWS_BEARER_TOKEN
// env var; the GWSServiceKeyPath field is reserved for future JWT exchange wiring.
type gwsFetcher struct {
	cfg Config
	log zerolog.Logger
}

func newGWSFetcher(cfg Config, log zerolog.Logger) *gwsFetcher {
	return &gwsFetcher{cfg: cfg, log: log}
}

func (f *gwsFetcher) Source() string { return "gws" }

// Matches returns true for gws_doc:<id> node IDs or Google Docs/Drive URLs.
func (f *gwsFetcher) Matches(nodeIDorURL string) bool {
	if gwsNodeRe.MatchString(nodeIDorURL) {
		return true
	}
	if gwsDocsURLRe.MatchString(nodeIDorURL) {
		return true
	}
	return gwsDriveURLRe.MatchString(nodeIDorURL)
}

type gwsDocResponse struct {
	DocumentID string `json:"documentId"`
	Title      string `json:"title"`
	RevisionID string `json:"revisionId"`
}

type gwsFileResponse struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	MimeType     string    `json:"mimeType"`
	ModifiedTime time.Time `json:"modifiedTime"`
	Owners       []gwsOwner `json:"owners"`
}

type gwsOwner struct {
	DisplayName  string `json:"displayName"`
	EmailAddress string `json:"emailAddress"`
}

// Fetch retrieves the GWS document.
//
// Production note: this fetcher reads a bearer token from the GWS_BEARER_TOKEN
// environment variable. The GWSServiceKeyPath field is reserved for a future
// service-account JWT exchange implementation. When GWSServiceKeyPath is set but
// GWS_BEARER_TOKEN is empty, the fetcher returns an error rather than crashing.
func (f *gwsFetcher) Fetch(ctx context.Context, node string) (FetchedBody, error) {
	// Check that we are configured enough to proceed.
	if f.cfg.GWSServiceKeyPath == "" && os.Getenv("GWS_BEARER_TOKEN") == "" {
		return FetchedBody{}, fmt.Errorf("gws fetcher not configured: set GWS_BEARER_TOKEN or GWSServiceKeyPath")
	}

	token := os.Getenv("GWS_BEARER_TOKEN")
	if token == "" {
		// GWSServiceKeyPath is set but JWT exchange not implemented yet.
		return FetchedBody{}, fmt.Errorf("gws fetcher not configured: GWS_BEARER_TOKEN not set; JWT exchange from GWSServiceKeyPath is not yet implemented")
	}

	fileID, err := f.parseNode(node)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("gws fetcher: %w", err)
	}

	// Try Docs API first for documents; fall back to Drive export.
	body, err := f.fetchDocsAPI(ctx, fileID, token)
	if err != nil {
		f.log.Debug().Str("file_id", fileID).Err(err).Msg("gws fetcher: docs API failed, falling back to drive export")
		return f.fetchDriveExport(ctx, fileID, token)
	}
	return body, nil
}

// fetchDocsAPI calls the Google Docs API and returns JSON body.
func (f *gwsFetcher) fetchDocsAPI(ctx context.Context, fileID, token string) (FetchedBody, error) {
	apiURL := fmt.Sprintf("https://docs.googleapis.com/v1/documents/%s", fileID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("gws fetcher: build docs request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := f.cfg.HTTPClient.Do(req)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("gws fetcher: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return FetchedBody{}, fmt.Errorf("gws fetcher status %d: %s", resp.StatusCode, string(body))
	}

	rawBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("gws fetcher: read docs body: %w", err)
	}

	var doc gwsDocResponse
	if err := json.Unmarshal(rawBytes, &doc); err != nil {
		return FetchedBody{}, fmt.Errorf("gws fetcher: decode docs response: %w", err)
	}

	title := doc.Title
	if title == "" {
		title = fileID
	}

	nodeID := ids.GWSDoc(fileID)
	return FetchedBody{
		NodeID:      nodeID,
		Type:        ids.TypeGWSDoc,
		URL:         fmt.Sprintf("https://docs.google.com/document/d/%s", fileID),
		Title:       title,
		Raw:         rawBytes,
		ContentType: "application/json",
	}, nil
}

// fetchDriveExport calls the Drive export API and returns plain-text body.
func (f *gwsFetcher) fetchDriveExport(ctx context.Context, fileID, token string) (FetchedBody, error) {
	// First get file metadata.
	metaURL := fmt.Sprintf("https://www.googleapis.com/drive/v3/files/%s?fields=id,name,mimeType,modifiedTime,owners", fileID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metaURL, nil)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("gws fetcher: build meta request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := f.cfg.HTTPClient.Do(req)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("gws fetcher: meta request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return FetchedBody{}, fmt.Errorf("gws fetcher status %d: %s", resp.StatusCode, string(body))
	}

	var fileMeta gwsFileResponse
	if err := json.NewDecoder(resp.Body).Decode(&fileMeta); err != nil {
		return FetchedBody{}, fmt.Errorf("gws fetcher: decode meta: %w", err)
	}

	// Export as plain text.
	exportURL := fmt.Sprintf("https://www.googleapis.com/drive/v3/files/%s/export?mimeType=text/plain", fileID)
	expReq, err := http.NewRequestWithContext(ctx, http.MethodGet, exportURL, nil)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("gws fetcher: build export request: %w", err)
	}
	expReq.Header.Set("Authorization", "Bearer "+token)

	expResp, err := f.cfg.HTTPClient.Do(expReq)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("gws fetcher: export request: %w", err)
	}
	defer expResp.Body.Close()

	if expResp.StatusCode < 200 || expResp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(expResp.Body, 256))
		return FetchedBody{}, fmt.Errorf("gws fetcher status %d: %s", expResp.StatusCode, string(body))
	}

	rawBytes, err := io.ReadAll(expResp.Body)
	if err != nil {
		return FetchedBody{}, fmt.Errorf("gws fetcher: read export body: %w", err)
	}

	var author AuthorRef
	if len(fileMeta.Owners) > 0 {
		owner := fileMeta.Owners[0]
		author = AuthorRef{
			Source:      "gws",
			DisplayName: owner.DisplayName,
			Email:       owner.EmailAddress,
		}
	}

	title := strings.TrimSpace(fileMeta.Name)

	nodeID := ids.GWSDoc(fileID)
	return FetchedBody{
		NodeID:      nodeID,
		Type:        ids.TypeGWSDoc,
		URL:         fmt.Sprintf("https://drive.google.com/file/d/%s", fileID),
		Title:       title,
		Raw:         rawBytes,
		ContentType: "text/plain",
		Author:      author,
		BodyTS:      fileMeta.ModifiedTime,
	}, nil
}

// parseNode extracts the file ID from a node ID or Google URL.
func (f *gwsFetcher) parseNode(node string) (string, error) {
	if m := gwsNodeRe.FindStringSubmatch(node); m != nil {
		return m[1], nil
	}
	if m := gwsDocsURLRe.FindStringSubmatch(node); m != nil {
		return m[1], nil
	}
	if m := gwsDriveURLRe.FindStringSubmatch(node); m != nil {
		return m[1], nil
	}
	return "", fmt.Errorf("cannot parse gws node %q", node)
}
