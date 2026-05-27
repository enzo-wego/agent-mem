-- +goose Up
-- graph schema: nodes, edges, people, artifact_index, artifact_bodies,
-- jobs, slack_groups, entities, identity_map, user_affinity_config

CREATE EXTENSION IF NOT EXISTS citext;

CREATE SCHEMA IF NOT EXISTS graph;
GRANT USAGE ON SCHEMA graph TO CURRENT_USER;

-- pgvector is already enabled in public; reuse it

-- graph.people must be created before graph.nodes (FK reference)
CREATE TABLE graph.people (
  id                   BIGSERIAL PRIMARY KEY,
  eeid                 INTEGER UNIQUE,            -- NULL until BambooHR matches
  email                CITEXT UNIQUE,
  display_name         TEXT NOT NULL,
  slack_user_id        TEXT UNIQUE,
  jira_account_id      TEXT UNIQUE,
  github_login         TEXT UNIQUE,
  pagerduty_user_id    TEXT UNIQUE,
  is_bot               BOOLEAN NOT NULL DEFAULT FALSE,
  reports_to           INTEGER,                   -- references graph.people.eeid via lookup
  depth_from_root      SMALLINT,                  -- 0 = root; denormalized for authority
  first_seen_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  identity_resolved_at TIMESTAMPTZ,
  merged_into          BIGINT REFERENCES graph.people(id),
  sync_id              UUID NOT NULL DEFAULT gen_random_uuid() UNIQUE,
  sync_version         BIGINT NOT NULL DEFAULT 0,
  machine_id           TEXT NOT NULL
);
CREATE INDEX idx_people_email         ON graph.people(email) WHERE merged_into IS NULL;
CREATE INDEX idx_people_unresolved    ON graph.people(id)
  WHERE identity_resolved_at IS NULL AND merged_into IS NULL;
CREATE INDEX idx_people_sync_unsynced ON graph.people(sync_version) WHERE sync_version = 0;

-- graph.nodes — every artifact, every entity, every person reference
CREATE TABLE graph.nodes (
  id              TEXT PRIMARY KEY,        -- e.g., 'slack:C08:1779709917.613979'
  type            TEXT NOT NULL,           -- slack_thread | jira | gh_pr | cf_page |
                                           -- pagerduty | datadog | sentry | gws_doc |
                                           -- slack_file | partner | feature | status |
                                           -- currency | code_file | person | usergroup
  natural_key     TEXT NOT NULL,           -- 'PAY-2128', 'wego/payments#1960', …
  url             TEXT,                    -- canonical URL when applicable
  title           TEXT,
  body            TEXT,                    -- plain-text normalized body
  body_revision   INTEGER NOT NULL DEFAULT 0,
  body_ts         TIMESTAMPTZ,             -- source's "last edited"; tiebreak for races
  -- media-only fields
  mime_type       TEXT,
  size_bytes      BIGINT,
  external_url    TEXT,                    -- url_private for Slack files etc.
  thumb_url       TEXT,
  -- relationships
  author_person_id  BIGINT REFERENCES graph.people(id),
  scope           TEXT,                    -- 'slack:C08S954G2LX', 'jira:PAY', …
  metadata        JSONB NOT NULL DEFAULT '{}'::jsonb,
  first_seen_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  deleted_at      TIMESTAMPTZ,
  -- sync columns (match existing pattern)
  sync_id         UUID NOT NULL DEFAULT gen_random_uuid() UNIQUE,
  sync_version    BIGINT NOT NULL DEFAULT 0,
  machine_id      TEXT NOT NULL
);
CREATE INDEX idx_nodes_type          ON graph.nodes(type);
CREATE INDEX idx_nodes_scope         ON graph.nodes(scope);
CREATE INDEX idx_nodes_author        ON graph.nodes(author_person_id);
CREATE INDEX idx_nodes_updated_at    ON graph.nodes(updated_at DESC);
CREATE INDEX idx_nodes_sync_unsynced ON graph.nodes(sync_version) WHERE sync_version = 0;

-- graph.edges — typed relationships between nodes
CREATE TABLE graph.edges (
  id            BIGSERIAL PRIMARY KEY,
  from_node_id  TEXT NOT NULL REFERENCES graph.nodes(id) ON DELETE CASCADE,
  to_node_id    TEXT NOT NULL REFERENCES graph.nodes(id) ON DELETE CASCADE,
  kind          TEXT NOT NULL,    -- REFERENCES | PART_OF | MENTIONS | TOUCHES |
                                  -- OWNED_BY | AUTHORED_BY | REPLIES_TO
  source_msg_id TEXT,             -- which message/version emitted this edge
  body_revision INTEGER NOT NULL DEFAULT 0,   -- revision of from-node that produced edge
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  sync_id       UUID NOT NULL DEFAULT gen_random_uuid() UNIQUE,
  sync_version  BIGINT NOT NULL DEFAULT 0,
  machine_id    TEXT NOT NULL,
  UNIQUE(from_node_id, to_node_id, kind)
);
CREATE INDEX idx_edges_from         ON graph.edges(from_node_id, kind);
CREATE INDEX idx_edges_to           ON graph.edges(to_node_id, kind);
CREATE INDEX idx_edges_sync_unsynced ON graph.edges(sync_version) WHERE sync_version = 0;

-- graph.artifact_index — warm tier (summaries + embeddings)
CREATE TABLE graph.artifact_index (
  node_id      TEXT PRIMARY KEY REFERENCES graph.nodes(id) ON DELETE CASCADE,
  summary      TEXT,                    -- 200-token system-specific or LLM-generated
  summary_kind TEXT NOT NULL DEFAULT 'heuristic',  -- heuristic | llm
  embedding    VECTOR(768),             -- match existing AGENT_MEM_GEMINI_EMBEDDING_DIMS
  refreshed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  sync_id      UUID NOT NULL DEFAULT gen_random_uuid() UNIQUE,
  sync_version BIGINT NOT NULL DEFAULT 0,
  machine_id   TEXT NOT NULL
);
CREATE INDEX idx_artifact_index_embedding ON graph.artifact_index
  USING hnsw (embedding vector_cosine_ops);
CREATE INDEX idx_artifact_index_sync_unsynced
  ON graph.artifact_index(sync_version) WHERE sync_version = 0;

-- graph.artifact_bodies — lazy tier (full bodies, OCR, descriptions)
CREATE TABLE graph.artifact_bodies (
  node_id    TEXT PRIMARY KEY REFERENCES graph.nodes(id) ON DELETE CASCADE,
  body_full  TEXT NOT NULL,             -- full normalized body
  ocr_text   TEXT,                      -- for media: OCR result
  description TEXT,                     -- for media: Gemini description
  fetched_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  expires_at TIMESTAMPTZ,               -- TTL per type
  sync_id    UUID NOT NULL DEFAULT gen_random_uuid() UNIQUE,
  sync_version BIGINT NOT NULL DEFAULT 0,
  machine_id TEXT NOT NULL
);
CREATE INDEX idx_artifact_bodies_expires ON graph.artifact_bodies(expires_at);
CREATE INDEX idx_artifact_bodies_sync_unsynced
  ON graph.artifact_bodies(sync_version) WHERE sync_version = 0;

-- graph.jobs — Postgres-backed work queue
CREATE TABLE graph.jobs (
  id            BIGSERIAL PRIMARY KEY,
  type          TEXT NOT NULL,
  payload       JSONB NOT NULL,
  priority      SMALLINT NOT NULL DEFAULT 5,         -- 0 urgent, 5 normal, 10 batch
  status        TEXT NOT NULL DEFAULT 'queued',      -- queued | running | done | failed
  available_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  attempts      SMALLINT NOT NULL DEFAULT 0,
  max_attempts  SMALLINT NOT NULL DEFAULT 5,
  last_error    TEXT,
  locked_by     TEXT,
  locked_at     TIMESTAMPTZ,
  enqueued_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  completed_at  TIMESTAMPTZ,
  target_runner TEXT NOT NULL DEFAULT 'any',          -- 'any' | 'vps' | 'local'
  sync_id       UUID NOT NULL DEFAULT gen_random_uuid() UNIQUE,
  sync_version  BIGINT NOT NULL DEFAULT 0,
  machine_id    TEXT NOT NULL
);
CREATE INDEX idx_jobs_ready ON graph.jobs(priority, available_at)
  WHERE status = 'queued';
CREATE INDEX idx_jobs_stuck ON graph.jobs(locked_at)
  WHERE status = 'running';
CREATE INDEX idx_jobs_sync_unsynced
  ON graph.jobs(sync_version) WHERE sync_version = 0;

-- graph.slack_groups
CREATE TABLE graph.slack_groups (
  id              TEXT PRIMARY KEY,         -- S01TMG8Q65R
  handle          TEXT NOT NULL,
  name            TEXT NOT NULL,
  description     TEXT,
  member_user_ids TEXT[] NOT NULL DEFAULT '{}',
  user_count      INTEGER NOT NULL DEFAULT 0,
  refreshed_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  sync_id         UUID NOT NULL DEFAULT gen_random_uuid() UNIQUE,
  sync_version    BIGINT NOT NULL DEFAULT 0,
  machine_id      TEXT NOT NULL
);

-- graph.entities
CREATE TABLE graph.entities (
  id           TEXT PRIMARY KEY,         -- 'partner:triplea', 'feature:auto_refund', …
  kind         TEXT NOT NULL,            -- partner | feature | status | currency | code_module
  display_name TEXT NOT NULL,
  aliases      TEXT[] NOT NULL DEFAULT '{}',
  metadata     JSONB NOT NULL DEFAULT '{}'::jsonb,
  source       TEXT NOT NULL,            -- 'seed:pkg-payment', 'manual', 'extracted'
  first_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  sync_id      UUID NOT NULL DEFAULT gen_random_uuid() UNIQUE,
  sync_version BIGINT NOT NULL DEFAULT 0,
  machine_id   TEXT NOT NULL
);
CREATE INDEX idx_entities_kind    ON graph.entities(kind);
-- GIN index so the extractor matches aliases in O(log n)
CREATE INDEX idx_entities_aliases ON graph.entities USING gin (aliases);

-- graph.identity_map — denormalized so the resolver doesn't always join graph.people
CREATE TABLE graph.identity_map (
  source      TEXT NOT NULL,             -- slack | jira | github | confluence | pagerduty | datadog | sentry | gws
  external_id TEXT NOT NULL,             -- U..., Atlassian accountId, GitHub login, …
  person_id   BIGINT NOT NULL REFERENCES graph.people(id),
  resolved_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY(source, external_id)
);

-- graph.user_affinity_config — read-phase prep, populated this phase
CREATE TABLE graph.user_affinity_config (
  eeid                    INTEGER PRIMARY KEY,
  team_group_ids          TEXT[] NOT NULL DEFAULT '{}',
  dept_group_ids          TEXT[] NOT NULL DEFAULT '{}',
  team_subtree_root_eeid  INTEGER,
  autodetected            BOOLEAN NOT NULL DEFAULT TRUE,
  updated_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  sync_id                 UUID NOT NULL DEFAULT gen_random_uuid() UNIQUE,
  sync_version            BIGINT NOT NULL DEFAULT 0,
  machine_id              TEXT NOT NULL
);

-- +goose Down
DROP TABLE IF EXISTS graph.user_affinity_config;
DROP TABLE IF EXISTS graph.identity_map;
DROP TABLE IF EXISTS graph.entities;
DROP TABLE IF EXISTS graph.slack_groups;
DROP TABLE IF EXISTS graph.jobs;
DROP TABLE IF EXISTS graph.artifact_bodies;
DROP TABLE IF EXISTS graph.artifact_index;
DROP TABLE IF EXISTS graph.edges;
DROP TABLE IF EXISTS graph.nodes;
DROP TABLE IF EXISTS graph.people;
DROP SCHEMA IF EXISTS graph;
DROP EXTENSION IF EXISTS citext;
