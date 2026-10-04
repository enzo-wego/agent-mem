-- Stratified sample of link_topics haiku verdicts with the current summaries
-- the judge reads. Read-only. One JSON object per line.
-- ponytail: summaries are today's, not the ones judged; restricting to
-- recent verdicts keeps the drift small.
SELECT setseed(0.42);
WITH pool AS (
  SELECT j.*, row_number() OVER (PARTITION BY j.same_topic ORDER BY random()) AS rn
  FROM graph.topic_link_judgments j
  WHERE j.judged_at >= now() - interval '35 days'
)
SELECT json_build_object(
  'source', p.source_node_id, 'target', p.target_node_id,
  'same_topic', p.same_topic, 'confidence', p.confidence, 'tag', p.tag,
  'a_type', na.type, 'a_dept', COALESCE(pa.department,''), 'a_summary', aa.summary,
  'b_type', nb.type, 'b_dept', COALESCE(pb.department,''), 'b_summary', ab.summary)
FROM pool p
JOIN graph.nodes na ON na.id = p.source_node_id
JOIN graph.nodes nb ON nb.id = p.target_node_id
JOIN graph.artifact_index aa ON aa.node_id = p.source_node_id
JOIN graph.artifact_index ab ON ab.node_id = p.target_node_id
LEFT JOIN graph.people pa ON pa.id = na.author_person_id
LEFT JOIN graph.people pb ON pb.id = nb.author_person_id
WHERE p.rn <= 1000
  AND length(trim(COALESCE(aa.summary,''))) >= 40
  AND length(trim(COALESCE(ab.summary,''))) >= 40;
