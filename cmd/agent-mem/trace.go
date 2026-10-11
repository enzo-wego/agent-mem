package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/agent-mem/agent-mem/internal/config"
	"github.com/agent-mem/agent-mem/internal/graph/handlers"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"
)

func newTraceCmd(getConfig func() *config.Config) *cobra.Command {
	cmd := &cobra.Command{Use: "trace", Short: "Inspect graph context traces"}
	var showBaseURL, statsBaseURL string
	show := &cobra.Command{Use: "show <id>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cfg := getConfig()
		return traceShow(cmd, cfg.DatabaseURL, args[0], writerCountersLine(showBaseURL, cfg.APIKey))
	}}
	show.Flags().StringVar(&showBaseURL, "base-url", "", "Serving worker URL, used to read the writer counters")
	var since string
	var route, caller string
	list := &cobra.Command{Use: "list", RunE: func(cmd *cobra.Command, _ []string) error {
		return traceList(cmd, getConfig().DatabaseURL, since, route, caller)
	}}
	list.Flags().StringVar(&since, "since", "24h", "Lookback duration")
	list.Flags().StringVar(&route, "route", "", "Route filter")
	list.Flags().StringVar(&caller, "caller", "", "Caller filter")
	var statsSince string
	stats := &cobra.Command{Use: "stats", RunE: func(cmd *cobra.Command, _ []string) error {
		cfg := getConfig()
		return traceStats(cmd, cfg.DatabaseURL, statsSince, writerCountersLine(statsBaseURL, cfg.APIKey))
	}}
	stats.Flags().StringVar(&statsSince, "since", "7d", "Lookback duration (Go duration or Nd days)")
	stats.Flags().StringVar(&statsBaseURL, "base-url", "", "Serving worker URL, used to read the writer counters")
	cmd.AddCommand(show, list, stats)
	return cmd
}
func tracePool(dsn string) (*pgxpool.Pool, error) { return pgxpool.New(context.Background(), dsn) }

// writerCountersLine reads the serving worker's process-local writer counters.
func writerCountersLine(baseURL, apiKey string) string {
	if strings.TrimSpace(baseURL) == "" {
		return "writer counters: unavailable (pass --base-url of the serving worker)"
	}
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(baseURL, "/")+"/api/graph/traces/stats", nil)
	if err != nil {
		return "writer counters: unavailable (" + err.Error() + ")"
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "writer counters: unavailable (" + err.Error() + ")"
	}
	defer resp.Body.Close()
	var stats struct{ Attempted, Written, Dropped, Failed uint64 }
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&stats) != nil {
		return "writer counters: unavailable (HTTP " + resp.Status + ")"
	}
	return fmt.Sprintf("writer counters (process-local since worker start): attempted=%d written=%d dropped=%d failed=%d", stats.Attempted, stats.Written, stats.Dropped, stats.Failed)
}
func traceShow(cmd *cobra.Command, dsn, id, counters string) error {
	p, err := tracePool(dsn)
	if err != nil {
		return err
	}
	defer p.Close()
	var created time.Time
	var route, rev, caller, acl string
	var status int
	var errText *string
	var request, items []byte
	var truncated bool
	var hash string
	var latency int
	err = p.QueryRow(context.Background(), `SELECT created_at,route,build_rev,caller,status,error,acl_mode,request,items,items_truncated,response_sha256,latency_ms FROM graph.context_traces WHERE id=$1`, id).Scan(&created, &route, &rev, &caller, &status, &errText, &acl, &request, &items, &truncated, &hash, &latency)
	if err == pgx.ErrNoRows {
		fmt.Fprintln(cmd.OutOrStdout(), "not found (never written, dropped, or older than 30 days)")
		fmt.Fprintln(cmd.OutOrStdout(), counters)
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s %s status=%d caller=%s acl=%s build=%s latency_ms=%d truncated=%t response_sha256=%s\nrequest: %s\n", id, route, status, caller, acl, rev, latency, truncated, hash, request)
	var rows []map[string]any
	_ = json.Unmarshal(items, &rows)
	var requestFields map[string]any
	_ = json.Unmarshal(request, &requestFields)
	mode := "ordinary"
	if fmt.Sprint(requestFields["match"]) == "hybrid" {
		mode = "hybrid"
	}
	for _, item := range rows {
		node := fmt.Sprint(item["node_id"])
		var bodyBytes *int
		if item["body_sha256"] != nil {
			n := int(item["body_bytes"].(float64))
			bodyBytes = &n
		}
		current, e := handlers.RenderTraceRepresentation(context.Background(), p, route, mode, node, bodyBytes)
		if e != nil {
			return e
		}
		if !current.Found {
			fmt.Fprintf(cmd.OutOrStdout(), "%v. %s — node no longer exists\n", item["rank"], node)
			continue
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%v. %s — current, may differ from what was returned\n", item["rank"], node)
		fmt.Fprintf(cmd.OutOrStdout(), "   title: %s\n", digestLabel(item["title_sha256"], current.Title))
		if item["summary_sha256"] != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "   summary: %s\n", digestLabel(item["summary_sha256"], current.Summary))
		}
		if item["body_sha256"] == nil {
			fmt.Fprintln(cmd.OutOrStdout(), "   body: not comparable (body not returned)")
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "   body: %s\n", digestLabel(item["body_sha256"], current.Body))
		}
	}
	return nil
}

func digestLabel(stored any, current string) string {
	sum := sha256.Sum256([]byte(current))
	if fmt.Sprint(stored) == hex.EncodeToString(sum[:]) {
		return "same as returned"
	}
	return "differs from returned"
}
func parseTraceSince(since string) (time.Duration, error) {
	if strings.HasSuffix(since, "d") {
		days, err := strconv.ParseFloat(strings.TrimSuffix(since, "d"), 64)
		if err != nil {
			return 0, err
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}
	return time.ParseDuration(since)
}

func traceList(cmd *cobra.Command, dsn, since, route, caller string) error {
	d, err := parseTraceSince(since)
	if err != nil {
		return err
	}
	p, err := tracePool(dsn)
	if err != nil {
		return err
	}
	defer p.Close()
	rows, err := p.Query(context.Background(), `SELECT id,created_at,route,caller,status,acl_mode FROM graph.context_traces WHERE created_at >= $1 AND ($2='' OR route=$2) AND ($3='' OR caller=$3) ORDER BY created_at DESC`, time.Now().Add(-d), route, caller)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, r, c, a string
		var at time.Time
		var status int
		if err := rows.Scan(&id, &at, &r, &c, &status, &a); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s %s %s caller=%s status=%d acl=%s\n", id, at.Format(time.RFC3339), r, c, status, a)
	}
	return rows.Err()
}
func traceStats(cmd *cobra.Command, dsn, since, counters string) error {
	d, err := parseTraceSince(since)
	if err != nil {
		return err
	}
	p, err := tracePool(dsn)
	if err != nil {
		return err
	}
	defer p.Close()
	rows, err := p.Query(context.Background(), `SELECT route,caller,acl_mode,status,count(*),percentile_disc(.5) WITHIN GROUP(ORDER BY latency_ms),percentile_disc(.95) WITHIN GROUP(ORDER BY latency_ms),avg(jsonb_array_length(items)) FROM graph.context_traces WHERE created_at >= $1 GROUP BY route,caller,acl_mode,status ORDER BY route,caller`, time.Now().Add(-d))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var route, caller, acl string
		var status, count, p50, p95 int
		var avg float64
		if err := rows.Scan(&route, &caller, &acl, &status, &count, &p50, &p95, &avg); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "route=%s caller=%s acl=%s status=%d calls=%d latency_p50=%d latency_p95=%d items_avg=%.2f\n", route, caller, acl, status, count, p50, p95, avg)
	}
	fmt.Fprintln(cmd.OutOrStdout(), counters)
	return rows.Err()
}
