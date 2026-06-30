-- +goose Up
-- Department label for people, ingested from the BambooHR "Department" CSV column.
-- Surfaced in thread/cluster summaries and hot-topic alerts so a person reads as
-- "Hazwan (Flights)" rather than a bare name. Nullable: not every record has one.
ALTER TABLE graph.people ADD COLUMN IF NOT EXISTS department text;

-- +goose Down
ALTER TABLE graph.people DROP COLUMN IF EXISTS department;
