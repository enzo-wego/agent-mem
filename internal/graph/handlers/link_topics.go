package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

const (
	topicLinkShortlistLimit = 12
	topicLinkMinFloor       = 0.65
)

type linkTopicsPayload struct {
	NodeID string `json:"node_id"`
	Force  bool   `json:"force,omitempty"`
}

type topicLinkNode struct {
	NodeID     string
	Type       string
	Summary    string
	Department string
}

type topicLinkCandidate struct {
	topicLinkNode
	Cosine float64
}

type topicLinkJudgment struct {
	SameTopic  bool
	Confidence float64
	Topic      string
	Why        string
}

// NewLinkTopicsHandler returns the job entry for exact-topic link generation.
func NewLinkTopicsHandler(deps Deps) jobs.Entry {
	return jobs.Entry{
		Handler:  linkTopicsHandler(deps),
		Systems:  []string{"gemini"},
		PoolSize: 2,
		Lease:    120 * time.Second,
	}
}

func linkTopicsHandler(deps Deps) jobs.Handler {
	return func(ctx context.Context, payload []byte) error {
		var p linkTopicsPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return fmt.Errorf("%w: link_topics unmarshal: %v", jobs.ErrFatal, err)
		}
		if p.NodeID == "" {
			return fmt.Errorf("%w: link_topics: node_id is required", jobs.ErrFatal)
		}
		if deps.Gemini == nil {
			return nil
		}

		source, err := loadTopicLinkSource(ctx, deps, p.NodeID)
		if err != nil || strings.TrimSpace(source.Summary) == "" {
			return err
		}
		cands, err := shortlistTopicLinks(ctx, deps, p.NodeID)
		if err != nil {
			return err
		}
		for _, cand := range cands {
			if strings.TrimSpace(cand.Summary) == "" {
				continue
			}
			from, to, sourceSummary, targetSummary := canonicalTopicPair(source.NodeID, cand.NodeID, source.Summary, cand.Summary)
			contentHash := topicLinkContentHash(from, to, sourceSummary, targetSummary)
			judgment, cached, err := cachedTopicLinkJudgment(ctx, deps, from, to, contentHash)
			if err != nil {
				return err
			}
			if !cached || p.Force {
				judgment = confirmTopicLink(ctx, deps, source, cand)
				if err := saveTopicLinkJudgment(ctx, deps, from, to, contentHash, judgment); err != nil {
					return err
				}
			}
			if judgment.SameTopic {
				if err := upsertSameTopicEdge(ctx, deps, from, to, judgment, cand.Cosine); err != nil {
					return err
				}
			} else if err := deleteSameTopicEdge(ctx, deps, from, to); err != nil {
				return err
			}
		}
		return nil
	}
}

func loadTopicLinkSource(ctx context.Context, deps Deps, nodeID string) (topicLinkNode, error) {
	var n topicLinkNode
	err := deps.DB.QueryRow(ctx, `
SELECT n.id, n.type, COALESCE(ai.summary,''), COALESCE(p.department,'')
FROM graph.nodes n
JOIN graph.artifact_index ai ON ai.node_id = n.id
LEFT JOIN graph.people p ON p.id = n.author_person_id
WHERE n.id=$1 AND n.deleted_at IS NULL`,
		nodeID,
	).Scan(&n.NodeID, &n.Type, &n.Summary, &n.Department)
	return n, err
}

func shortlistTopicLinks(ctx context.Context, deps Deps, nodeID string) ([]topicLinkCandidate, error) {
	rows, err := deps.DB.Query(ctx, `
WITH src AS (
  SELECT ai.embedding AS emb,
         REPLACE(n.scope,'slack:','') AS ch,
         COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3)) AS tt
  FROM graph.nodes n
  JOIN graph.artifact_index ai ON ai.node_id = n.id
  WHERE n.id = $1 AND ai.embedding IS NOT NULL
),
sims AS (
  SELECT n.id, n.type, COALESCE(ai.summary,'') AS summary, COALESCE(p.department,'') AS department,
         1.0 - (ai.embedding <=> src.emb) AS cosine,
         REPLACE(n.scope,'slack:','') AS ch,
         COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3)) AS tt
  FROM graph.artifact_index ai
  JOIN graph.nodes n ON n.id = ai.node_id
  LEFT JOIN graph.people p ON p.id = n.author_person_id
  CROSS JOIN src
  WHERE ai.node_id <> $1
    AND ai.embedding IS NOT NULL
    AND n.deleted_at IS NULL
    AND NOT (n.type IN ('slack','slack_thread') AND n.scope LIKE 'slack:D%')
    AND NOT (n.type IN ('slack','slack_thread')
      AND REPLACE(n.scope,'slack:','') = src.ch
      AND COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3)) = src.tt)
),
stats AS (
  SELECT avg(cosine) AS mean, stddev_pop(cosine) AS sigma FROM sims
)
SELECT id, type, summary, department, cosine
FROM sims, stats
WHERE cosine >= GREATEST(COALESCE(mean + 2 * sigma, $2), $2)
ORDER BY cosine DESC
LIMIT $3`,
		nodeID, topicLinkMinFloor, topicLinkShortlistLimit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []topicLinkCandidate
	for rows.Next() {
		var c topicLinkCandidate
		if err := rows.Scan(&c.NodeID, &c.Type, &c.Summary, &c.Department, &c.Cosine); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func canonicalTopicPair(a, b, aSummary, bSummary string) (from, to, fromSummary, toSummary string) {
	if a <= b {
		return a, b, aSummary, bSummary
	}
	return b, a, bSummary, aSummary
}

func topicLinkContentHash(from, to, fromSummary, toSummary string) string {
	h := sha256.Sum256([]byte(from + "\x00" + to + "\x00" + fromSummary + "\x00" + toSummary))
	return hex.EncodeToString(h[:])
}

func cachedTopicLinkJudgment(ctx context.Context, deps Deps, from, to, contentHash string) (topicLinkJudgment, bool, error) {
	var j topicLinkJudgment
	var cachedHash string
	err := deps.DB.QueryRow(ctx, `
SELECT content_hash, same_topic, confidence, topic, why
FROM graph.topic_link_judgments
WHERE source_node_id=$1 AND target_node_id=$2`,
		from, to,
	).Scan(&cachedHash, &j.SameTopic, &j.Confidence, &j.Topic, &j.Why)
	if err != nil {
		return topicLinkJudgment{}, false, nil
	}
	return j, cachedHash == contentHash, nil
}

func saveTopicLinkJudgment(ctx context.Context, deps Deps, from, to, contentHash string, j topicLinkJudgment) error {
	_, err := deps.DB.Exec(ctx, `
INSERT INTO graph.topic_link_judgments
  (source_node_id, target_node_id, content_hash, same_topic, confidence, topic, why, judged_at, machine_id)
VALUES ($1,$2,$3,$4,$5,$6,$7,NOW(),$8)
ON CONFLICT (source_node_id, target_node_id) DO UPDATE SET
  content_hash=EXCLUDED.content_hash,
  same_topic=EXCLUDED.same_topic,
  confidence=EXCLUDED.confidence,
  topic=EXCLUDED.topic,
  why=EXCLUDED.why,
  judged_at=NOW(),
  machine_id=EXCLUDED.machine_id`,
		from, to, contentHash, j.SameTopic, j.Confidence, j.Topic, j.Why, deps.MachineID,
	)
	return err
}

func confirmTopicLink(ctx context.Context, deps Deps, source topicLinkNode, cand topicLinkCandidate) topicLinkJudgment {
	const sys = `You decide whether two graph artifacts are substantively about the same exact topic.
Return JSON only: {"same_topic":true|false,"confidence":0.0-1.0,"topic":"short shared topic label","why":"one short factual reason"}.
Be strict: same words are not enough. Prefer false when teams or artifacts mention similar vocabulary for different work.`
	user := fmt.Sprintf(
		"Artifact A (%s, department %s):\n%s\n\nArtifact B (%s, department %s, cosine %.3f):\n%s",
		source.Type, blankAsUnknown(source.Department), source.Summary,
		cand.Type, blankAsUnknown(cand.Department), cand.Cosine, cand.Summary,
	)
	out, err := deps.Gemini.Generate(ctx, sys, user)
	if err != nil || strings.TrimSpace(out) == "" {
		return topicLinkJudgment{SameTopic: false}
	}
	var parsed struct {
		SameTopic  bool    `json:"same_topic"`
		Confidence float64 `json:"confidence"`
		Topic      string  `json:"topic"`
		Why        string  `json:"why"`
	}
	if json.Unmarshal([]byte(out), &parsed) != nil {
		return topicLinkJudgment{SameTopic: false}
	}
	return topicLinkJudgment{
		SameTopic:  parsed.SameTopic,
		Confidence: clamp01(parsed.Confidence),
		Topic:      firstLine(parsed.Topic, 120),
		Why:        firstLine(parsed.Why, 240),
	}
}

func upsertSameTopicEdge(ctx context.Context, deps Deps, from, to string, j topicLinkJudgment, cosine float64) error {
	meta, _ := json.Marshal(map[string]any{
		"confidence": j.Confidence,
		"topic":      j.Topic,
		"why":        j.Why,
		"method":     "cosine-shortlist + llm-confirm",
		"cosine":     cosine,
	})
	_, err := deps.DB.Exec(ctx, `
INSERT INTO graph.edges (from_node_id, to_node_id, kind, metadata, machine_id)
VALUES ($1,$2,'SAME_TOPIC',$3::jsonb,$4)
ON CONFLICT (from_node_id, to_node_id, kind) DO UPDATE SET
  metadata=EXCLUDED.metadata,
  machine_id=EXCLUDED.machine_id`,
		from, to, string(meta), deps.MachineID,
	)
	return err
}

func deleteSameTopicEdge(ctx context.Context, deps Deps, from, to string) error {
	_, err := deps.DB.Exec(ctx,
		`DELETE FROM graph.edges WHERE from_node_id=$1 AND to_node_id=$2 AND kind='SAME_TOPIC'`,
		from, to,
	)
	return err
}

func blankAsUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

func clamp01(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func enqueueLinkTopics(ctx context.Context, deps Deps, nodeID string, force bool) {
	if deps.DB == nil || nodeID == "" {
		return
	}
	if _, err := jobs.Enqueue(ctx, deps.DB, "link_topics", map[string]any{
		"node_id": nodeID,
		"force":   force,
	}, jobs.EnqueueOptions{Priority: 7, MachineID: deps.MachineID}); err != nil {
		deps.Logger.Warn().Err(err).Str("node_id", nodeID).Msg("enqueue link_topics failed")
	}
}
