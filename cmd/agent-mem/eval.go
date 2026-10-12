package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const evalDataDir = "testdata/eval/graph_context"
const evalCaseDir = evalDataDir + "/cases"
const probeManifestPath = "docs/ai/2026-10-11-graph-context-probes.yaml"

type evalRequest struct {
	Body    map[string]any    `yaml:"body,omitempty"`
	Params  map[string]any    `yaml:"params,omitempty"`
	Headers map[string]string `yaml:"headers"`
}
type evalLabels struct {
	Must    []string `yaml:"must"`
	May     []string `yaml:"may"`
	MustNot []string `yaml:"must_not"`
}
type evalCase struct {
	ID             string                 `yaml:"id"`
	UseCase        string                 `yaml:"use_case"`
	SourceThread   string                 `yaml:"source_thread"`
	AskerSlackID   string                 `yaml:"asker_slack_id"`
	AskerEEID      int                    `yaml:"asker_eeid"`
	NeedsContext   string                 `yaml:"needs_context"`
	CanonicalSeeds []string               `yaml:"canonical_seeds"`
	Requests       map[string]evalRequest `yaml:"requests"`
	Labels         map[string]evalLabels  `yaml:"labels"`
	LabelReasons   map[string]string      `yaml:"label_reasons"`
	LabelledBy     string                 `yaml:"labelled_by"`
	CheckedBy      *string                `yaml:"checked_by"`
	Notes          string                 `yaml:"notes"`
}

func newEvalCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "eval", Short: "Graph context evaluation tools"}
	cmd.AddCommand(&cobra.Command{Use: "import-probes", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error { return importProbes() }})
	var baseURL string
	var dryRun bool
	var jsonPath string
	var quiet bool
	relevance := &cobra.Command{Use: "relevance", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return runRelevance(cmd, relevanceOpts{BaseURL: baseURL, DryRun: dryRun, JSONPath: jsonPath, DataDir: evalDataDir, EnvPath: ".env", Quiet: quiet})
	}}
	relevance.Flags().BoolVar(&quiet, "quiet", false, "Do not print request URLs/bodies or raw error text; failures are reported as a category")
	relevance.Flags().StringVar(&baseURL, "base-url", "", "Graph API base URL")
	relevance.Flags().BoolVar(&dryRun, "dry-run", false, "Print requests without sending them")
	relevance.Flags().StringVar(&jsonPath, "json", "", "Write machine-readable results")
	cmd.AddCommand(relevance)

	var prepBase, prepSnapshot string
	var refresh bool
	prepare := &cobra.Command{Use: "prepare", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return runEvalPrepareCLI(cmd, prepBase, refresh, prepSnapshot)
	}}
	prepare.Flags().StringVar(&prepBase, "base-url", "", "Graph API base URL used for the replays")
	prepare.Flags().BoolVar(&refresh, "refresh", false, "Add node ids missing from an existing node_meta.json")
	prepare.Flags().StringVar(&prepSnapshot, "snapshot", "", "Private snapshot directory for the lead response text (required; must not exist, outside the repo)")
	cmd.AddCommand(prepare)

	var sheetOut, sheetVariant, sheetSnapshot string
	var sheetAll, sheetMask bool
	sheet := &cobra.Command{Use: "label-sheet [case-id]", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return runLabelSheetCLI(cmd, args, sheetAll, sheetOut, sheetVariant, sheetMask, sheetSnapshot)
	}}
	sheet.Flags().StringVar(&sheetOut, "out", "", "Output directory (required, outside the repo)")
	sheet.Flags().BoolVar(&sheetAll, "all", false, "Write a sheet for every case")
	sheet.Flags().StringVar(&sheetVariant, "variant", "all", "all (union of every variant) or resolve-lead")
	sheet.Flags().BoolVar(&sheetMask, "mask", false, "Pseudonymize the text (required for --variant resolve-lead)")
	sheet.Flags().StringVar(&sheetSnapshot, "snapshot", "", "Private snapshot directory (--variant resolve-lead)")
	cmd.AddCommand(sheet)

	var asmDraft, asmReview, asmOwner, asmOut string
	assemble := &cobra.Command{Use: "relation-assemble", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cmd.SilenceUsage = true
		return runRelationAssemble(cmd.OutOrStdout(), relationAssembleOpts{Draft: asmDraft, Review: asmReview, Owner: asmOwner, Out: asmOut, DataDir: evalDataDir})
	}}
	assemble.Flags().StringVar(&asmDraft, "draft", "", "Draft rows file (required)")
	assemble.Flags().StringVar(&asmReview, "review", "", "Independent review rows file (required)")
	assemble.Flags().StringVar(&asmOwner, "owner", "", "Owner override rows file (optional)")
	assemble.Flags().StringVar(&asmOut, "out", "", "Output relation_labels.json path (required, must not exist)")
	cmd.AddCommand(assemble)

	var chkFile string
	check := &cobra.Command{Use: "relation-check", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cmd.SilenceUsage = true
		return runRelationCheck(cmd.OutOrStdout(), chkFile, evalDataDir)
	}}
	check.Flags().StringVar(&chkFile, "file", evalDataDir+"/relation_labels.json", "relation_labels.json to check")
	cmd.AddCommand(check)
	return cmd
}

func importProbes() error {
	data, err := os.ReadFile(probeManifestPath)
	if err != nil {
		return err
	}
	var manifest struct {
		HybridTypes string `yaml:"hybrid_types"`
		Probes      []struct {
			ID            string           `yaml:"id"`
			UseCase       string           `yaml:"use_case"`
			Label         string           `yaml:"label"`
			SourceThread  string           `yaml:"source_thread"`
			AskerSlackUID string           `yaml:"asker_slack_uid"`
			AskerEEID     int              `yaml:"asker_eeid"`
			Resolve       map[string]any   `yaml:"resolve_request"`
			Hybrid        map[string]any   `yaml:"hybrid_request"`
			Variants      []map[string]any `yaml:"variants"`
		} `yaml:"probes"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return err
	}
	if err := os.MkdirAll(evalCaseDir, 0755); err != nil {
		return err
	}
	for _, p := range manifest.Probes {
		askers, ok := p.Resolve["asker_eeid"].([]any)
		if !ok || len(askers) != 2 {
			return fmt.Errorf("%s: asker_eeid must contain two values", p.ID)
		}
		base := cloneMap(p.Resolve)
		delete(base, "asker_eeid")
		deleteNil(base)
		zero := cloneMap(base)
		zero["asker_eeid"] = numberInt(askers[0])
		real := cloneMap(base)
		real["asker_eeid"] = numberInt(askers[1])
		params := cloneMap(p.Hybrid)
		delete(params, "X-Asker-User")
		deleteNil(params)
		notes := p.Label
		if len(p.Variants) > 0 {
			b, _ := yaml.Marshal(p.Variants)
			notes += "; manifest variants: " + strings.TrimSpace(string(b))
		}
		needsContext := "yes"
		if p.UseCase == "UC9" {
			needsContext = "no"
		}
		c := evalCase{ID: p.ID, UseCase: p.UseCase, SourceThread: p.SourceThread, AskerSlackID: p.AskerSlackUID, AskerEEID: p.AskerEEID, NeedsContext: needsContext, Requests: map[string]evalRequest{"resolve-admin": {Body: zero, Headers: map[string]string{}}, "resolve-asker": {Body: real, Headers: map[string]string{}}, "search-hybrid": {Params: params, Headers: map[string]string{}}}, Labels: map[string]evalLabels{"resolve": {}, "search": {}}, LabelReasons: map[string]string{}, LabelledBy: "none", CheckedBy: nil, Notes: notes}
		out, err := yaml.Marshal(c)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(evalCaseDir, p.ID+".yaml"), out, 0644); err != nil {
			return err
		}
	}
	return nil
}
func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
func deleteNil(m map[string]any) {
	for k, v := range m {
		if v == nil {
			delete(m, k)
		}
	}
}
func numberInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	default:
		return 0
	}
}

func unknownBuildRev(rev string) string {
	if strings.TrimSpace(rev) == "" {
		return "unknown"
	}
	return rev
}

func fileSHA256(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
