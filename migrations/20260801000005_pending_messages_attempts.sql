-- +goose Up
-- Bound retries for message-specific failures so malformed LLM output or a
-- poisonous payload cannot loop forever, while still giving transient bad
-- responses another chance instead of permanently discarding the message.
ALTER TABLE pending_messages ADD COLUMN attempts INT NOT NULL DEFAULT 0;

-- +goose Down
-- An older worker neither reads nor maintains the retry budget, so retain no
-- misleading attempt history when rolling back to that behavior.
ALTER TABLE pending_messages DROP COLUMN attempts;
