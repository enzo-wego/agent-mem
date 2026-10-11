package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/agent-mem/agent-mem/internal/config"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"
)

func newTraceCmd(getConfig func() *config.Config) *cobra.Command {
	cmd := &cobra.Command{Use: "trace", Short: "Inspect graph context traces"}
	show := &cobra.Command{Use: "show <id>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error { return traceShow(cmd, getConfig().DatabaseURL, args[0]) }}
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
		return traceStats(cmd, getConfig().DatabaseURL, statsSince)
	}}
	stats.Flags().StringVar(&statsSince, "since", "168h", "Lookback duration")
	cmd.AddCommand(show, list, stats)
	return cmd
}
func tracePool(dsn string) (*pgxpool.Pool, error) { return pgxpool.New(context.Background(), dsn) }
func traceShow(cmd *cobra.Command, dsn, id string) error {
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
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s %s status=%d caller=%s acl=%s build=%s latency_ms=%d truncated=%t response_sha256=%s\nrequest: %s\n", id, route, status, caller, acl, rev, latency, truncated, hash, request)
	var rows []map[string]any
	_ = json.Unmarshal(items, &rows)
	for _, item := range rows {
		node := fmt.Sprint(item["node_id"])
		var title, body, summary string
		e := p.QueryRow(context.Background(), `SELECT COALESCE(n.title,''),COALESCE(n.body,''),COALESCE(ai.summary,'') FROM graph.nodes n LEFT JOIN graph.artifact_index ai ON ai.node_id=n.id WHERE n.id=$1 AND n.deleted_at IS NULL`, node).Scan(&title, &body, &summary)
		if e == pgx.ErrNoRows {
			fmt.Fprintf(cmd.OutOrStdout(), "%v. %s — node no longer exists\n", item["rank"], node)
			continue
		}
		if e != nil {
			return e
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%v. %s — current, may differ from what was returned\n", item["rank"], node)
		fmt.Fprintf(cmd.OutOrStdout(), "   title: %s\n", digestLabel(item["title_sha256"], title))
		if item["summary_sha256"] != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "   summary: %s\n", digestLabel(item["summary_sha256"], summary))
		}
		if item["body_sha256"] == nil {
			fmt.Fprintln(cmd.OutOrStdout(), "   body: not comparable (body not returned)")
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "   body: %s\n", digestLabel(item["body_sha256"], body))
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
func traceList(cmd *cobra.Command, dsn, since, route, caller string) error {
	d, err := time.ParseDuration(since)
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
func traceStats(cmd *cobra.Command, dsn, since string) error {
	d, err := time.ParseDuration(since)
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
	fmt.Fprintln(cmd.OutOrStdout(), "writer counters are process-local; GET /api/graph/traces/stats on the serving worker")
	return rows.Err()
}
