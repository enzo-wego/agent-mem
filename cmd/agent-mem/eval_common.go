package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"
)

var evalVariants = []string{"resolve-asker0", "resolve-asker", "search-hybrid"}

const evalRequestTimeout = 10 * time.Second

// evalQuerier is satisfied by *pgxpool.Pool and pgx.Tx (and so by the seed
// canonicalisation interface in the handlers package).
type evalQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type loadedCase struct {
	evalCase
	path string
	raw  []byte
}

// loadEvalCases reads every case file under dataDir/cases in name order and
// returns the sha256 over their concatenated bytes.
func loadEvalCases(dataDir string) ([]loadedCase, string, error) {
	paths, err := filepath.Glob(filepath.Join(dataDir, "cases", "*.yaml"))
	if err != nil {
		return nil, "", err
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return nil, "", fmt.Errorf("no case files")
	}
	out := make([]loadedCase, 0, len(paths))
	raws := make([][]byte, 0, len(paths))
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, "", err
		}
		var c evalCase
		if err := yaml.Unmarshal(data, &c); err != nil {
			return nil, "", fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, loadedCase{evalCase: c, path: p, raw: data})
		raws = append(raws, data)
	}
	return out, casesSHA(raws), nil
}

func casesSHA(raws [][]byte) string {
	h := sha256.New()
	for _, r := range raws {
		_, _ = h.Write(r)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// buildEvalRequest returns the HTTP request for one named variant of a case.
func buildEvalRequest(c evalCase, name string) (method, pathURL string, body []byte, err error) {
	req, ok := c.Requests[name]
	if !ok {
		return "", "", nil, fmt.Errorf("%s: missing %s", c.ID, name)
	}
	if name == "search-hybrid" {
		values := url.Values{}
		keys := make([]string, 0, len(req.Params))
		for k := range req.Params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			values.Set(k, fmt.Sprint(req.Params[k]))
		}
		return http.MethodGet, "/api/graph/search?" + values.Encode(), nil, nil
	}
	body, _ = json.Marshal(req.Body)
	return http.MethodPost, "/api/graph/resolve", body, nil
}

// readDotEnv parses KEY=VALUE lines (optional `export`, quotes, # comments).
// A missing file yields an empty map.
func readDotEnv(path string) map[string]string {
	out := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out[strings.TrimSpace(k)] = v
	}
	return out
}

// evalAPIKey: the process environment wins, then the .env file. Never argv.
func evalAPIKey(envPath string) string {
	if k := os.Getenv("AGENT_MEM_API_KEY"); k != "" {
		return k
	}
	return readDotEnv(envPath)["AGENT_MEM_API_KEY"]
}

// evalDatabaseURL reads DATABASE_URL from the .env file, falling back to the
// process environment. Never argv.
func evalDatabaseURL(envPath string) string {
	if v := readDotEnv(envPath)["DATABASE_URL"]; v != "" {
		return v
	}
	return os.Getenv("DATABASE_URL")
}

type evalCall struct {
	Status int
	Header http.Header
	Body   []byte
	Dur    time.Duration
	Err    error
}

type evalClient struct {
	base string
	key  string
	http *http.Client
}

func newEvalClient(base, key string) *evalClient {
	return &evalClient{base: strings.TrimRight(base, "/"), key: key, http: &http.Client{Timeout: evalRequestTimeout}}
}

// do sends one eval request (always no-side-effects, caller=eval). Err is set
// for transport errors, timeouts and non-2xx statuses.
func (e *evalClient) do(method, pathURL string, body []byte) evalCall {
	req, err := http.NewRequest(method, e.base+pathURL, bytes.NewReader(body))
	if err != nil {
		return evalCall{Err: err}
	}
	req.Header.Set("X-Graph-Caller", "eval")
	req.Header.Set("X-Graph-No-Side-Effects", "1")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if e.key != "" {
		req.Header.Set("Authorization", "Bearer "+e.key)
	}
	start := time.Now()
	resp, err := e.http.Do(req)
	if err != nil {
		return evalCall{Dur: time.Since(start), Err: err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	call := evalCall{Status: resp.StatusCode, Header: resp.Header, Body: data, Dur: time.Since(start)}
	switch {
	case err != nil:
		call.Err = err
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		call.Err = fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return call
}

type evalReturn struct {
	IDs       []string          `json:"ids"`
	Linked    []bool            `json:"-"`
	ArmErrors map[string]string `json:"arm_errors,omitempty"`
	RequestID string            `json:"request_id,omitempty"`
}

// parseEvalResponse extracts ordered node ids; for search it also records which
// rows are linked-thread rows (`linked_via` present).
func parseEvalResponse(variant string, call evalCall) (evalReturn, error) {
	var out evalReturn
	var decoded struct {
		RequestID string `json:"request_id"`
		Artifacts []struct {
			NodeID string `json:"node_id"`
		} `json:"artifacts"`
		Results []struct {
			NodeID    string `json:"node_id"`
			ID        string `json:"id"`
			LinkedVia string `json:"linked_via"`
		} `json:"results"`
		ArmErrors map[string]string `json:"arm_errors"`
	}
	if err := json.Unmarshal(call.Body, &decoded); err != nil {
		return out, fmt.Errorf("decode response: %w", err)
	}
	out.RequestID = call.Header.Get("X-Graph-Request-Id")
	if out.RequestID == "" {
		out.RequestID = decoded.RequestID
	}
	out.ArmErrors = decoded.ArmErrors
	out.IDs = []string{}
	if variant == "search-hybrid" {
		for _, r := range decoded.Results {
			id := r.NodeID
			if id == "" {
				id = r.ID
			}
			out.IDs = append(out.IDs, id)
			out.Linked = append(out.Linked, r.LinkedVia != "")
		}
		return out, nil
	}
	for _, a := range decoded.Artifacts {
		out.IDs = append(out.IDs, a.NodeID)
		out.Linked = append(out.Linked, false)
	}
	return out, nil
}

type nodeMeta struct {
	Type       string `json:"type"`
	Exists     bool   `json:"exists"`
	IsReply    bool   `json:"is_reply"`
	ThreadRoot string `json:"thread_root"`
}

func idPrefix(id string) string {
	p, _, _ := strings.Cut(id, ":")
	return p
}

// slackReply applies the reply predicate: a slack node whose thread_ts is
// present and differs from its own ts (colon component 3 of the id). Returns
// the thread root id ("" when thread_ts is absent).
func slackReply(id, threadTS string) (isReply bool, root string) {
	parts := strings.SplitN(id, ":", 3)
	if len(parts) != 3 || parts[0] != "slack" || threadTS == "" {
		return false, ""
	}
	return threadTS != parts[2], "slack:" + parts[1] + ":" + threadTS
}

// fetchNodeMeta reads node metadata for ids through q (a read-only transaction).
// Any query error is returned; nothing is guessed.
func fetchNodeMeta(ctx context.Context, q evalQuerier, ids []string) (map[string]nodeMeta, error) {
	out := make(map[string]nodeMeta, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT id, type, COALESCE(metadata->>'thread_ts','') FROM graph.nodes WHERE id = ANY($1) AND deleted_at IS NULL`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, typ, ts string
		if err := rows.Scan(&id, &typ, &ts); err != nil {
			return nil, err
		}
		reply, root := slackReply(id, ts)
		out[id] = nodeMeta{Type: typ, Exists: true, IsReply: reply, ThreadRoot: root}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, ok := out[id]; !ok {
			out[id] = nodeMeta{Type: idPrefix(id), Exists: false}
		}
	}
	return out, nil
}

// beginEvalTx opens a transaction and makes it read-only before any query.
func beginEvalTx(ctx context.Context, pool *pgxpool.Pool) (pgx.Tx, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, "SET TRANSACTION READ ONLY"); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}

func uniqueSorted(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func labelIDs(c evalCase) []string {
	var out []string
	for _, l := range c.Labels {
		out = append(out, l.Must...)
		out = append(out, l.May...)
		out = append(out, l.MustNot...)
	}
	return out
}

// writeFilesAtomic writes every file to a temp name in its directory and only
// then renames them all; any write error removes the temps and replaces nothing.
func writeFilesAtomic(files map[string][]byte) error {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	temps := map[string]string{}
	cleanup := func() {
		for _, t := range temps {
			_ = os.Remove(t)
		}
	}
	for _, p := range paths {
		f, err := os.CreateTemp(filepath.Dir(p), ".eval-*.tmp")
		if err != nil {
			cleanup()
			return err
		}
		temps[p] = f.Name()
		_ = os.Chmod(f.Name(), 0o644)
		if _, err := f.Write(files[p]); err != nil {
			f.Close()
			cleanup()
			return err
		}
		if err := f.Close(); err != nil {
			cleanup()
			return err
		}
	}
	for _, p := range paths {
		if err := os.Rename(temps[p], p); err != nil {
			cleanup()
			return err
		}
		delete(temps, p)
	}
	return nil
}

func (c evalCall) RequestID() string { return c.Header.Get("X-Graph-Request-Id") }

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// validateEvalCaseState checks the allowed labelling states of a case file.
func validateEvalCaseState(c evalCase) error {
	switch c.LabelledBy {
	case "none", "fable-draft", "human":
	default:
		return fmt.Errorf("%s: labelled_by=%q not in {none, fable-draft, human}", c.ID, c.LabelledBy)
	}
	if c.CheckedBy != nil && *c.CheckedBy != "enzo" {
		return fmt.Errorf("%s: checked_by=%q must be null or enzo", c.ID, *c.CheckedBy)
	}
	seeds := setOf(c.CanonicalSeeds)
	for route, l := range c.Labels {
		seen := map[string]string{}
		for kind, list := range map[string][]string{"must": l.Must, "may": l.May, "must_not": l.MustNot} {
			if c.LabelledBy == "none" && len(list) > 0 {
				return fmt.Errorf("%s: labelled_by none but %s.%s is not empty", c.ID, route, kind)
			}
			for _, id := range list {
				if seeds[id] {
					return fmt.Errorf("%s: canonical seed %s in %s.%s", c.ID, id, route, kind)
				}
				if prev, dup := seen[id]; dup {
					return fmt.Errorf("%s: %s in both %s.%s and %s.%s", c.ID, id, route, prev, route, kind)
				}
				seen[id] = kind
			}
		}
	}
	return nil
}
