package fetchers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// RepoDoc is one markdown file read from a GitHub repository.
type RepoDoc struct {
	Path    string
	Content string
}

// CFPageRef is a Confluence page id + title (from descendant enumeration).
type CFPageRef struct {
	ID    string
	Title string
}

// maxRepoMarkdown caps how many markdown files we read from a repo, so a huge
// repo can't blow up the read / LLM-distill step.
const maxRepoMarkdown = 300

// cfBase returns the Confluence base URL (CFBaseURL, else JiraBaseURL + /wiki).
func (r *Registry) cfBase() string {
	base := strings.TrimRight(r.cfg.CFBaseURL, "/")
	if base == "" {
		base = strings.TrimRight(r.cfg.JiraBaseURL, "/") + "/wiki"
	}
	return base
}

func (r *Registry) cfAuth(req *http.Request) {
	if r.cfg.CFToken != "" {
		req.SetBasicAuth(r.cfg.JiraEmail, r.cfg.CFToken)
	} else {
		req.SetBasicAuth(r.cfg.JiraEmail, r.cfg.JiraToken)
	}
	req.Header.Set("Accept", "application/json")
}

// ConfluenceDescendants returns id+title for all descendants of pageID (the
// whole sub-tree, not including pageID itself), paginated via the v2 API.
func (r *Registry) ConfluenceDescendants(ctx context.Context, pageID string) ([]CFPageRef, error) {
	base := r.cfBase()
	var refs []CFPageRef
	cursor := ""
	for len(refs) < 1000 {
		apiURL := fmt.Sprintf("%s/api/v2/pages/%s/descendants?limit=250", base, pageID)
		if cursor != "" {
			apiURL += "&cursor=" + url.QueryEscape(cursor)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
		if err != nil {
			return refs, err
		}
		r.cfAuth(req)
		resp, err := r.cfg.HTTPClient.Do(req)
		if err != nil {
			return refs, err
		}
		var page struct {
			Results []struct {
				ID    string `json:"id"`
				Title string `json:"title"`
			} `json:"results"`
			Links struct {
				Next string `json:"next"`
			} `json:"_links"`
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
			resp.Body.Close()
			return refs, fmt.Errorf("confluence descendants status %d: %s", resp.StatusCode, string(body))
		}
		err = json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return refs, err
		}
		for _, p := range page.Results {
			refs = append(refs, CFPageRef{ID: p.ID, Title: p.Title})
		}
		cursor = cursorFromNext(page.Links.Next)
		if cursor == "" {
			break
		}
	}
	return refs, nil
}

// cursorFromNext extracts the "cursor" query param from a v2 _links.next URL.
func cursorFromNext(next string) string {
	_, q, ok := strings.Cut(next, "?")
	if !ok {
		return ""
	}
	if vals, err := url.ParseQuery(q); err == nil {
		return vals.Get("cursor")
	}
	return ""
}

// isMarkdown reports whether a repo path is a markdown file.
func isMarkdown(path string) bool {
	p := strings.ToLower(path)
	return strings.HasSuffix(p, ".md") || strings.HasSuffix(p, ".markdown")
}

func (r *Registry) ghAuth(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+r.cfg.GHToken)
	req.Header.Set("Accept", "application/vnd.github+json")
}

// RepoMarkdown reads every markdown file in repo (e.g. "wego/payments"), up to
// maxRepoMarkdown, using the default branch when ref is empty.
func (r *Registry) RepoMarkdown(ctx context.Context, repo, ref string) ([]RepoDoc, error) {
	base := strings.TrimRight(r.cfg.GHBaseURL, "/")
	if base == "" {
		base = "https://api.github.com"
	}
	if ref == "" {
		var meta struct {
			DefaultBranch string `json:"default_branch"`
		}
		if err := r.ghGet(ctx, fmt.Sprintf("%s/repos/%s", base, repo), &meta); err != nil {
			return nil, fmt.Errorf("repo meta: %w", err)
		}
		ref = meta.DefaultBranch
		if ref == "" {
			ref = "main"
		}
	}

	var tree struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
		Truncated bool `json:"truncated"`
	}
	if err := r.ghGet(ctx, fmt.Sprintf("%s/repos/%s/git/trees/%s?recursive=1", base, repo, url.PathEscape(ref)), &tree); err != nil {
		return nil, fmt.Errorf("repo tree: %w", err)
	}

	var docs []RepoDoc
	for _, e := range tree.Tree {
		if e.Type != "blob" || !isMarkdown(e.Path) {
			continue
		}
		if len(docs) >= maxRepoMarkdown {
			break
		}
		var content struct {
			Content  string `json:"content"`
			Encoding string `json:"encoding"`
		}
		// Path segments must be escaped individually; the slashes stay.
		escaped := strings.Join(mapStrings(strings.Split(e.Path, "/"), url.PathEscape), "/")
		cURL := fmt.Sprintf("%s/repos/%s/contents/%s?ref=%s", base, repo, escaped, url.QueryEscape(ref))
		if err := r.ghGet(ctx, cURL, &content); err != nil {
			continue // skip unreadable file
		}
		body := content.Content
		if content.Encoding == "base64" {
			if dec, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(body, "\n", "")); err == nil {
				body = string(dec)
			}
		}
		docs = append(docs, RepoDoc{Path: e.Path, Content: body})
	}
	return docs, nil
}

func (r *Registry) ghGet(ctx context.Context, url string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	r.ghAuth(req)
	resp, err := r.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("github status %d: %s", resp.StatusCode, string(body))
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

func mapStrings(in []string, f func(string) string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = f(s)
	}
	return out
}
