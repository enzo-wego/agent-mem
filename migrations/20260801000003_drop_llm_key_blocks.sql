-- +goose Up
-- Drop the key-block list. It existed to take a dead google/OpenRouter key out
-- of rotation, but agent-mem holds no provider keys any more — every LLM call
-- goes through llm-gateway, which owns credentials and failover. With the Go
-- readers gone (internal/database/llm_keys.go deleted) the table is inert; this
-- removes it so no stale fingerprints linger in the schema.
DROP TABLE IF EXISTS llm_key_blocks;

-- +goose Down
-- Deliberately empty. The table is only useful to code that rotates keys inside
-- agent-mem, and that code is gone for good; recreating it would just leave an
-- unused table. Key rotation now lives entirely in llm-gateway.
SELECT 1;
