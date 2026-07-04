-- +goose Up
-- Cached topic-judge verdicts. detect_hot_topics re-evaluates every hot thread
-- each 5-min tick; without a cache the nondeterministic LLM judge is re-rolled
-- ~288x/day and eventually flips a borderline "false" to "true" (late false
-- positives + wasted LLM calls). One verdict per (subscription, thread) at a
-- given thread size; the thread is re-judged only when its message count changes.
CREATE TABLE IF NOT EXISTS graph.topic_judgments (
  subscription_id BIGINT NOT NULL REFERENCES graph.topic_subscriptions(id) ON DELETE CASCADE,
  root_node_id    TEXT NOT NULL,
  msg_count       INTEGER NOT NULL,
  relevant        BOOLEAN NOT NULL,
  judged_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (subscription_id, root_node_id)
);

-- +goose Down
DROP TABLE IF EXISTS graph.topic_judgments;
