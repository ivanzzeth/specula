-- +goose Up
-- verification_events: durable Admin Events feed (fail/warn verify outcomes).
-- Bounded by application prune; not a full SIEM.

CREATE TABLE IF NOT EXISTS verification_events (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    unix_ts  INTEGER NOT NULL,
    protocol TEXT    NOT NULL,
    artifact TEXT    NOT NULL DEFAULT '',
    digest   TEXT    NOT NULL DEFAULT '',
    tier     TEXT    NOT NULL DEFAULT '',
    result   TEXT    NOT NULL,
    detail   TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_verification_events_unix
    ON verification_events (unix_ts DESC);

-- +goose Down
DROP INDEX IF EXISTS idx_verification_events_unix;
DROP TABLE IF EXISTS verification_events;
