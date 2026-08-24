-- +goose Up
CREATE INDEX IF NOT EXISTS eligibility_decisions_channel_message_idx
  ON graph.eligibility_decisions (channel_id, message_ts);

ALTER TABLE graph.eligibility_decisions
  ALTER COLUMN score DROP NOT NULL,
  ADD COLUMN decision_source TEXT NOT NULL DEFAULT 'scored'
    CHECK (decision_source IN ('scored', 'inherited_root'));

ALTER TABLE graph.eligibility_decisions
  ADD CONSTRAINT eligibility_decisions_source_score_check
    CHECK (
      (decision_source = 'scored' AND score IS NOT NULL) OR
      (decision_source = 'inherited_root' AND score IS NULL)
    ),
  ALTER COLUMN decision_source DROP DEFAULT;

-- +goose Down
DELETE FROM graph.eligibility_decisions
WHERE decision_source = 'inherited_root';

ALTER TABLE graph.eligibility_decisions
  ALTER COLUMN score SET NOT NULL,
  DROP COLUMN decision_source;

DROP INDEX IF EXISTS graph.eligibility_decisions_channel_message_idx;
