package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/agent-mem/agent-mem/internal/graph/handlers"
	"github.com/agent-mem/agent-mem/internal/redact"
)

type replayVariant struct {
	Status int `json:"status"`
	// Outcome: ok, lead_off, http_error, transport_error, decode_error. Empty
	// (legacy replay) with a 2xx status reads as ok for the three old variants.
	Outcome   string   `json:"outcome,omitempty"`
	RequestID string   `json:"request_id"`
	Returned  []string `json:"returned"`
	// Items is set for resolve-lead only (ids, enums, numbers; never text). A
	// pointer so an empty list stays distinguishable from a missing one.
	Items *[]leadItem `json:"items,omitempty"`
}

type replayCase struct {
	CanonicalSeeds []string                 `json:"canonical_seeds"`
	Variants       map[string]replayVariant `json:"variants"`
}

type replayFile struct {
	PreparedAt        string                `json:"prepared_at"`
	BuildRev          string                `json:"build_rev"`
	CasesSHA256       string                `json:"cases_sha256"`
	EligibilitySHA256 string                `json:"eligibility_sha256,omitempty"`
	SnapshotSHA256    string                `json:"snapshot_sha256,omitempty"`
	Cases             map[string]replayCase `json:"cases"`
}

// snapshotItem is the exact text the server returned for one lead root.
type snapshotItem struct {
	Title         string `json:"title"`
	LeadSummary   string `json:"lead_summary"`
	Author        string `json:"author"`
	AuthorSlackID string `json:"author_slack_id"`
}

type snapshotCase struct {
	Query string                  `json:"query"`
	Items map[string]snapshotItem `json:"items"`
}

type snapshotFile struct {
	PreparedAt        string                  `json:"prepared_at"`
	CasesSHA256       string                  `json:"cases_sha256"`
	EligibilitySHA256 string                  `json:"eligibility_sha256"`
	Cases             map[string]snapshotCase `json:"cases"`
}

type evalPrepareOpts struct {
	BaseURL  string
	Refresh  bool
	Snapshot string // private snapshot dir; must not exist, outside the repo
	DataDir  string
	EnvPath  string
	Pool     *pgxpool.Pool
}

func runEvalPrepareCLI(cmd *cobra.Command, baseURL string, refresh bool, snapshot string) error {
	cmd.SilenceUsage = true
	dsn := evalDatabaseURL(".env")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL not found in .env")
	}
	pool, err := pgxpool.New(cmd.Context(), dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	return runEvalPrepare(cmd.Context(), cmd.OutOrStdout(), evalPrepareOpts{BaseURL: baseURL, Refresh: refresh, Snapshot: snapshot, DataDir: evalDataDir, EnvPath: ".env", Pool: pool})
}

// checkPrivateTarget validates a new private path (snapshot or sheet dir): it
// must not exist (Lstat, so a symlink counts), and its parent, resolved through
// symlinks, must be outside the repo. It returns the resolved path to create.
func checkPrivateTarget(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", categoryErr("out_path")
	}
	if _, err := os.Lstat(abs); err == nil {
		return "", categoryErr("out_exists")
	} else if !os.IsNotExist(err) {
		return "", categoryErr("out_stat")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", categoryErr("out_parent")
	}
	root, err := repoRoot()
	if err != nil {
		return "", categoryErr("repo_root")
	}
	if insideAnyCheckout(parent, root) {
		return "", categoryErr("out_inside_repo")
	}
	return filepath.Join(parent, filepath.Base(abs)), nil
}

// writePrivateFile creates path exclusively with mode 0600 after checking that
// nothing (not even a symlink) exists there.
func writePrivateFile(path string, data []byte) error {
	if _, err := os.Lstat(path); err == nil {
		return categoryErr("file_exists")
	} else if !os.IsNotExist(err) {
		return categoryErr("file_stat")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return categoryErr("file_create")
	}
	_, werr := f.Write(data)
	if cerr := f.Close(); werr != nil || cerr != nil {
		return categoryErr("file_write")
	}
	return nil
}

// askerQuery is the case query for lead mode: the resolve-asker body's query
// (not caseQuery, which prefers the admin query).
func askerQuery(c evalCase) string {
	if b, ok := c.Requests["resolve-asker"]; ok {
		if q, ok := b.Body["query"].(string); ok {
			return q
		}
	}
	return ""
}

// leadCategory maps a non-ok outcome to the fixed prepare category
// (lead_off, lead_http_error, lead_transport_error, lead_decode_error).
func leadCategory(outcome string) string {
	return "lead_" + strings.TrimPrefix(outcome, "lead_")
}

// leadSeedsPresent reports whether every canonical seed that exists in the
// graph (node_meta exists=true), or its thread root, is a hop-0 lead item.
// Seeds with exists=false or no meta entry are skipped: never ingested.
func leadSeedsPresent(seeds []string, items []leadItem, meta map[string]nodeMeta) bool {
	hop0 := map[string]bool{}
	for _, it := range items {
		if it.Hop == 0 {
			hop0[it.ID] = true
		}
	}
	for _, s := range seeds {
		m, ok := meta[s]
		if !ok || !m.Exists {
			continue
		}
		if !hop0[s] && !hop0[m.ThreadRoot] {
			return false
		}
	}
	return true
}

// validateLeadReplay is the shared strict reader for lead data: every case has
// an ok resolve-lead variant with a 2xx status and well-formed items, and the
// stored eligibility_sha256 equals a fresh recomputation. It returns the items
// per case.
func validateLeadReplay(replay replayFile, cases []loadedCase) (map[string][]leadItem, error) {
	byCase := make(map[string][]leadItem, len(cases))
	for _, c := range cases {
		v, ok := replay.Cases[c.ID].Variants[evalLeadVariant]
		if !ok {
			return nil, categoryErr(catLeadMissing)
		}
		if v.Outcome != outcomeOK || v.Status < 200 || v.Status >= 300 {
			return nil, categoryErr(catLeadOutcome)
		}
		if v.Items == nil || !leadItemsValid(*v.Items) {
			return nil, categoryErr(catLeadItems)
		}
		byCase[c.ID] = *v.Items
	}
	if replay.SnapshotSHA256 == "" {
		return nil, categoryErr("snapshot_missing")
	}
	if replay.EligibilitySHA256 == "" || leadEligibilityDigest(byCase) != replay.EligibilitySHA256 {
		return nil, categoryErr(catEligibilityMismatch)
	}
	return byCase, nil
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
	if b, ok := c.Requests["resolve-admin"]; ok {
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
	if o.Snapshot == "" {
		return fmt.Errorf("--snapshot is required (resolve-lead is replayed)")
	}
	snapDir, err := checkPrivateTarget(o.Snapshot)
	if err != nil {
		return fmt.Errorf("--snapshot: %w", err)
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
	leadItems := map[string][]leadItem{}
	snapCases := map[string]snapshotCase{}
	var ids []string
	buildRev := ""
	for _, c := range cases {
		rc := replayCase{CanonicalSeeds: seeds[c.ID], Variants: map[string]replayVariant{}}
		ids = append(ids, seeds[c.ID]...)
		ids = append(ids, labelIDs(c.evalCase)...)
		for _, name := range evalVariants {
			method, pathURL, body, headers, err := buildEvalRequest(c.evalCase, name)
			if err != nil {
				return err
			}
			call := client.do(method, pathURL, body, headers)
			if rev := call.Header.Get("X-Graph-Build-Rev"); rev != "" {
				buildRev = rev
			}
			if name == evalLeadVariant {
				outcome, parsed := parseLeadResponse(call)
				if outcome != outcomeOK {
					return fmt.Errorf("prepare: %s %s", leadCategory(outcome), c.ID)
				}
				items := make([]leadItem, 0, len(parsed))
				returned := make([]string, 0, len(parsed))
				snapItems := make(map[string]snapshotItem, len(parsed))
				for _, p := range parsed {
					items = append(items, p.Item)
					returned = append(returned, p.Item.ID)
					snapItems[p.Item.ID] = snapshotItem{Title: p.Title, LeadSummary: p.Summary, Author: p.Author, AuthorSlackID: p.AuthorSlackID}
				}
				leadItems[c.ID] = items
				snapCases[c.ID] = snapshotCase{Query: askerQuery(c.evalCase), Items: snapItems}
				rc.Variants[name] = replayVariant{Status: call.Status, Outcome: outcomeOK, RequestID: call.RequestID(), Returned: returned, Items: &items}
				ids = append(ids, returned...)
				fmt.Fprintf(out, "  %s: %d items\n", name, len(returned))
				continue
			}
			if call.Err != nil {
				return fmt.Errorf("%s %s: %w", c.ID, name, call.Err)
			}
			ret, err := parseEvalResponse(name, call)
			if err != nil {
				return fmt.Errorf("%s %s: %w", c.ID, name, err)
			}
			rc.Variants[name] = replayVariant{Status: call.Status, Outcome: outcomeOK, RequestID: ret.RequestID, Returned: ret.IDs}
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

	for _, c := range cases {
		if !leadSeedsPresent(seeds[c.ID], leadItems[c.ID], meta) {
			return fmt.Errorf("prepare: seed_mismatch %s", c.ID)
		}
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
	// Build revision: the response header when the server sends one, else the
	// BUILD_REV env var, else "unknown". (The server does not expose it today.)
	replay.BuildRev = unknownBuildRev(firstNonEmpty(buildRev, os.Getenv("BUILD_REV")))
	replay.CasesSHA256 = casesSHA(raws)
	replay.EligibilitySHA256 = leadEligibilityDigest(leadItems)
	snapJSON, err := json.MarshalIndent(snapshotFile{PreparedAt: replay.PreparedAt, CasesSHA256: replay.CasesSHA256, EligibilitySHA256: replay.EligibilitySHA256, Cases: snapCases}, "", "  ")
	if err != nil {
		return err
	}
	snapJSON = append(snapJSON, '\n')
	snapSum := sha256.Sum256(snapJSON)
	replay.SnapshotSHA256 = hex.EncodeToString(snapSum[:])
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
	// The snapshot is written last, after every case succeeded; a later
	// failure removes the directory this run created.
	if err := os.Mkdir(snapDir, 0o700); err != nil {
		return categoryErr("snapshot_mkdir")
	}
	if err := writePrivateFile(filepath.Join(snapDir, "snapshot.json"), snapJSON); err != nil {
		_ = os.RemoveAll(snapDir)
		return err
	}
	if err := writeFilesAtomic(files); err != nil {
		_ = os.RemoveAll(snapDir)
		return err
	}
	fmt.Fprintf(out, "wrote node_meta.json (%d nodes), replay.json, %d case files, private snapshot\n", len(meta), len(cases))
	return nil
}

type labelSheetOpts struct {
	CaseID      string
	All         bool
	OutDir      string
	Variant     string // "" or "all" (union of every variant) or resolve-lead
	Mask        bool
	SnapshotDir string
	DataDir     string
	EnvPath     string
	Pool        *pgxpool.Pool
}

func runLabelSheetCLI(cmd *cobra.Command, args []string, all bool, outDir, variant string, mask bool, snapshot string) error {
	cmd.SilenceUsage = true
	id := ""
	if len(args) == 1 {
		id = args[0]
	}
	o := labelSheetOpts{CaseID: id, All: all, OutDir: outDir, Variant: variant, Mask: mask, SnapshotDir: snapshot, DataDir: evalDataDir, EnvPath: ".env"}
	if variant == evalLeadVariant {
		// Lead sheets are built from the replay and the private snapshot: no database.
		return runLabelSheet(cmd.Context(), cmd.OutOrStdout(), o)
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
	o.Pool = pool
	return runLabelSheet(cmd.Context(), cmd.OutOrStdout(), o)
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

// insideAnyCheckout reports whether path lies inside the repo root or the hub
// (main) checkout this worktree belongs to. It compares by file identity
// (os.SameFile) while walking the parents, so case-insensitive file systems and
// alternative spellings of the same directory cannot slip through.
func insideAnyCheckout(path, root string) bool {
	roots := []string{root}
	if hub := hubCheckout(root); hub != "" {
		roots = append(roots, hub)
	}
	var infos []os.FileInfo
	for _, r := range roots {
		if fi, err := os.Stat(r); err == nil {
			infos = append(infos, fi)
		}
	}
	for dir := path; ; {
		if fi, err := os.Stat(dir); err == nil {
			for _, ri := range infos {
				if os.SameFile(fi, ri) {
					return true
				}
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

// hubCheckout returns the main checkout of the repository that root belongs to
// when root is a linked worktree (its .git is a file pointing into the hub's
// .git/worktrees), else "".
func hubCheckout(root string) string {
	raw, err := os.ReadFile(filepath.Join(root, ".git"))
	if err != nil {
		return ""
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir:")
	if !ok {
		return ""
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(root, gitdir)
	}
	// <hub>/.git/worktrees/<name>
	common := filepath.Dir(filepath.Dir(gitdir))
	if filepath.Base(common) != ".git" {
		return ""
	}
	return filepath.Dir(common)
}

func runLabelSheet(ctx context.Context, out io.Writer, o labelSheetOpts) error {
	switch o.Variant {
	case "", "all":
		if o.Mask || o.SnapshotDir != "" {
			return categoryErr("flag_invalid")
		}
		return runLabelSheetAll(ctx, out, o)
	case evalLeadVariant:
		return runLeadLabelSheet(out, o)
	}
	return categoryErr("variant_invalid")
}

// leadLinkBucket is the link bucket of a non-seed lead item (see the plan's
// Cohort buckets): structural, same_topic_hi, same_topic_lo, other_link, no_link.
func leadLinkBucket(links []leadLink) string {
	if len(links) == 0 {
		return "no_link"
	}
	hi, lo := false, false
	for _, l := range links {
		switch l.Kind {
		case "PART_OF", "REFERS_TO", "REFERENCES":
			return "structural"
		case "SAME_TOPIC":
			if l.Confidence >= 0.9 {
				hi = true
			} else {
				lo = true
			}
		}
	}
	switch {
	case hi:
		return "same_topic_hi"
	case lo:
		return "same_topic_lo"
	}
	return "other_link"
}

const leadSheetHeader = "MASKED: Slack mentions, author names, @-names, emails, phones, cards, refs with a digit (any token after a label word such as booking/payment/order). NOT masked: other names in prose, merchant/partner names. CONFIDENTIAL: do not commit or share."

// truncRunes cuts s to at most n runes, ending in "…" when cut.
func truncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// sheetText pseudonymizes the full string first, then cuts and flattens it.
func sheetText(ps *redact.Pseudonymizer, s string, limit int) string {
	s = truncRunes(ps.Apply(s), limit)
	return strings.Join(strings.Fields(s), " ")
}

type manifestRow struct {
	R      int    `json:"r"`
	NodeID string `json:"node_id"`
}

// runLeadLabelSheet writes masked sheets for the lead variant from replay.json
// and the private snapshot. Every refusal is a fixed category; no content.
func runLeadLabelSheet(out io.Writer, o labelSheetOpts) error {
	switch {
	case o.OutDir == "":
		return categoryErr("out_required")
	case !o.Mask:
		return categoryErr("mask_required")
	case o.SnapshotDir == "":
		return categoryErr("snapshot_required")
	case (o.CaseID == "") == !o.All:
		return categoryErr("case_selection")
	}
	target, err := checkPrivateTarget(o.OutDir)
	if err != nil {
		return err
	}
	replayData, err := os.ReadFile(filepath.Join(o.DataDir, "replay.json"))
	if err != nil {
		return categoryErr("replay_read")
	}
	var replay replayFile
	if json.Unmarshal(replayData, &replay) != nil {
		return categoryErr("replay_read")
	}
	cases, sum, err := loadEvalCases(o.DataDir)
	if err != nil {
		return categoryErr("cases_read")
	}
	if replay.CasesSHA256 != sum {
		return categoryErr("cases_sha256_mismatch")
	}
	lead, err := validateLeadReplay(replay, cases)
	if err != nil {
		return err
	}
	snapBytes, err := os.ReadFile(filepath.Join(o.SnapshotDir, "snapshot.json"))
	if err != nil {
		return categoryErr("snapshot_read")
	}
	if got := sha256.Sum256(snapBytes); replay.SnapshotSHA256 == "" || hex.EncodeToString(got[:]) != replay.SnapshotSHA256 {
		return categoryErr("snapshot_digest_mismatch")
	}
	var snap snapshotFile
	if json.Unmarshal(snapBytes, &snap) != nil {
		return categoryErr("snapshot_read")
	}
	if snap.EligibilitySHA256 != leadEligibilityDigest(lead) {
		return categoryErr(catEligibilityMismatch)
	}
	if snap.CasesSHA256 != sum {
		return categoryErr("cases_sha256_mismatch")
	}
	var chosen []loadedCase
	for _, c := range cases {
		if o.All || c.ID == o.CaseID {
			chosen = append(chosen, c)
		}
	}
	if len(chosen) == 0 {
		return categoryErr("case_unknown")
	}

	sheets := map[string][]byte{}
	manifest := map[string][]manifestRow{}
	for _, c := range chosen {
		items, sc := lead[c.ID], snap.Cases[c.ID]
		for _, it := range items {
			if _, ok := sc.Items[it.ID]; !ok {
				return categoryErr("snapshot_missing_row")
			}
		}
		ps := redact.NewPseudonymizer()
		for _, it := range items {
			row := sc.Items[it.ID]
			ps.AddPerson(row.AuthorSlackID, row.Author)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "# Label sheet: %s\n\n%s\n\n", c.ID, leadSheetHeader)
		fmt.Fprintf(&b, "Use case: %s\n\nSource thread: %s\n\n", sheetText(ps, c.UseCase, 200), c.ID)
		fmt.Fprintf(&b, "Query: %s\n", sheetText(ps, askerQuery(c.evalCase), 600))
		b.WriteString("\n## Seeds\n")
		rows := []manifestRow{}
		var rest []leadItem
		for _, it := range items {
			if it.Hop == 0 {
				row := sc.Items[it.ID]
				fmt.Fprintf(&b, "\n- title: %s\n  summary: %s\n", sheetText(ps, row.Title, 200), sheetText(ps, row.LeadSummary, 600))
			} else {
				rest = append(rest, it)
			}
		}
		fmt.Fprintf(&b, "\nCandidates: %d (seeds excluded).\n", len(rest))
		for k, it := range rest {
			row := sc.Items[it.ID]
			rows = append(rows, manifestRow{R: k + 1, NodeID: it.ID})
			fmt.Fprintf(&b, "\n## [r%d] %s\n\n- bucket: %s\n- hop: %d\n- authored_by_bot: %s\n- title: %s\n- summary: %s\n", k+1, it.ID, leadLinkBucket(it.Links), it.Hop, it.AuthoredByBot, sheetText(ps, row.Title, 200), sheetText(ps, row.LeadSummary, 600))
		}
		manifest[c.ID] = rows
		sheets[c.ID+".md"] = []byte(b.String())
	}
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	sheets["manifest.json"] = append(manifestJSON, '\n')

	if err := os.Mkdir(target, 0o700); err != nil {
		return categoryErr("out_mkdir")
	}
	names := make([]string, 0, len(sheets))
	for n := range sheets {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := writePrivateFile(filepath.Join(target, n), sheets[n]); err != nil {
			_ = os.RemoveAll(target)
			return err
		}
	}
	fmt.Fprintf(out, "wrote %d masked label sheets\n", len(chosen))
	return nil
}

func runLabelSheetAll(ctx context.Context, out io.Writer, o labelSheetOpts) error {
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
	if insideAnyCheckout(resolveExisting(o.OutDir), root) {
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
		for k, id := range nodes {
			row, err := readSheetNode(ctx, tx, id)
			if err != nil {
				return fmt.Errorf("%s %s: %w", c.ID, id, err)
			}
			fmt.Fprintf(&b, "\n## [r%d] %s\n\n- type: %s\n- is_reply: %t\n- title: %s\n- content: %s\n", k+1, id, row.Type, row.IsReply, row.Title, row.Content)
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
	if b, ok := c.Requests["resolve-admin"]; ok {
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
