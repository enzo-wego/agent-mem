-- +goose Up
-- agent-mem initial schema
-- Requires: pgvector extension

CREATE EXTENSION IF NOT EXISTS vector;

-- Schema version tracking (used for sync metadata)
CREATE TABLE IF NOT EXISTS schema_versions (
    version     INTEGER PRIMARY KEY,
    applied_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    description TEXT
);

-- SDK session tracking
CREATE TABLE IF NOT EXISTS sdk_sessions (
    id                  SERIAL PRIMARY KEY,
    content_session_id  TEXT UNIQUE NOT NULL,
    memory_session_id   TEXT UNIQUE,
    project             TEXT NOT NULL,
    user_prompt         TEXT,
    started_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at_epoch    BIGINT NOT NULL DEFAULT (EXTRACT(EPOCH FROM now())::BIGINT),
    completed_at        TIMESTAMPTZ,
    completed_at_epoch  BIGINT,
    status              TEXT NOT NULL DEFAULT 'active'
                        CHECK (status IN ('active', 'completed', 'failed')),
    sync_id             TEXT UNIQUE,
    sync_version        INTEGER NOT NULL DEFAULT 0,
    sync_source         TEXT
);

CREATE INDEX IF NOT EXISTS idx_sdk_sessions_project ON sdk_sessions (project);
CREATE INDEX IF NOT EXISTS idx_sdk_sessions_status ON sdk_sessions (status);
CREATE INDEX IF NOT EXISTS idx_sdk_sessions_started_at ON sdk_sessions (started_at_epoch DESC);

-- Observations extracted by Gemini
CREATE TABLE IF NOT EXISTS observations (
    id                  SERIAL PRIMARY KEY,
    memory_session_id   TEXT NOT NULL REFERENCES sdk_sessions (memory_session_id) ON DELETE CASCADE,
    project             TEXT NOT NULL,
    type                TEXT NOT NULL
                        CHECK (type IN ('decision', 'bugfix', 'feature', 'refactor', 'discovery')),
    title               TEXT,
    subtitle            TEXT,
    narrative           TEXT,
    text                TEXT,
    facts               JSONB DEFAULT '[]'::JSONB,
    concepts            JSONB DEFAULT '[]'::JSONB,
    files_read          JSONB DEFAULT '[]'::JSONB,
    files_modified      JSONB DEFAULT '[]'::JSONB,
    discovery_tokens    INTEGER DEFAULT 0,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at_epoch    BIGINT NOT NULL DEFAULT (EXTRACT(EPOCH FROM now())::BIGINT),
    embedding           vector(768),
    search_vector       tsvector GENERATED ALWAYS AS (
        setweight(to_tsvector('english', COALESCE(title, '')), 'A') ||
        setweight(to_tsvector('english', COALESCE(subtitle, '')), 'B') ||
        setweight(to_tsvector('english', COALESCE(narrative, '')), 'C') ||
        setweight(to_tsvector('english', COALESCE(text, '')), 'D')
    ) STORED,
    sync_id             TEXT UNIQUE,
    sync_version        INTEGER NOT NULL DEFAULT 0,
    sync_source         TEXT
);

CREATE INDEX IF NOT EXISTS idx_observations_session ON observations (memory_session_id);
CREATE INDEX IF NOT EXISTS idx_observations_project ON observations (project);
CREATE INDEX IF NOT EXISTS idx_observations_type ON observations (type);
CREATE INDEX IF NOT EXISTS idx_observations_created_at ON observations (created_at_epoch DESC);
CREATE INDEX IF NOT EXISTS idx_observations_search ON observations USING GIN (search_vector);
CREATE INDEX IF NOT EXISTS idx_observations_embedding ON observations USING hnsw (embedding vector_cosine_ops);

-- Session summaries
CREATE TABLE IF NOT EXISTS session_summaries (
    id                  SERIAL PRIMARY KEY,
    memory_session_id   TEXT UNIQUE NOT NULL REFERENCES sdk_sessions (memory_session_id) ON DELETE CASCADE,
    project             TEXT NOT NULL,
    request             TEXT,
    investigated        TEXT,
    learned             TEXT,
    completed           TEXT,
    next_steps          TEXT,
    notes               TEXT,
    files_read          JSONB DEFAULT '[]'::JSONB,
    files_edited        JSONB DEFAULT '[]'::JSONB,
    discovery_tokens    INTEGER DEFAULT 0,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at_epoch    BIGINT NOT NULL DEFAULT (EXTRACT(EPOCH FROM now())::BIGINT),
    embedding           vector(768),
    search_vector       tsvector GENERATED ALWAYS AS (
        setweight(to_tsvector('english', COALESCE(request, '')), 'A') ||
        setweight(to_tsvector('english', COALESCE(investigated, '')), 'B') ||
        setweight(to_tsvector('english', COALESCE(learned, '')), 'B') ||
        setweight(to_tsvector('english', COALESCE(completed, '')), 'C') ||
        setweight(to_tsvector('english', COALESCE(next_steps, '')), 'C') ||
        setweight(to_tsvector('english', COALESCE(notes, '')), 'D')
    ) STORED,
    sync_id             TEXT UNIQUE,
    sync_version        INTEGER NOT NULL DEFAULT 0,
    sync_source         TEXT
);

CREATE INDEX IF NOT EXISTS idx_session_summaries_project ON session_summaries (project);
CREATE INDEX IF NOT EXISTS idx_session_summaries_created_at ON session_summaries (created_at_epoch DESC);
CREATE INDEX IF NOT EXISTS idx_session_summaries_search ON session_summaries USING GIN (search_vector);
CREATE INDEX IF NOT EXISTS idx_session_summaries_embedding ON session_summaries USING hnsw (embedding vector_cosine_ops);

-- User prompts
CREATE TABLE IF NOT EXISTS user_prompts (
    id                  SERIAL PRIMARY KEY,
    content_session_id  TEXT NOT NULL REFERENCES sdk_sessions (content_session_id) ON DELETE CASCADE,
    project             TEXT NOT NULL,
    prompt              TEXT NOT NULL,
    prompt_number       INTEGER NOT NULL DEFAULT 1,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at_epoch    BIGINT NOT NULL DEFAULT (EXTRACT(EPOCH FROM now())::BIGINT),
    embedding           vector(768),
    search_vector       tsvector GENERATED ALWAYS AS (
        to_tsvector('english', prompt)
    ) STORED,
    sync_id             TEXT UNIQUE,
    sync_version        INTEGER NOT NULL DEFAULT 0,
    sync_source         TEXT
);

CREATE INDEX IF NOT EXISTS idx_user_prompts_session ON user_prompts (content_session_id);
CREATE INDEX IF NOT EXISTS idx_user_prompts_project ON user_prompts (project);
CREATE INDEX IF NOT EXISTS idx_user_prompts_search ON user_prompts USING GIN (search_vector);
CREATE INDEX IF NOT EXISTS idx_user_prompts_embedding ON user_prompts USING hnsw (embedding vector_cosine_ops);

-- Async work queue for pending messages
CREATE TABLE IF NOT EXISTS pending_messages (
    id                  SERIAL PRIMARY KEY,
    content_session_id  TEXT NOT NULL,
    message_type        TEXT NOT NULL
                        CHECK (message_type IN ('observation', 'summary', 'continuation')),
    payload             JSONB NOT NULL,
    status              TEXT NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending', 'processing', 'completed', 'failed')),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at        TIMESTAMPTZ,
    error               TEXT
);

CREATE INDEX IF NOT EXISTS idx_pending_messages_status ON pending_messages (status, created_at);

-- Record initial schema version
INSERT INTO schema_versions (version, description)
VALUES (1, 'Initial schema with pgvector support')
ON CONFLICT (version) DO NOTHING;

-- +goose Down
DROP TABLE IF EXISTS pending_messages;
DROP TABLE IF EXISTS user_prompts;
DROP TABLE IF EXISTS session_summaries;
DROP TABLE IF EXISTS observations;
DROP TABLE IF EXISTS sdk_sessions;
DROP TABLE IF EXISTS schema_versions;
DROP EXTENSION IF EXISTS vector;
