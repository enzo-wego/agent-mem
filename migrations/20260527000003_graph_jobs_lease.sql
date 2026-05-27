-- +goose Up
ALTER TABLE graph.jobs
  ADD COLUMN lease_until TIMESTAMPTZ;

CREATE INDEX idx_jobs_stuck_lease ON graph.jobs(lease_until)
  WHERE status = 'running';

-- +goose Down
DROP INDEX IF EXISTS graph.idx_jobs_stuck_lease;
ALTER TABLE graph.jobs DROP COLUMN IF EXISTS lease_until;
