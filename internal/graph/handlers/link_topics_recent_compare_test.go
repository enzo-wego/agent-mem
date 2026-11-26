package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// oldRecentMarketSQL is the pre-HNSW query, copied verbatim from 8ff5d80.
const oldRecentMarketSQL = `
WITH src AS (
  SELECT ai.embedding AS emb, REPLACE(n.scope,'slack:','') AS ch,
         COALESCE(NULLIF(n.metadata->>'thread_ts',''),split_part(n.id,':',3)) AS tt
  FROM graph.nodes n JOIN graph.artifact_index ai ON ai.node_id=n.id
  WHERE n.id=$1 AND ai.embedding IS NOT NULL
),
sims AS (
  SELECT n.id,n.type,COALESCE(n.scope,'') AS scope,COALESCE(ai.summary,'') AS summary,
         COALESCE(ai.summary_kind,'') AS summary_kind,COALESCE(p.department,'') AS department,
         COALESCE(ts.kind,'') AS kind,1.0-(ai.embedding <=> src.emb) AS cosine,
         ts.markets IS NOT NULL AS has_markets,COALESCE(ts.markets,'{}') AS markets,
         activity.started,activity.ended
  FROM graph.artifact_index ai
  JOIN graph.nodes n ON n.id=ai.node_id
  LEFT JOIN graph.people p ON p.id=n.author_person_id
  CROSS JOIN src
  LEFT JOIN graph.thread_summaries ts ON ts.channel_id=REPLACE(n.scope,'slack:','')
    AND ts.thread_ts=COALESCE(NULLIF(n.metadata->>'thread_ts',''),split_part(n.id,':',3))
  CROSS JOIN LATERAL (
    SELECT
      CASE WHEN n.type IN ('slack','slack_thread') THEN
        COALESCE((SELECT min(COALESCE(to_timestamp(NULLIF(msg.metadata->>'ts','')::float8),msg.first_seen_at))
          FROM graph.nodes msg WHERE msg.scope=n.scope AND msg.deleted_at IS NULL
            AND COALESCE(NULLIF(msg.metadata->>'thread_ts',''),split_part(msg.id,':',3))=
                COALESCE(NULLIF(n.metadata->>'thread_ts',''),split_part(n.id,':',3))),n.first_seen_at)
      ELSE n.first_seen_at END AS started,
      CASE WHEN n.type IN ('slack','slack_thread') THEN
        COALESCE((SELECT max(COALESCE(to_timestamp(NULLIF(msg.metadata->>'ts','')::float8),msg.first_seen_at))
          FROM graph.nodes msg WHERE msg.scope=n.scope AND msg.deleted_at IS NULL
            AND COALESCE(NULLIF(msg.metadata->>'thread_ts',''),split_part(msg.id,':',3))=
                COALESCE(NULLIF(n.metadata->>'thread_ts',''),split_part(n.id,':',3))),n.updated_at)
      ELSE n.updated_at END AS ended
  ) activity
  WHERE ai.node_id<>$1 AND ai.embedding IS NOT NULL AND n.deleted_at IS NULL
    AND 1.0-(ai.embedding <=> src.emb) >= $2
    AND NOT (n.type IN ('slack','slack_thread') AND ai.summary_kind<>'thread_summary')
    AND n.type NOT IN ('slack_file','jira_attachment')
    AND length(TRIM(COALESCE(ai.summary,''))) >= $3
    AND COALESCE(ts.kind,'') <> 'chatter'
    AND NOT (n.type IN ('slack','slack_thread') AND n.scope LIKE 'slack:D%')
    AND NOT (n.type IN ('slack','slack_thread') AND REPLACE(n.scope,'slack:','')=src.ch
      AND COALESCE(NULLIF(n.metadata->>'thread_ts',''),split_part(n.id,':',3))=src.tt)
)
SELECT id,type,scope,summary,summary_kind,department,kind,cosine,has_markets,markets
FROM sims WHERE started <= $5 AND ended >= $4
ORDER BY cosine DESC,id ASC LIMIT $6`

type compareSource struct {
	id      string
	node    topicLinkNode
	markets []string
	lo, hi  time.Time
	oldRows []topicLinkCandidate
	oldGo   []topicLinkCandidate
	newRows []topicLinkCandidate
	newGo   []topicLinkCandidate
}

func compareOldRows(ctx context.Context, tx pgx.Tx, id string, lo, hi time.Time) ([]topicLinkCandidate, error) {
	rows, err := tx.Query(ctx, oldRecentMarketSQL, id, topicLinkRecentMinCosine, topicLinkMinSummaryChars, lo, hi, topicLinkRecentPrefilter)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []topicLinkCandidate
	for rows.Next() {
		var c topicLinkCandidate
		if err := rows.Scan(&c.NodeID, &c.Type, &c.Scope, &c.Summary, &c.SummaryKind, &c.Department, &c.Kind, &c.Cosine, &c.HasMarkets, &c.Markets); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type planStats struct {
	ms, buffers float64
	usesIndex   bool
	seqScan     bool
}

func walkPlan(n map[string]any, st *planStats) {
	if nt, _ := n["Node Type"].(string); nt == "Seq Scan" && n["Relation Name"] == "artifact_index" {
		st.seqScan = true
	}
	if idx, _ := n["Index Name"].(string); idx == "idx_artifact_index_embedding" {
		st.usesIndex = true
	}
	if kids, ok := n["Plans"].([]any); ok {
		for _, k := range kids {
			walkPlan(k.(map[string]any), st)
		}
	}
}

func explainOnce(ctx context.Context, tx pgx.Tx, sql string, args ...any) (planStats, error) {
	var raw []byte
	if err := tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+sql, args...).Scan(&raw); err != nil {
		return planStats{}, err
	}
	var doc []map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return planStats{}, err
	}
	plan := doc[0]["Plan"].(map[string]any)
	st := planStats{ms: doc[0]["Execution Time"].(float64)}
	st.buffers, _ = plan["Shared Hit Blocks"].(float64)
	rd, _ := plan["Shared Read Blocks"].(float64)
	st.buffers += rd
	walkPlan(plan, &st)
	return st, nil
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if len(s)%2 == 0 {
		return (s[len(s)/2-1] + s[len(s)/2]) / 2
	}
	return s[len(s)/2]
}

// timeQuery: 1 warm-up, then 5 runs; returns median ms, median buffers, and
// whether every run used the index / avoided an artifact_index seq scan.
func timeQuery(ctx context.Context, tx pgx.Tx, sql string, args ...any) (ms, buf float64, idx, seq bool, err error) {
	if _, err = explainOnce(ctx, tx, sql, args...); err != nil {
		return
	}
	idx = true
	var mss, bufs []float64
	for range 5 {
		var st planStats
		if st, err = explainOnce(ctx, tx, sql, args...); err != nil {
			return
		}
		mss, bufs = append(mss, st.ms), append(bufs, st.buffers)
		idx = idx && st.usesIndex
		seq = seq || st.seqScan
	}
	return median(mss), median(bufs), idx, seq, nil
}

func idsOf(c []topicLinkCandidate) map[string]int {
	m := make(map[string]int, len(c))
	for i, x := range c {
		m[x.NodeID] = i + 1
	}
	return m
}

// TestRecentMarketCompareHub compares old and new recent-market queries on a
// real database. Read-only; skipped unless AGENT_MEM_COMPARE_DSN is set.
func TestRecentMarketCompareHub(t *testing.T) {
	dsn := os.Getenv("AGENT_MEM_COMPARE_DSN")
	if dsn == "" {
		t.Skip("AGENT_MEM_COMPARE_DSN not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, s := range []string{
		fmt.Sprintf(`SET LOCAL hnsw.ef_search = %d`, topicLinkRecentEFSearch),
		`SET LOCAL hnsw.iterative_scan = relaxed_order`,
	} {
		if _, err := tx.Exec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := tx.Query(ctx, `SELECT ai.node_id FROM graph.artifact_index ai JOIN graph.nodes n
ON n.id=ai.node_id WHERE ai.embedding IS NOT NULL AND ai.summary_kind='thread_summary'
AND n.deleted_at IS NULL ORDER BY ai.refreshed_at DESC, ai.node_id LIMIT 300`)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	var cohort []*compareSource
	for _, id := range ids {
		if len(cohort) == 50 {
			break
		}
		node, err := loadTopicLinkSourceFrom(ctx, tx, id)
		if err != nil {
			continue
		}
		start, end, ok := nodeActivityWindowFrom(ctx, tx, id)
		if !ok {
			continue
		}
		cs := &compareSource{id: id, node: node,
			markets: resolveMarkets(node.Markets, node.HasMarkets, node.Summary),
			lo:      start.Add(-topicLinkRecentWindow), hi: end.Add(topicLinkRecentWindow)}
		if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = on`); err != nil {
			t.Fatal(err)
		}
		if cs.oldRows, err = compareOldRows(ctx, tx, id, cs.lo, cs.hi); err != nil {
			t.Fatal(err)
		}
		if cs.oldGo = selectRecentMarket(cs.oldRows, cs.markets, node); len(cs.oldGo) == 0 {
			continue
		}
		if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
			t.Fatal(err)
		}
		if cs.newRows, err = queryRecentMarketRows(ctx, tx, id, cs.lo, cs.hi, topicLinkRecentANNCandidates); err != nil {
			t.Fatal(err)
		}
		cs.newGo = selectRecentMarket(cs.newRows, cs.markets, node)
		cohort = append(cohort, cs)
	}
	if len(cohort) < 50 {
		t.Fatalf("insufficient evidence: %d qualifying sources within %d", len(cohort), len(ids))
	}

	var inter, total int
	for _, cs := range cohort {
		newGo := idsOf(cs.newGo)
		oldSQL, newSQL := idsOf(cs.oldRows), idsOf(cs.newRows)
		for _, c := range cs.oldGo {
			total++
			if _, ok := newGo[c.NodeID]; ok {
				inter++
				continue
			}
			_, inNew := newSQL[c.NodeID]
			t.Logf("MISS source=%s id=%s cosine=%.4f inNewSQL=%v oldSQLpos=%d newSQLpos=%d", cs.id, c.NodeID, c.Cosine, inNew, oldSQL[c.NodeID], newSQL[c.NodeID])
		}
	}
	recall := float64(inter) / float64(total)

	var oldMs, oldBuf, newMs, newBuf []float64
	for _, cs := range cohort[:10] {
		// Old query is timed with seq scans allowed (its production setting).
		if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = on`); err != nil {
			t.Fatal(err)
		}
		om, ob, _, _, err := timeQuery(ctx, tx, oldRecentMarketSQL, cs.id, topicLinkRecentMinCosine, topicLinkMinSummaryChars, cs.lo, cs.hi, topicLinkRecentPrefilter)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
			t.Fatal(err)
		}
		nm, nb, idx, seq, err := timeQuery(ctx, tx, recentMarketSQL, cs.id, topicLinkRecentMinCosine, topicLinkMinSummaryChars, cs.lo, cs.hi, topicLinkRecentPrefilter, topicLinkRecentANNCandidates)
		if err != nil {
			t.Fatal(err)
		}
		if !idx || seq {
			t.Errorf("source %s: new plan usesIndex=%v seqScan=%v", cs.id, idx, seq)
		}
		oldMs, oldBuf, newMs, newBuf = append(oldMs, om), append(oldBuf, ob), append(newMs, nm), append(newBuf, nb)
	}
	om, ob, nm, nb := median(oldMs), median(oldBuf), median(newMs), median(newBuf)

	var b strings.Builder
	fmt.Fprintf(&b, "\n| metric | old | new | ratio |\n|---|---|---|---|\n")
	fmt.Fprintf(&b, "| median execution ms (10 sources) | %.1f | %.1f | %.1fx |\n", om, nm, om/nm)
	fmt.Fprintf(&b, "| median root shared buffers | %.0f | %.0f | %.1fx |\n", ob, nb, ob/nb)
	fmt.Fprintf(&b, "| recall over %d sources (%d/%d old Go ids) | | %.4f | |\n", len(cohort), inter, total, recall)
	fmt.Fprintf(&b, "| K / ef_search | | %d / %d | |\n", topicLinkRecentANNCandidates, topicLinkRecentEFSearch)
	t.Log(b.String())

	if recall < 0.95 {
		t.Errorf("recall %.4f < 0.95", recall)
	}
	if om/nm < 10 || ob/nb < 10 {
		t.Errorf("speedup below 10x: time %.1fx buffers %.1fx", om/nm, ob/nb)
	}
}
