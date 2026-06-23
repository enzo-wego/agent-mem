-- +goose Up
-- Seed graph default configuration rows into public.settings.
-- Uses ON CONFLICT DO NOTHING so re-running is idempotent.

-- Body TTLs (duration strings consumed by the graph workers)
INSERT INTO settings (key, value) VALUES ('graph.body_ttl.slack',       '5m')  ON CONFLICT (key) DO NOTHING;
INSERT INTO settings (key, value) VALUES ('graph.body_ttl.jira',        '1h')  ON CONFLICT (key) DO NOTHING;
INSERT INTO settings (key, value) VALUES ('graph.body_ttl.gh_pr',       '15m') ON CONFLICT (key) DO NOTHING;
INSERT INTO settings (key, value) VALUES ('graph.body_ttl.confluence',  '24h') ON CONFLICT (key) DO NOTHING;
INSERT INTO settings (key, value) VALUES ('graph.body_ttl.pagerduty',   '30m') ON CONFLICT (key) DO NOTHING;
INSERT INTO settings (key, value) VALUES ('graph.body_ttl.datadog',     '30m') ON CONFLICT (key) DO NOTHING;
INSERT INTO settings (key, value) VALUES ('graph.body_ttl.sentry',      '30m') ON CONFLICT (key) DO NOTHING;
INSERT INTO settings (key, value) VALUES ('graph.body_ttl.gws',         '6h')  ON CONFLICT (key) DO NOTHING;

-- Per-source concurrency rate caps (max concurrent fetcher goroutines)
INSERT INTO settings (key, value) VALUES ('graph.rate.slack',           '5')   ON CONFLICT (key) DO NOTHING;
INSERT INTO settings (key, value) VALUES ('graph.rate.jira',            '5')   ON CONFLICT (key) DO NOTHING;
INSERT INTO settings (key, value) VALUES ('graph.rate.github',          '10')  ON CONFLICT (key) DO NOTHING;
INSERT INTO settings (key, value) VALUES ('graph.rate.confluence',      '5')   ON CONFLICT (key) DO NOTHING;
INSERT INTO settings (key, value) VALUES ('graph.rate.pagerduty',       '3')   ON CONFLICT (key) DO NOTHING;
INSERT INTO settings (key, value) VALUES ('graph.rate.datadog',         '3')   ON CONFLICT (key) DO NOTHING;
INSERT INTO settings (key, value) VALUES ('graph.rate.sentry',          '5')   ON CONFLICT (key) DO NOTHING;
INSERT INTO settings (key, value) VALUES ('graph.rate.gws',             '5')   ON CONFLICT (key) DO NOTHING;
INSERT INTO settings (key, value) VALUES ('graph.rate.gemini',          '4')   ON CONFLICT (key) DO NOTHING;

-- Read-phase scoring weights (stored now, unused until next phase)
INSERT INTO settings (key, value) VALUES ('graph.weights.sem',          '0.50') ON CONFLICT (key) DO NOTHING;
INSERT INTO settings (key, value) VALUES ('graph.weights.rec',          '0.15') ON CONFLICT (key) DO NOTHING;
INSERT INTO settings (key, value) VALUES ('graph.weights.edge',         '0.15') ON CONFLICT (key) DO NOTHING;
INSERT INTO settings (key, value) VALUES ('graph.weights.team',         '0.15') ON CONFLICT (key) DO NOTHING;
INSERT INTO settings (key, value) VALUES ('graph.weights.auth',         '0.05') ON CONFLICT (key) DO NOTHING;

-- +goose Down
DELETE FROM settings WHERE key IN (
  'graph.body_ttl.slack',
  'graph.body_ttl.jira',
  'graph.body_ttl.gh_pr',
  'graph.body_ttl.confluence',
  'graph.body_ttl.pagerduty',
  'graph.body_ttl.datadog',
  'graph.body_ttl.sentry',
  'graph.body_ttl.gws',
  'graph.rate.slack',
  'graph.rate.jira',
  'graph.rate.github',
  'graph.rate.confluence',
  'graph.rate.pagerduty',
  'graph.rate.datadog',
  'graph.rate.sentry',
  'graph.rate.gws',
  'graph.rate.gemini',
  'graph.weights.sem',
  'graph.weights.rec',
  'graph.weights.edge',
  'graph.weights.team',
  'graph.weights.auth'
);
