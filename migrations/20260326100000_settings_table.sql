-- +goose Up
-- Runtime settings stored in PostgreSQL for centralized config

CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- +goose Down
DROP TABLE IF EXISTS settings;
