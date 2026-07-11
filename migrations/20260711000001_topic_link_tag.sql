-- +goose Up
-- Topic-rules tag (bug_incident, feature_business, …) the judge assigned when
-- ruling on a pair. Rules live in internal/graph/handlers/topic_rules.json.
ALTER TABLE graph.topic_link_judgments ADD COLUMN IF NOT EXISTS tag TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE graph.topic_link_judgments DROP COLUMN IF EXISTS tag;
