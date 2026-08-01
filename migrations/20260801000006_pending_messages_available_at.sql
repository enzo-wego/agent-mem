-- +goose Up
-- Keep a requeued message out of the worker's 1s claim loop until its retry
-- backoff expires instead of immediately burning the remaining attempt budget.
ALTER TABLE pending_messages
  ADD COLUMN available_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

-- +goose Down
-- An older worker reclaims every pending message immediately, so retain no
-- misleading retry schedule when rolling back to that behavior.
ALTER TABLE pending_messages DROP COLUMN available_at;
