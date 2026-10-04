package handlers

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/scoring"
)

// hybridResult is the /search?match=hybrid result row. It is a separate struct
// so default-mode JSON keeps exactly today's key set.
type hybridResult struct {
	NodeID           string             `json:"node_id"`
	ID               string             `json:"id"`
	Type             string             `json:"type"`
	Title            string             `json:"title"`
	URL              string             `json:"url"`
	Summary          string             `json:"summary"`
	Decisions        []threadDecision   `json:"decisions,omitempty"`
	OpenQuestions    []string           `json:"open_questions,omitempty"`
	Score            float64            `json:"score"`
	ScoreBreakdown   scoring.Components `json:"score_breakdown"`
	Author           string             `json:"author,omitempty"`
	CreatedAt        time.Time          `json:"created_at"`
	Match            []string           `json:"match"`
	ThreadRoot       string             `json:"thread_root,omitempty"`
	Channel          string             `json:"channel,omitempty"`
	RootAuthor       string             `json:"root_author,omitempty"`
	MsgCount         int                `json:"msg_count,omitempty"`
	Participants     []string           `json:"participants,omitempty"`
	ParticipantCount int                `json:"participant_count,omitempty"`
	FirstTSMs        int64              `json:"first_ts_ms,omitempty"`
	LastTSMs         int64              `json:"last_ts_ms,omitempty"`
	PRCount          int                `json:"pr_count,omitempty"`
	PRs              []prRef            `json:"prs,omitempty"`
}

// hybridResponse is the /search?match=hybrid envelope: the page's
// {results,total,semantic_error?} plus the four-arm diagnostics.
type hybridResponse struct {
	Results       []hybridResult    `json:"results"`
	Total         int               `json:"total"`
	SemanticError string            `json:"semantic_error,omitempty"`
	Arms          []string          `json:"arms"`
	ArmErrors     map[string]string `json:"arm_errors,omitempty"`
	Window        *searchWindow     `json:"window,omitempty"`
	Query         string            `json:"query"`
}

// hybridDisplay turns the fused, boosted, truncated results into hybrid rows:
// match from per-arm ranks, and Slack thread / Jira PR enrichment for the kept
// rows only. scope is the request's ACL scope (nil = unfiltered).
func hybridDisplay(ctx context.Context, db *pgxpool.Pool, results []searchResult, scope any) ([]hybridResult, error) {
	out := make([]hybridResult, 0, len(results))
	if len(results) == 0 {
		return out, nil
	}
	ids := make([]string, len(results))
	for i, r := range results {
		ids[i] = r.NodeID
	}
	rows, err := db.Query(ctx, `
SELECT n.id, `+slackThreadRootSQL+`, LEFT(COALESCE(n.body,''), 200)
FROM graph.nodes n WHERE n.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	roots, bodies := map[string]string{}, map[string]string{}
	for rows.Next() {
		var id, body string
		var root *string
		if err := rows.Scan(&id, &root, &body); err != nil {
			rows.Close()
			return nil, err
		}
		if root != nil {
			roots[id] = *root
		}
		bodies[id] = body
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var chans, tss []string
	seen := map[string]bool{}
	for _, root := range roots {
		if c, t, ok := slackRootParts(root); ok && !seen[root] {
			seen[root] = true
			chans, tss = append(chans, c), append(tss, t)
		}
	}
	type tsum struct {
		summary, overview string
		dec, oq           []byte
	}
	sums := map[string]tsum{}
	if len(chans) > 0 {
		srows, err := db.Query(ctx, `
SELECT channel_id, thread_ts, summary, overview, decisions, open_questions
FROM graph.thread_summaries
WHERE (channel_id, thread_ts) IN (SELECT unnest($1::text[]), unnest($2::text[]))`, chans, tss)
		if err != nil {
			return nil, err
		}
		for srows.Next() {
			var c, t string
			var ts tsum
			if err := srows.Scan(&c, &t, &ts.summary, &ts.overview, &ts.dec, &ts.oq); err == nil {
				sums[c+":"+t] = ts
			}
		}
		srows.Close()
	}

	var slackRoots, jiraIDs []string
	for _, r := range results {
		res := hybridResult{
			NodeID: r.NodeID, ID: r.ID, Type: r.Type, Title: r.Title, URL: r.URL,
			Summary: r.Summary, Score: r.Score, ScoreBreakdown: r.ScoreBreakdown,
			Author: r.Author, CreatedAt: r.CreatedAt, Match: []string{},
		}
		for _, arm := range []string{armKeyword, armSemantic} {
			if _, ok := r.ScoreBreakdown.Ranks[arm]; ok {
				res.Match = append(res.Match, arm)
			}
		}
		if res.Title == "" {
			res.Title = firstLine(bodies[r.NodeID], 120)
		}
		if root := roots[r.NodeID]; root != "" {
			res.ThreadRoot = root
			if res.URL == "" {
				res.URL = slackPermalink(root)
			}
			slackRoots = append(slackRoots, root)
		} else {
			ms := r.CreatedAt.UnixMilli()
			res.FirstTSMs, res.LastTSMs = ms, ms
		}
		if r.Type == "jira" {
			jiraIDs = append(jiraIDs, r.NodeID)
		}
		out = append(out, res)
	}

	if len(slackRoots) > 0 {
		cards, err := threadCards(ctx, db, slackRoots)
		if err != nil {
			return nil, err
		}
		for i := range out {
			rid := out[i].ThreadRoot
			if rid == "" {
				continue
			}
			ch, t, _ := slackRootParts(rid)
			if ts, ok := sums[ch+":"+t]; ok {
				if ts.summary != "" {
					out[i].Title = ts.summary
				}
				if ts.overview != "" {
					out[i].Summary = ts.overview
				}
				out[i].Decisions, out[i].OpenQuestions = decodeThreadDecisions(ch, t, ts.dec, ts.oq)
			}
			if c, ok := cards[rid]; ok {
				out[i].Channel = c.Channel
				out[i].RootAuthor = c.RootAuthor
				out[i].MsgCount = c.MsgCount
				out[i].Participants = c.Participants
				out[i].ParticipantCount = c.ParticipantCount
				out[i].FirstTSMs = c.FirstTSMs
				out[i].LastTSMs = c.LastTSMs
			}
		}
	}

	if len(jiraIDs) > 0 {
		ss, _ := scope.([]string)
		visible := func(sc *string) bool {
			if scope == nil || sc == nil || *sc == "" {
				return true
			}
			for _, s := range ss {
				if s == *sc {
					return true
				}
			}
			return false
		}
		lists, err := jiraPRs(ctx, db, jiraIDs, visible)
		if err != nil {
			return nil, err
		}
		for i := range out {
			if l, ok := lists[out[i].NodeID]; ok && out[i].Type == "jira" {
				out[i].PRCount = l.Count
				out[i].PRs = l.PRs
			}
		}
	}
	return out, nil
}
