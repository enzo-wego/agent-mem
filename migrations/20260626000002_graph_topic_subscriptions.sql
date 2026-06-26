-- +goose Up
-- Topic subscriptions: enzobot DMs the subscriber when a "hot" Slack thread
-- matching their topic appears (a senior person raised it, OR many people are
-- discussing it). topic_notifications dedupes so each hot thread fires once.

CREATE TABLE IF NOT EXISTS graph.topic_subscriptions (
  id                  BIGSERIAL PRIMARY KEY,
  subscriber_slack_id TEXT NOT NULL,                     -- Slack user id (U…) to DM
  topic               TEXT NOT NULL,                     -- keyword/phrase to match
  channel_filter      TEXT[] NOT NULL DEFAULT '{}',      -- empty = all channels; else slack channel ids
  min_participants    INTEGER NOT NULL DEFAULT 4,        -- "many humans discussing" trigger
  max_author_depth    SMALLINT NOT NULL DEFAULT 2,       -- seniority trigger: a post by depth ≤ this (0=CEO)
  active              BOOLEAN NOT NULL DEFAULT TRUE,
  created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_topic_subscriptions_active
  ON graph.topic_subscriptions(active) WHERE active;

CREATE TABLE IF NOT EXISTS graph.topic_notifications (
  subscription_id BIGINT NOT NULL REFERENCES graph.topic_subscriptions(id) ON DELETE CASCADE,
  root_node_id    TEXT NOT NULL,                         -- thread root node id (slack:<chan>:<thread_ts>)
  notified_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (subscription_id, root_node_id)
);

-- +goose Down
DROP TABLE IF EXISTS graph.topic_notifications;
DROP TABLE IF EXISTS graph.topic_subscriptions;
