package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const evalMeasuredRuns = 10
const evalLowSample = 30

type relevanceOpts struct {
	BaseURL  string
	DryRun   bool
	JSONPath string
	DataDir  string
	EnvPath  string
	// Quiet suppresses request URLs/bodies and raw error strings (transport
	// errors embed the full URL); failures are reported as a category only.
	Quiet bool
}

type evalRunResult struct {
	OK        bool              `json:"ok"`
	Ms        float64           `json:"ms"`
	Error     string            `json:"error,omitempty"`
	RequestID string            `json:"request_id,omitempty"`
	ArmErrors map[string]string `json:"arm_errors,omitempty"`
	Ret       evalReturn        `json:"-"`
}

type metricVal struct {
	Value float64
	NA    bool
}

type perCaseResult struct {
	CaseID          string          `json:"case_id"`
	Variant         string          `json:"variant"`
	NeedsContext    string          `json:"needs_context"`
	Unlabelled      bool            `json:"unlabelled"`
	Failed          bool            `json:"failed"`
	Degraded        bool            `json:"degraded"`
	Unstable        int             `json:"unstable_distinct_lists"`
	Returned        int             `json:"returned"`
	Primary         int             `json:"primary"`
	Linked          int             `json:"linked"`
	NonSeed         []string        `json:"non_seed_ids"`
	Precision       *float64        `json:"precision_at_10,omitempty"`
	Unlabelled10    *int            `json:"unlabelled_at_10,omitempty"`
	Recall          *float64        `json:"recall,omitempty"`
	Harmful         *int            `json:"harmful,omitempty"`
	Roots           *int            `json:"roots,omitempty"`
	SeedGroups      *int            `json:"seed_groups,omitempty"`
	UnreachableMust *int            `json:"unreachable_must,omitempty"`
	DupReplies      int             `json:"duplicate_replies"`
	UnknownMeta     int             `json:"unknown_meta"`
	Runs            []evalRunResult `json:"runs"`
}

type variantAgg struct {
	Cases       int      `json:"cases"`
	Scored      int      `json:"scored"`
	Unlabelled  int      `json:"unlabelled"`
	Failed      int      `json:"failed"`
	FailedIDs   []string `json:"failed_ids"`
	Degraded    int      `json:"degraded"`
	Unstable    int      `json:"unstable"`
	PrecisionN  int      `json:"precision_n"`
	PrecisionNA int      `json:"precision_na"`
	Precision   *float64 `json:"precision_at_10_mean"`
	Unl10       *float64 `json:"unlabelled_at_10_mean"`
	RecallN     int      `json:"recall_n"`
	RecallNA    int      `json:"recall_na"`
	Recall      *float64 `json:"recall_mean"`
	Harmful     *float64 `json:"harmful_mean"`
	DupReplies  *float64 `json:"duplicate_replies_mean"`
	UnknownMeta int      `json:"unknown_meta"`
	Samples     int      `json:"latency_samples"`
	P50Ms       *float64 `json:"latency_p50_ms"`
	P95Ms       *float64 `json:"latency_p95_ms"`
	LowSample   bool     `json:"low_sample"`
	FailedRuns  []string `json:"failed_runs"`
	Primary     int      `json:"primary_items"`
	Linked      int      `json:"linked_items"`
	WarmupOK    int      `json:"warmup_ok"`
	WarmupTotal int      `json:"warmup_total"`
	RunsOK      int      `json:"runs_ok"`
	RunsTotal   int      `json:"runs_total"`
}

func nearestRank(sorted []float64, p float64) float64 {
	n := len(sorted)
	rank := int(math.Ceil(p / 100 * float64(n)))
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1]
}

func mean(v []float64) *float64 {
	if len(v) == 0 {
		return nil
	}
	s := 0.0
	for _, x := range v {
		s += x
	}
	m := s / float64(len(v))
	return &m
}

func setOf(l []string) map[string]bool {
	m := make(map[string]bool, len(l))
	for _, s := range l {
		m[s] = true
	}
	return m
}

// quietEvalError carries only a failure category; it never wraps the cause.
type quietEvalError struct{ category string }

func (e *quietEvalError) Error() string { return "eval relevance failed: " + e.category }

// evalErrorCategory classifies a failed call without exposing URLs or bodies.
func evalErrorCategory(call evalCall) string {
	if call.Status == 0 {
		return "transport"
	}
	if call.Status < 200 || call.Status >= 300 {
		return fmt.Sprintf("http_%d", call.Status)
	}
	return "transport"
}

func runRelevance(cmd *cobra.Command, o relevanceOpts) error {
	err := runRelevanceInner(cmd, o)
	if err == nil || !o.Quiet {
		return err
	}
	var qe *quietEvalError
	if errors.As(err, &qe) {
		return qe
	}
	return &quietEvalError{category: "setup"}
}

func runRelevanceInner(cmd *cobra.Command, o relevanceOpts) error {
	cases, sum, err := loadEvalCases(o.DataDir)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	var meta map[string]nodeMeta
	metaPath := filepath.Join(o.DataDir, "node_meta.json")
	if !o.DryRun {
		if o.BaseURL == "" {
			return fmt.Errorf("--base-url is required")
		}
		data, err := os.ReadFile(metaPath)
		if err != nil {
			return fmt.Errorf("missing oracle file %s; run eval prepare: %w", metaPath, err)
		}
		if err := json.Unmarshal(data, &meta); err != nil {
			return fmt.Errorf("%s: %w", metaPath, err)
		}
	}
	client := newEvalClient(o.BaseURL, "")
	if !o.DryRun {
		client = newEvalClient(o.BaseURL, evalAPIKey(o.EnvPath))
	}
	buildRev := ""
	requestIDs := []string{}
	perCase := []perCaseResult{}
	aggs := map[string]*variantAgg{}
	lat := map[string][]float64{}
	dups := map[string][]float64{}
	type vals struct{ prec, unl, rec, harm []float64 }
	vv := map[string]*vals{}
	for _, v := range evalVariants {
		aggs[v] = &variantAgg{FailedIDs: []string{}, FailedRuns: []string{}}
		vv[v] = &vals{}
	}
	started := time.Now().UTC()
	reachableChecked := false

	for _, c := range cases {
		fmt.Fprintf(out, "case %s needs_context=%s", c.ID, c.NeedsContext)
		unlabelled := c.LabelledBy == "none"
		if unlabelled {
			fmt.Fprint(out, " unlabelled")
		}
		fmt.Fprintln(out)
		seeds := setOf(c.CanonicalSeeds)
		for _, name := range evalVariants {
			method, pathURL, body, headers, err := buildEvalRequest(c.evalCase, name)
			if err != nil {
				return err
			}
			if o.Quiet {
				fmt.Fprintf(out, "  %s\n", name)
			} else {
				fmt.Fprintf(out, "  %s: %s %s headers=%s body=%s\n", name, method, pathURL, formatEvalHeaders(evalRequestHeaders(headers)), string(body))
			}
			if o.DryRun {
				continue
			}
			agg := aggs[name]
			agg.Cases++
			res := perCaseResult{CaseID: c.ID, Variant: name, NeedsContext: c.NeedsContext, Unlabelled: unlabelled, NonSeed: []string{}, Runs: []evalRunResult{}}

			warm := client.do(method, pathURL, body, headers)
			if warm.RequestID() != "" {
				requestIDs = append(requestIDs, warm.RequestID())
			}
			agg.WarmupTotal++
			if evalCallOK(name, warm) {
				agg.WarmupOK++
			}
			if !reachableChecked {
				reachableChecked = true
				if warm.Status == 0 && warm.Err != nil {
					if o.Quiet {
						return &quietEvalError{category: evalErrorCategory(warm)}
					}
					return fmt.Errorf("unreachable server %s: %w", o.BaseURL, warm.Err)
				}
			}
			var first *evalReturn
			var firstLead []leadParsed
			distinct := map[string]bool{}
			okRuns := 0
			for i := 1; i <= evalMeasuredRuns; i++ {
				call := client.do(method, pathURL, body, headers)
				run := evalRunResult{Ms: float64(call.Dur.Microseconds()) / 1000}
				var ret evalReturn
				var leadRows []leadParsed
				decodeFailed := false
				quietCat := ""
				if call.Err == nil && name == evalLeadVariant {
					outcome, parsed := parseLeadResponse(call)
					switch outcome {
					case outcomeOK:
						leadRows = parsed
						ret = evalReturn{IDs: make([]string, 0, len(parsed)), RequestID: call.RequestID()}
						for _, p := range parsed {
							ret.IDs = append(ret.IDs, p.Item.ID)
							ret.Linked = append(ret.Linked, false)
						}
					case outcomeLeadOff:
						call.Err, quietCat = fmt.Errorf("%s", outcome), outcomeLeadOff
					default:
						call.Err, decodeFailed = fmt.Errorf("%s", outcome), true
					}
				} else if call.Err == nil {
					ret, err = parseEvalResponse(name, call)
					if err != nil {
						call.Err = err
						decodeFailed = true
					}
				}
				if id := firstNonEmpty(ret.RequestID, call.RequestID()); id != "" {
					requestIDs = append(requestIDs, id)
				}
				if call.Err != nil {
					run.Error = call.Err.Error()
					if o.Quiet {
						run.Error = evalErrorCategory(call)
						if decodeFailed {
							run.Error = "decode"
						}
						if quietCat != "" {
							run.Error = quietCat
						}
					}
					agg.FailedRuns = append(agg.FailedRuns, fmt.Sprintf("%s/%s#%d: %s", c.ID, name, i, run.Error))
					res.Runs = append(res.Runs, run)
					continue
				}
				run.OK, run.RequestID, run.ArmErrors, run.Ret = true, ret.RequestID, ret.ArmErrors, ret
				if rev := call.Header.Get("X-Graph-Build-Rev"); rev != "" {
					buildRev = rev
				}
				okRuns++
				lat[name] = append(lat[name], run.Ms)
				if len(ret.ArmErrors) > 0 {
					res.Degraded = true
				}
				distinct[strings.Join(ret.IDs, "\x00")] = true
				if first == nil {
					r := ret
					first = &r
					firstLead = leadRows
				}
				res.Runs = append(res.Runs, run)
			}
			agg.Samples += okRuns
			agg.RunsTotal += evalMeasuredRuns
			agg.RunsOK += okRuns
			if len(distinct) > 1 {
				res.Unstable = len(distinct)
				agg.Unstable++
			}
			if res.Degraded {
				agg.Degraded++
			}
			if first == nil {
				res.Failed = true
				agg.Failed++
				agg.FailedIDs = append(agg.FailedIDs, c.ID)
				fmt.Fprintf(out, "    result: FAILED (0/%d runs succeeded)\n", evalMeasuredRuns)
				perCase = append(perCase, res)
				continue
			}

			// Ranking metrics: first successful measured run, non-seed items only.
			seen := map[string]bool{}
			var items []string
			for i, id := range first.IDs {
				res.Returned++
				if i < len(first.Linked) && first.Linked[i] {
					res.Linked++
				} else {
					res.Primary++
				}
				if seeds[id] || seen[id] {
					continue
				}
				seen[id] = true
				items = append(items, id)
			}
			res.NonSeed = items
			agg.Primary += res.Primary
			agg.Linked += res.Linked
			for _, id := range items {
				m, ok := meta[id]
				if !ok {
					res.UnknownMeta++
				} else if m.IsReply {
					res.DupReplies++
				}
			}
			agg.UnknownMeta += res.UnknownMeta
			dups[name] = append(dups[name], float64(res.DupReplies))

			labelKey := "resolve"
			if name == "search-hybrid" {
				labelKey = "search"
			}
			line := fmt.Sprintf("    result: runs=%d/%d returned=%d (primary=%d linked=%d) non_seed=%d dup_replies=%d unknown_meta=%d", okRuns, evalMeasuredRuns, res.Returned, res.Primary, res.Linked, len(items), res.DupReplies, res.UnknownMeta)
			if unlabelled {
				agg.Unlabelled++
				line += " unlabelled"
			} else {
				agg.Scored++
				l := c.Labels[labelKey]
				must, may, mustNot := setOf(l.Must), setOf(l.May), setOf(l.MustNot)
				top := items
				if len(top) > 10 {
					top = top[:10]
				}
				hit, denom, unl := 0, 0, 0
				for _, id := range top {
					if must[id] {
						hit++
					}
					if !may[id] {
						denom++
					}
					if !must[id] && !may[id] && !mustNot[id] {
						unl++
					}
				}
				if denom == 0 {
					agg.PrecisionNA++
					line += " precision@10=n/a"
				} else {
					p := float64(hit) / float64(denom)
					res.Precision = &p
					vv[name].prec = append(vv[name].prec, p)
					line += fmt.Sprintf(" precision@10=%.3f", p)
				}
				res.Unlabelled10 = &unl
				vv[name].unl = append(vv[name].unl, float64(unl))
				rhit := 0
				harm := 0
				// Reply-only musts cannot be returned in lead mode (items are roots):
				// excluded from the lead recall numerator and denominator only.
				unreachable := map[string]bool{}
				if name == evalLeadVariant {
					for id := range must {
						if m, ok := meta[id]; ok && m.IsReply {
							unreachable[id] = true
						}
					}
					n := len(unreachable)
					res.UnreachableMust = &n
				}
				for _, id := range items {
					if must[id] && !unreachable[id] {
						rhit++
					}
					if mustNot[id] {
						harm++
					}
				}
				mustDen := len(must) - len(unreachable)
				if mustDen == 0 {
					agg.RecallNA++
					line += " recall=n/a"
				} else {
					r := float64(rhit) / float64(mustDen)
					res.Recall = &r
					vv[name].rec = append(vv[name].rec, r)
					line += fmt.Sprintf(" recall=%.3f", r)
				}
				res.Harmful = &harm
				vv[name].harm = append(vv[name].harm, float64(harm))
				line += fmt.Sprintf(" unlabelled@10=%d harmful=%d", unl, harm)
			}
			if name == evalLeadVariant {
				roots, seedGroups := len(firstLead), 0
				for _, p := range firstLead {
					if p.Item.Hop == 0 {
						seedGroups++
					}
				}
				res.Roots, res.SeedGroups = &roots, &seedGroups
				line += fmt.Sprintf(" roots=%d seed_groups=%d", roots, seedGroups)
				if res.UnreachableMust != nil {
					line += fmt.Sprintf(" unreachable_must=%d", *res.UnreachableMust)
				}
			}
			if res.Degraded {
				line += " degraded"
			}
			if res.Unstable > 0 {
				line += fmt.Sprintf(" unstable(%d distinct)", res.Unstable)
			}
			fmt.Fprintln(out, line)
			perCase = append(perCase, res)
		}
	}
	if o.DryRun {
		return nil
	}

	for _, v := range evalVariants {
		a := aggs[v]
		a.PrecisionN, a.RecallN = len(vv[v].prec), len(vv[v].rec)
		a.Precision, a.Unl10, a.Recall, a.Harmful = mean(vv[v].prec), mean(vv[v].unl), mean(vv[v].rec), mean(vv[v].harm)
		a.DupReplies = mean(dups[v])
		if l := lat[v]; len(l) > 0 {
			s := append([]float64(nil), l...)
			sort.Float64s(s)
			p50, p95 := nearestRank(s, 50), nearestRank(s, 95)
			a.P50Ms, a.P95Ms = &p50, &p95
		}
		a.LowSample = a.Samples < evalLowSample
	}

	fmt.Fprintln(out, "\nSummary (client round-trip latency; draft labels unless checked_by is set)")
	for _, v := range evalVariants {
		a := aggs[v]
		fmt.Fprintf(out, "\n%s\n", v)
		if a.Cases == 0 {
			fmt.Fprintln(out, "  no coverage")
			continue
		}
		fmt.Fprintf(out, "  cases=%d scored=%d unlabelled=%d failed=%d", a.Cases, a.Scored, a.Unlabelled, a.Failed)
		if len(a.FailedIDs) > 0 {
			fmt.Fprintf(out, " (%s)", strings.Join(a.FailedIDs, ", "))
		}
		fmt.Fprintf(out, " degraded=%d unstable=%d\n", a.Degraded, a.Unstable)
		if o.Quiet {
			fmt.Fprintf(out, "  warmup_ok=%d/%d runs_ok=%d/%d\n", a.WarmupOK, a.WarmupTotal, a.RunsOK, a.RunsTotal)
		}
		fmt.Fprintf(out, "  precision@10=%s (n=%d, n/a=%d)  recall=%s (n=%d, n/a=%d)\n", fmtMean(a.Precision), a.PrecisionN, a.PrecisionNA, fmtMean(a.Recall), a.RecallN, a.RecallNA)
		fmt.Fprintf(out, "  unlabelled@10=%s  harmful=%s  duplicate_replies=%s  unknown_meta=%d  primary=%d linked=%d\n", fmtMean(a.Unl10), fmtMean(a.Harmful), fmtMean(a.DupReplies), a.UnknownMeta, a.Primary, a.Linked)
		if a.P50Ms == nil {
			fmt.Fprintf(out, "  latency: no successful samples\n")
		} else {
			low := ""
			if a.LowSample {
				low = " low sample"
			}
			fmt.Fprintf(out, "  latency: p50=%.0fms p95=%.0fms samples=%d%s\n", *a.P50Ms, *a.P95Ms, a.Samples, low)
		}
		if len(a.FailedRuns) > 0 {
			fmt.Fprintf(out, "  failed runs (%d):\n", len(a.FailedRuns))
			for _, f := range a.FailedRuns {
				fmt.Fprintf(out, "    %s\n", f)
			}
		}
	}

	if o.JSONPath != "" {
		result := map[string]any{
			"build_rev":        unknownBuildRev(buildRev),
			"cases_sha256":     sum,
			"node_meta_sha256": fileSHA256(metaPath),
			"started_at":       started.Format(time.RFC3339Nano),
			"per_case":         perCase,
			"aggregate":        aggs,
			"request_ids":      requestIDs,
		}
		encoded, err := json.MarshalIndent(result, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(o.JSONPath, append(encoded, '\n'), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func fmtMean(v *float64) string {
	if v == nil {
		return "no coverage"
	}
	return fmt.Sprintf("%.3f", *v)
}

// evalCallOK reports whether a request (a warm-up or a measured run) succeeded:
// no transport/HTTP error and, for resolve-lead, outcome ok under the strict
// lead decoding.
func evalCallOK(name string, call evalCall) bool {
	if call.Err != nil {
		return false
	}
	if name == evalLeadVariant {
		outcome, _ := parseLeadResponse(call)
		return outcome == outcomeOK
	}
	_, err := parseEvalResponse(name, call)
	return err == nil
}
