package main

import (
	"bytes"
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

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const evalCaseDir = "testdata/eval/graph_context/cases"
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
	relevance := &cobra.Command{Use: "relevance", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return runRelevance(cmd, baseURL, dryRun, jsonPath) }}
	relevance.Flags().StringVar(&baseURL, "base-url", "", "Graph API base URL")
	relevance.Flags().BoolVar(&dryRun, "dry-run", false, "Print requests without sending them")
	relevance.Flags().StringVar(&jsonPath, "json", "", "Write machine-readable results")
	cmd.AddCommand(relevance)
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
		c := evalCase{ID: p.ID, UseCase: p.UseCase, SourceThread: p.SourceThread, AskerSlackID: p.AskerSlackUID, AskerEEID: p.AskerEEID, NeedsContext: needsContext, Requests: map[string]evalRequest{"resolve-asker0": {Body: zero, Headers: map[string]string{}}, "resolve-asker": {Body: real, Headers: map[string]string{}}, "search-hybrid": {Params: params, Headers: map[string]string{}}}, Labels: map[string]evalLabels{"resolve": {}, "search": {}}, LabelReasons: map[string]string{}, LabelledBy: "none", CheckedBy: nil, Notes: notes}
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

func runRelevance(cmd *cobra.Command, baseURL string, dryRun bool, jsonPath string) error {
	paths, err := filepath.Glob(filepath.Join(evalCaseDir, "*.yaml"))
	if err != nil {
		return err
	}
	sort.Strings(paths)
	if len(paths) == 0 {
		return fmt.Errorf("no case files")
	}
	started := time.Now().UTC()
	casesHash := sha256.New()
	result := map[string]any{
		"build_rev":        unknownBuildRev(buildRev),
		"started_at":       started.Format(time.RFC3339Nano),
		"per_case":         []map[string]any{},
		"aggregate":        map[string]any{"scored": 0, "unlabelled": 0, "failed": 0},
		"request_ids":      []string{},
		"node_meta_sha256": fileSHA256(filepath.Join(filepath.Dir(evalCaseDir), "node_meta.json")),
	}
	client := http.DefaultClient
	for _, casePath := range paths {
		data, err := os.ReadFile(casePath)
		if err != nil {
			return err
		}
		_, _ = casesHash.Write(data)
		var c evalCase
		if err := yaml.Unmarshal(data, &c); err != nil {
			return fmt.Errorf("%s: %w", casePath, err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "case %s needs_context=%s", c.ID, c.NeedsContext)
		if c.LabelledBy == "none" {
			fmt.Fprint(cmd.OutOrStdout(), " unlabelled")
			result["aggregate"].(map[string]any)["unlabelled"] = result["aggregate"].(map[string]any)["unlabelled"].(int) + 1
		}
		fmt.Fprintln(cmd.OutOrStdout())
		for _, name := range []string{"resolve-asker0", "resolve-asker", "search-hybrid"} {
			req, ok := c.Requests[name]
			if !ok {
				return fmt.Errorf("%s: missing %s", c.ID, name)
			}
			method, pathURL := http.MethodPost, "/api/graph/resolve"
			var body []byte
			if name == "search-hybrid" {
				method = http.MethodGet
				values := url.Values{}
				keys := make([]string, 0, len(req.Params))
				for k := range req.Params {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					values.Set(k, fmt.Sprint(req.Params[k]))
				}
				pathURL = "/api/graph/search?" + values.Encode()
			} else {
				body, _ = json.Marshal(req.Body)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "  %s: %s %s headers={X-Graph-Caller:eval,X-Graph-No-Side-Effects:1} body=%s\n", name, method, pathURL, string(body))
			if dryRun {
				continue
			}
			if baseURL == "" {
				return fmt.Errorf("--base-url is required")
			}
			httpReq, _ := http.NewRequest(method, strings.TrimRight(baseURL, "/")+pathURL, bytes.NewReader(body))
			httpReq.Header.Set("X-Graph-Caller", "eval")
			httpReq.Header.Set("X-Graph-No-Side-Effects", "1")
			if key := os.Getenv("AGENT_MEM_API_KEY"); key != "" {
				httpReq.Header.Set("Authorization", "Bearer "+key)
			}
			resp, err := client.Do(httpReq)
			if err != nil {
				return err
			}
			responseBody, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				return readErr
			}
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return fmt.Errorf("%s %s: %s", c.ID, name, resp.Status)
			}
			var decoded struct {
				RequestID string            `json:"request_id"`
				Artifacts []json.RawMessage `json:"artifacts"`
				Results   []json.RawMessage `json:"results"`
				ArmErrors map[string]string `json:"arm_errors"`
			}
			if err := json.Unmarshal(responseBody, &decoded); err != nil {
				return fmt.Errorf("%s %s: decode response: %w", c.ID, name, err)
			}
			requestID := resp.Header.Get("X-Graph-Request-Id")
			if requestID == "" {
				requestID = decoded.RequestID
			}
			if requestID != "" {
				result["request_ids"] = append(result["request_ids"].([]string), requestID)
			}
			if rev := resp.Header.Get("X-Graph-Build-Rev"); rev != "" {
				result["build_rev"] = rev
			}
			count := len(decoded.Artifacts)
			if name == "search-hybrid" {
				count = len(decoded.Results)
			}
			entry := map[string]any{"case_id": c.ID, "variant": name, "request_id": requestID, "returned": count, "unlabelled": c.LabelledBy == "none"}
			if len(decoded.ArmErrors) > 0 {
				entry["arm_errors"] = decoded.ArmErrors
				entry["degraded"] = true
			}
			result["per_case"] = append(result["per_case"].([]map[string]any), entry)
		}
	}
	result["cases_sha256"] = hex.EncodeToString(casesHash.Sum(nil))
	if jsonPath != "" {
		encoded, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(jsonPath, append(encoded, '\n'), 0644); err != nil {
			return err
		}
	}
	return nil
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
