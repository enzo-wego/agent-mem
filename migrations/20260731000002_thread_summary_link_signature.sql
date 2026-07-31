-- +goose Up
-- Track the linked-resource state a thread summary was generated for.
--
-- graph.thread_summaries.signature covers message count + newest updated_at, so
-- a thread whose messages are unchanged but whose linked ticket/doc TITLE just
-- landed looks identical and gets skipped. fetch_body worked around that by
-- force-re-enqueueing summaries, which bypassed both the pending-job dedup and
-- the signature check — 1,335 LLM calls/hour for 3 real updates.
--
-- This column closes the gap properly: it stores a hash of the "Linked
-- resources" prompt block, so a real title change regenerates and an unchanged
-- one skips. It is deliberately SEPARATE from signature: channels.go recomputes
-- signature cheaply on every channel view to detect staleness, and folding the
-- link hash into it would make every view see a mismatch and re-enqueue
-- everything — the same amplification by another route.
--
-- Existing rows default to '', which the handler reads as "unknown" rather than
-- "changed", so deploying this does NOT re-summarize every thread with links.
-- Those rows get the hash backfilled on their next visit, without an LLM call.
ALTER TABLE graph.thread_summaries
  ADD COLUMN IF NOT EXISTS link_signature TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE graph.thread_summaries DROP COLUMN IF EXISTS link_signature;
