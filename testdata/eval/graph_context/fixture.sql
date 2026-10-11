DELETE FROM graph.artifact_index;
DELETE FROM graph.artifact_bodies;
DELETE FROM graph.edges;
DELETE FROM graph.jobs;
DELETE FROM graph.identity_map;
DELETE FROM graph.people;
DELETE FROM graph.nodes;

INSERT INTO graph.nodes (id, type, natural_key, title, body, scope, metadata, updated_at, created_at, machine_id) VALUES
  ('slack:CGC0:100.000001', 'slack', 'slack:CGC0:100.000001', 'Phase zero root', 'phase zero root body', 'public', '{"thread_ts":"100.000001"}', '2026-10-10T00:00:00Z', '2026-10-10T00:00:00Z', 'test'),
  ('slack:CGC0:101.000001', 'slack', 'slack:CGC0:101.000001', 'Phase zero reply', 'phase zero reply body', 'public', '{"thread_ts":"100.000001"}', '2026-10-09T00:00:00Z', '2026-10-09T00:00:00Z', 'test'),
  ('jira:GC0-1', 'jira', 'jira:GC0-1', 'Phase zero issue', 'phase zero issue body', 'public', '{}', '2026-10-08T00:00:00Z', '2026-10-08T00:00:00Z', 'test'),
  ('jira:GC0-MISS', 'jira', 'jira:GC0-MISS', 'Phase zero missing body', '', 'public', '{}', '2026-10-07T00:00:00Z', '2026-10-07T00:00:00Z', 'test');

INSERT INTO graph.edges (from_node_id, to_node_id, kind, metadata, machine_id) VALUES
  ('slack:CGC0:100.000001', 'jira:GC0-1', 'REFERENCES', '{"confidence":0.91}', 'test'),
  ('jira:GC0-MISS', 'jira:GC0-1', 'REFERENCES', '{"confidence":0.83}', 'test');
INSERT INTO graph.artifact_bodies (node_id, body_full, machine_id) VALUES
  ('slack:CGC0:100.000001', 'phase zero root body', 'test'),
  ('slack:CGC0:101.000001', 'phase zero reply body', 'test'),
  ('jira:GC0-1', 'phase zero issue body', 'test');

INSERT INTO graph.artifact_index (node_id, summary, summary_kind, refreshed_at, machine_id) VALUES
  ('slack:CGC0:100.000001', 'root summary', 'heuristic', '2026-10-10T00:00:00Z', 'test'),
  ('slack:CGC0:101.000001', 'reply summary', 'heuristic', '2026-10-09T00:00:00Z', 'test'),
  ('jira:GC0-1', 'issue summary', 'heuristic', '2026-10-08T00:00:00Z', 'test'),
  ('jira:GC0-MISS', 'missing summary', 'heuristic', '2026-10-07T00:00:00Z', 'test');
