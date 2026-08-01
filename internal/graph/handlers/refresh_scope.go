package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/agent-mem/agent-mem/internal/graph/ids"
	"github.com/agent-mem/agent-mem/internal/graph/jobs"
	"github.com/agent-mem/agent-mem/internal/llmjson"
)

var (
	cfPageIDRe = regexp.MustCompile(`/pages/(\d+)`)
	ghRepoRe   = regexp.MustCompile(`github\.com[/:]([^/\s]+/[^/\s#?]+)`)
)

// topicSource is one knowledge source attached to a subscription.
type topicSource struct {
	Type string `json:"type"` // "confluence" | "github"
	URL  string `json:"url"`
}

type refreshScopePayload struct {
	SubscriptionID int64 `json:"subscription_id"`
}

// scopeDistillCap bounds how much source text we feed the distiller in one pass.
const scopeDistillCap = 18000

// NewRefreshTopicScope returns the handler for refresh_topic_scope: it reads each
// of a subscription's sources (Confluence page-tree + repo *.md), ingests them as
// graph nodes, distills a scope_definition (judge guidance) + scope_summary
// (human-readable), and stores them on the subscription.
func NewRefreshTopicScope(deps Deps) jobs.Handler {
	log := deps.Logger
	return func(ctx context.Context, payload []byte) error {
		var p refreshScopePayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return fmt.Errorf("%w: refresh_topic_scope unmarshal: %v", jobs.ErrFatal, err)
		}

		var topic string
		var srcRaw []byte
		if err := deps.DB.QueryRow(ctx,
			`SELECT topic, sources FROM graph.topic_subscriptions WHERE id=$1`, p.SubscriptionID).
			Scan(&topic, &srcRaw); err != nil {
			return fmt.Errorf("%w: load subscription %d: %v", jobs.ErrFatal, p.SubscriptionID, err)
		}
		var sources []topicSource
		_ = json.Unmarshal(srcRaw, &sources)

		var titles []string // Confluence page titles
		var docs []string   // "path: excerpt" for repo markdown
		ingested := 0

		for _, s := range sources {
			switch s.Type {
			case "confluence":
				m := cfPageIDRe.FindStringSubmatch(s.URL)
				if m == nil {
					log.Warn().Str("url", s.URL).Msg("refresh_topic_scope: no page id in confluence url")
					continue
				}
				rootID := m[1]
				ingestConfluencePage(ctx, deps, rootID) // the page itself
				ingested++
				refs, err := deps.Fetchers.ConfluenceDescendants(ctx, rootID)
				if err != nil {
					log.Warn().Err(err).Str("page", rootID).Msg("refresh_topic_scope: descendants failed")
				}
				for _, ref := range refs {
					ingestConfluencePage(ctx, deps, ref.ID)
					ingested++
					if ref.Title != "" {
						titles = append(titles, ref.Title)
					}
				}
			case "github":
				m := ghRepoRe.FindStringSubmatch(s.URL)
				if m == nil {
					log.Warn().Str("url", s.URL).Msg("refresh_topic_scope: no repo in github url")
					continue
				}
				repo := strings.TrimSuffix(m[1], ".git")
				mds, err := deps.Fetchers.RepoMarkdown(ctx, repo, "")
				if err != nil {
					log.Warn().Err(err).Str("repo", repo).Msg("refresh_topic_scope: repo markdown failed")
				}
				for _, d := range mds {
					ingestRepoMarkdown(ctx, deps, repo, d.Path, d.Content)
					ingested++
					docs = append(docs, d.Path+": "+firstLine(d.Content, 600))
				}
			default:
				// Any other supported source (slack, jira, gws, wegohub,
				// claude_artifact, …): ingest the single URL via the standard
				// pipeline so it's searchable and counts toward the scope.
				if ingestURLSource(ctx, deps, s.URL) {
					ingested++
					docs = append(docs, s.Type+" source: "+s.URL)
				} else {
					log.Warn().Str("type", s.Type).Str("url", s.URL).Msg("refresh_topic_scope: unsupported source")
				}
			}
		}

		scopeDef, scopeSum := genScope(ctx, deps, topic, titles, docs)
		status := "ready"
		if scopeDef == "" {
			status = "error"
		}
		_, err := deps.DB.Exec(ctx,
			`UPDATE graph.topic_subscriptions
			 SET scope_definition=$2, scope_summary=$3, scope_status=$4, scope_refreshed_at=NOW()
			 WHERE id=$1`,
			p.SubscriptionID, scopeDef, scopeSum, status)
		log.Info().Int64("sub", p.SubscriptionID).Int("ingested", ingested).
			Int("titles", len(titles)).Int("docs", len(docs)).Str("status", status).
			Msg("refresh_topic_scope: done")
		return err
	}
}

// ingestConfluencePage upserts a cf node and enqueues fetch_body + index_artifact
// (the same pipeline ingest/url uses). The confluence fetcher resolves cf:<id>.
func ingestConfluencePage(ctx context.Context, deps Deps, pageID string) {
	id64, err := strconv.ParseInt(pageID, 10, 64)
	if err != nil {
		return
	}
	nodeID := ids.CFPage(id64)
	_, _ = deps.DB.Exec(ctx, `
		INSERT INTO graph.nodes (id, type, natural_key, body_revision, updated_at, machine_id)
		VALUES ($1, 'cf', $2, 0, NOW(), $3) ON CONFLICT (id) DO NOTHING`,
		nodeID, pageID, deps.MachineID)
	_, _ = jobs.Enqueue(ctx, deps.DB, "fetch_body",
		map[string]string{"node_id": nodeID, "source": "confluence"},
		jobs.EnqueueOptions{Priority: 6, MachineID: deps.MachineID})
	_, _ = jobs.Enqueue(ctx, deps.DB, "index_artifact",
		map[string]any{"node_id": nodeID, "force": false},
		jobs.EnqueueOptions{Priority: 7, MachineID: deps.MachineID})
}

// ingestURLSource ingests a single artifact URL (any source the fetchers
// support) via the standard pipeline: upsert a placeholder node, then fetch_body
// + index_artifact. Returns false if no fetcher claims the URL.
func ingestURLSource(ctx context.Context, deps Deps, rawURL string) bool {
	fetcher, ok := deps.Fetchers.For(rawURL)
	if !ok {
		return false
	}
	nodeID := nodeIDFromURL(rawURL, fetcher.Source())
	if nodeID == "" {
		return false
	}
	nodeType, _ := ids.ParseType(nodeID)
	naturalKey, _ := ids.ParseNaturalKey(nodeID)
	_, _ = deps.DB.Exec(ctx, `
		INSERT INTO graph.nodes (id, type, natural_key, url, body_revision, updated_at, machine_id)
		VALUES ($1, $2, $3, $4, 0, NOW(), $5) ON CONFLICT (id) DO NOTHING`,
		nodeID, string(nodeType), naturalKey, rawURL, deps.MachineID)
	_, _ = jobs.Enqueue(ctx, deps.DB, "fetch_body",
		map[string]string{"node_id": nodeID, "url": rawURL, "source": fetcher.Source()},
		jobs.EnqueueOptions{Priority: 6, MachineID: deps.MachineID})
	_, _ = jobs.Enqueue(ctx, deps.DB, "index_artifact",
		map[string]any{"node_id": nodeID, "force": false},
		jobs.EnqueueOptions{Priority: 7, MachineID: deps.MachineID})
	return true
}

// ingestRepoMarkdown stores a repo markdown file as a code_file node (body set
// directly) and enqueues index_artifact (which falls back to nodes.body).
func ingestRepoMarkdown(ctx context.Context, deps Deps, repo, path, content string) {
	nk := repo + "/" + path
	nodeID := ids.CodeFile(nk)
	url := fmt.Sprintf("https://github.com/%s/blob/HEAD/%s", repo, path)
	_, _ = deps.DB.Exec(ctx, `
		INSERT INTO graph.nodes (id, type, natural_key, url, title, body, body_revision, updated_at, machine_id)
		VALUES ($1, 'code_file', $2, $3, $4, $5, 0, NOW(), $6)
		ON CONFLICT (id) DO UPDATE SET title=EXCLUDED.title, body=EXCLUDED.body, url=EXCLUDED.url, updated_at=NOW()`,
		nodeID, nk, url, path, content, deps.MachineID)
	_, _ = jobs.Enqueue(ctx, deps.DB, "index_artifact",
		map[string]any{"node_id": nodeID, "force": true},
		jobs.EnqueueOptions{Priority: 7, MachineID: deps.MachineID})
}

// genScope asks the LLM to distill the sources into judge guidance + a summary.
func genScope(ctx context.Context, deps Deps, topic string, titles, docs []string) (string, string) {
	if deps.Gemini == nil {
		return "", ""
	}
	var b strings.Builder
	if len(titles) > 0 {
		b.WriteString("DOC PAGE TITLES:\n")
		for _, t := range titles {
			line := "- " + t + "\n"
			if b.Len()+len(line) > scopeDistillCap {
				break
			}
			b.WriteString(line)
		}
	}
	if len(docs) > 0 {
		b.WriteString("\nREPO DOC EXCERPTS:\n")
		for _, d := range docs {
			line := "- " + d + "\n"
			if b.Len()+len(line) > scopeDistillCap {
				break
			}
			b.WriteString(line)
		}
	}
	if strings.TrimSpace(b.String()) == "" {
		return "", ""
	}
	const sys = `You are given a team's documentation (page titles + repo doc excerpts) that
define a topic domain. Produce JSON only:
{"scope_definition":"precise guidance for deciding whether a Slack thread is about this domain: list the concrete in-scope areas/subsystems/partners, and note common adjacent things that are OUT of scope",
 "summary":"3-6 sentence plain-language summary of what this topic covers, for a human to review"}
Ground everything in the provided material; do not invent areas not implied by it.`
	out, err := deps.Gemini.Generate(ctx, sys, "TOPIC: "+topic+"\n\n"+b.String())
	if err != nil || out == "" {
		return "", ""
	}
	var parsed struct {
		ScopeDefinition string `json:"scope_definition"`
		Summary         string `json:"summary"`
	}
	if json.Unmarshal(llmjson.ExtractJSON(out), &parsed) != nil {
		return "", ""
	}
	return strings.TrimSpace(parsed.ScopeDefinition), strings.TrimSpace(parsed.Summary)
}
