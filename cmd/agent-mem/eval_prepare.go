package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/agent-mem/agent-mem/internal/graph/handlers"
)

type replayVariant struct {
	Status    int      `json:"status"`
	RequestID string   `json:"request_id"`
	Returned  []string `json:"returned"`
}

type replayCase struct {
	CanonicalSeeds []string                 `json:"canonical_seeds"`
	Variants       map[string]replayVariant `json:"variants"`
}

type replayFile struct {
	PreparedAt  string                `json:"prepared_at"`
	BuildRev    string                `json:"build_rev"`
	CasesSHA256 string                `json:"cases_sha256"`
	Cases       map[string]replayCase `json:"cases"`
}

type evalPrepareOpts struct {
	BaseURL string
	Refresh bool
	DataDir string
	EnvPath string
	Pool    *pgxpool.Pool
}

func runEvalPrepareCLI(cmd *cobra.Command, baseURL string, refresh bool) error {
	dsn := evalDatabaseURL(".env")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL not found in .env")
	}
	pool, err := pgxpool.New(cmd.Context(), dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	return runEvalPrepare(cmd.Context(), cmd.OutOrStdout(), evalPrepareOpts{BaseURL: baseURL, Refresh: refresh, DataDir: evalDataDir, EnvPath: ".env", Pool: pool})
}

var canonicalSeedsLine = regexp.MustCompile(`(?m)^canonical_seeds:.*$`)

// rewriteCanonicalSeeds replaces the single-line canonical_seeds entry in the
// raw case file so anchors and formatting survive.
func rewriteCanonicalSeeds(raw []byte, seeds []string) ([]byte, error) {
	if !canonicalSeedsLine.Match(raw) {
		return nil, fmt.Errorf("no canonical_seeds line")
	}
	if seeds == nil {
		seeds = []string{}
	}
	enc, _ := json.Marshal(seeds)
	line := "canonical_seeds: " + string(enc)
	return canonicalSeedsLine.ReplaceAllLiteral(raw, []byte(line)), nil
}

func caseSeedInput(c evalCase) []string {
	var out []string
	if b, ok := c.Requests["resolve-asker0"]; ok {
		if raw, ok := b.Body["seeds"].([]any); ok {
			for _, s := range raw {
				if str, ok := s.(string); ok {
					out = append(out, str)
				}
			}
		}
	}
	return out
}

// runEvalPrepare computes canonical seeds in a read-only transaction, replays
// every variant over HTTP, and writes node_meta.json, replay.json and the
// case files' canonical_seeds together, only after everything succeeded.
func runEvalPrepare(ctx context.Context, out io.Writer, o evalPrepareOpts) error {
	if o.BaseURL == "" {
		return fmt.Errorf("--base-url is required")
	}
	cases, _, err := loadEvalCases(o.DataDir)
	if err != nil {
		return err
	}
	tx, err := beginEvalTx(ctx, o.Pool)
	if err != nil {
		return fmt.Errorf("begin read-only transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	seeds := map[string][]string{}
	for _, c := range cases {
		resolved, diags := handlers.CanonicalizeSeeds(ctx, tx, caseSeedInput(c.evalCase))
		if len(diags) > 0 {
			return fmt.Errorf("%s: canonicalise seeds: %w", c.ID, diags[0])
		}
		seeds[c.ID] = resolved
		fmt.Fprintf(out, "case %s canonical_seeds=%d\n", c.ID, len(resolved))
	}

	client := newEvalClient(o.BaseURL, evalAPIKey(o.EnvPath))
	replay := replayFile{Cases: map[string]replayCase{}}
	var ids []string
	buildRev := ""
	for _, c := range cases {
		rc := replayCase{CanonicalSeeds: seeds[c.ID], Variants: map[string]replayVariant{}}
		ids = append(ids, seeds[c.ID]...)
		ids = append(ids, labelIDs(c.evalCase)...)
		for _, name := range evalVariants {
			method, pathURL, body, err := buildEvalRequest(c.evalCase, name)
			if err != nil {
				return err
			}
			call := client.do(method, pathURL, body)
			if call.Err != nil {
				return fmt.Errorf("%s %s: %w", c.ID, name, call.Err)
			}
			ret, err := parseEvalResponse(name, call)
			if err != nil {
				return fmt.Errorf("%s %s: %w", c.ID, name, err)
			}
			if rev := call.Header.Get("X-Graph-Build-Rev"); rev != "" {
				buildRev = rev
			}
			rc.Variants[name] = replayVariant{Status: call.Status, RequestID: ret.RequestID, Returned: ret.IDs}
			ids = append(ids, ret.IDs...)
			fmt.Fprintf(out, "  %s: %d items\n", name, len(ret.IDs))
		}
		replay.Cases[c.ID] = rc
	}
	ids = uniqueSorted(ids)

	metaPath := filepath.Join(o.DataDir, "node_meta.json")
	meta := map[string]nodeMeta{}
	need := ids
	if o.Refresh {
		if data, err := os.ReadFile(metaPath); err == nil {
			if err := json.Unmarshal(data, &meta); err != nil {
				return fmt.Errorf("%s: %w", metaPath, err)
			}
			need = nil
			for _, id := range ids {
				if _, ok := meta[id]; !ok {
					need = append(need, id)
				}
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	fetched, err := fetchNodeMeta(ctx, tx, need)
	if err != nil {
		return fmt.Errorf("node metadata: %w", err)
	}
	for id, m := range fetched {
		meta[id] = m
	}

	files := map[string][]byte{}
	raws := make([][]byte, 0, len(cases))
	for _, c := range cases {
		raw, err := rewriteCanonicalSeeds(c.raw, seeds[c.ID])
		if err != nil {
			return fmt.Errorf("%s: %w", c.path, err)
		}
		var check evalCase
		if err := yaml.Unmarshal(raw, &check); err != nil {
			return fmt.Errorf("%s: rewritten file does not parse: %w", c.path, err)
		}
		if strings.Join(check.CanonicalSeeds, "\n") != strings.Join(seeds[c.ID], "\n") {
			return fmt.Errorf("%s: rewritten canonical_seeds mismatch", c.path)
		}
		raws = append(raws, raw)
		files[c.path] = raw
	}
	replay.PreparedAt = time.Now().UTC().Format(time.RFC3339)
	replay.BuildRev = unknownBuildRev(buildRev)
	replay.CasesSHA256 = casesSHA(raws)
	metaJSON, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	replayJSON, err := json.MarshalIndent(replay, "", "  ")
	if err != nil {
		return err
	}
	files[metaPath] = append(metaJSON, '\n')
	files[filepath.Join(o.DataDir, "replay.json")] = append(replayJSON, '\n')
	if err := writeFilesAtomic(files); err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote node_meta.json (%d nodes), replay.json, %d case files\n", len(meta), len(cases))
	return nil
}

type labelSheetOpts struct {
	CaseID  string
	All     bool
	OutDir  string
	DataDir string
	EnvPath string
	Pool    *pgxpool.Pool
}

func runLabelSheetCLI(cmd *cobra.Command, args []string, all bool, outDir string) error {
	id := ""
	if len(args) == 1 {
		id = args[0]
	}
	dsn := evalDatabaseURL(".env")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL not found in .env")
	}
	pool, err := pgxpool.New(cmd.Context(), dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	return runLabelSheet(cmd.Context(), cmd.OutOrStdout(), labelSheetOpts{CaseID: id, All: all, OutDir: outDir, DataDir: evalDataDir, EnvPath: ".env", Pool: pool})
}

// repoRoot walks up from the working directory to the directory holding go.mod.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("repo root (go.mod) not found")
		}
		dir = parent
	}
}

func resolveExisting(p string) string {
	p, _ = filepath.Abs(p)
	rest := ""
	for {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, rest)
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

func insideDir(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func runLabelSheet(ctx context.Context, out io.Writer, o labelSheetOpts) error {
	if o.OutDir == "" {
		return fmt.Errorf("--out is required")
	}
	if (o.CaseID == "") == !o.All {
		return fmt.Errorf("give exactly one of <case-id> or --all")
	}
	root, err := repoRoot()
	if err != nil {
		return err
	}
	if insideDir(resolveExisting(o.OutDir), resolveExisting(root)) {
		return fmt.Errorf("--out must be outside the repo (%s)", root)
	}
	replayData, err := os.ReadFile(filepath.Join(o.DataDir, "replay.json"))
	if err != nil {
		return fmt.Errorf("replay.json missing; run eval prepare first: %w", err)
	}
	var replay replayFile
	if err := json.Unmarshal(replayData, &replay); err != nil {
		return fmt.Errorf("replay.json: %w", err)
	}
	cases, sum, err := loadEvalCases(o.DataDir)
	if err != nil {
		return err
	}
	if replay.CasesSHA256 != sum {
		return fmt.Errorf("replay.json cases_sha256 differs from the current case files; run eval prepare")
	}
	var chosen []loadedCase
	for _, c := range cases {
		if o.All || c.ID == o.CaseID {
			chosen = append(chosen, c)
		}
	}
	if !o.All && len(chosen) == 0 {
		return fmt.Errorf("case %q not found", o.CaseID)
	}
	for _, c := range chosen {
		if _, ok := replay.Cases[c.ID]; !ok {
			return fmt.Errorf("case %s is absent from replay.json; run eval prepare", c.ID)
		}
	}
	tx, err := beginEvalTx(ctx, o.Pool)
	if err != nil {
		return fmt.Errorf("begin read-only transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	sheets := map[string][]byte{}
	for _, c := range chosen {
		rc := replay.Cases[c.ID]
		seedSet := map[string]bool{}
		for _, s := range rc.CanonicalSeeds {
			seedSet[s] = true
		}
		seen := map[string]bool{}
		var nodes []string
		for _, name := range evalVariants {
			for _, id := range rc.Variants[name].Returned {
				if !seedSet[id] && !seen[id] {
					seen[id] = true
					nodes = append(nodes, id)
				}
			}
		}
		var b strings.Builder
		fmt.Fprintf(&b, "# Label sheet: %s\n\n", c.ID)
		fmt.Fprintf(&b, "Use case: %s\n\nSource thread: %s\n\n", c.UseCase, c.SourceThread)
		fmt.Fprintf(&b, "Query: %s\n\n", caseQuery(c.evalCase))
		fmt.Fprintf(&b, "Candidates: %d (canonical seeds excluded). CONFIDENTIAL: may contain customer content; do not commit.\n", len(nodes))
		for _, id := range nodes {
			row, err := readSheetNode(ctx, tx, id)
			if err != nil {
				return fmt.Errorf("%s %s: %w", c.ID, id, err)
			}
			fmt.Fprintf(&b, "\n## %s\n\n- type: %s\n- is_reply: %t\n- title: %s\n- content: %s\n", id, row.Type, row.IsReply, row.Title, row.Content)
		}
		sheets[filepath.Join(o.OutDir, c.ID+".md")] = []byte(b.String())
	}
	if err := os.MkdirAll(o.OutDir, 0o700); err != nil {
		return err
	}
	for p, data := range sheets {
		if err := os.WriteFile(p, data, 0o600); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "wrote %d label sheets to %s\n", len(sheets), o.OutDir)
	return nil
}

func caseQuery(c evalCase) string {
	if b, ok := c.Requests["resolve-asker0"]; ok {
		if q, ok := b.Body["query"].(string); ok {
			return q
		}
	}
	if p, ok := c.Requests["search-hybrid"]; ok {
		return fmt.Sprint(p.Params["q"])
	}
	return ""
}

type sheetNode struct {
	Type, Title, Content string
	IsReply              bool
}

func readSheetNode(ctx context.Context, q evalQuerier, id string) (sheetNode, error) {
	var typ, title, ts, content string
	err := q.QueryRow(ctx, `
SELECT n.type, COALESCE(n.title,''), COALESCE(n.metadata->>'thread_ts',''),
       left(COALESCE(NULLIF(ai.summary,''), ab.body_full, ''), 300)
FROM graph.nodes n
LEFT JOIN graph.artifact_index ai ON ai.node_id = n.id
LEFT JOIN graph.artifact_bodies ab ON ab.node_id = n.id
WHERE n.id = $1 AND n.deleted_at IS NULL`, id).Scan(&typ, &title, &ts, &content)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return sheetNode{Type: idPrefix(id), Title: "(node not found)"}, nil
		}
		return sheetNode{}, err
	}
	reply, _ := slackReply(id, ts)
	return sheetNode{Type: typ, Title: title, Content: strings.ReplaceAll(content, "\n", " "), IsReply: reply}, nil
}
