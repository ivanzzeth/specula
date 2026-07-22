-- +goose Up
-- stats_series_samples: capacity time series (protocol '' = grand total).
-- upstream_blocks: per-protocol upstream auto-block circuit breaker state.

CREATE TABLE IF NOT EXISTS stats_series_samples (
    protocol TEXT   NOT NULL DEFAULT '',
    unix_ts  INTEGER NOT NULL,
    bytes    INTEGER NOT NULL,
    PRIMARY KEY (protocol, unix_ts)
);

CREATE INDEX IF NOT EXISTS idx_stats_series_protocol_unix
    ON stats_series_samples (protocol, unix_ts DESC);

CREATE TABLE IF NOT EXISTS upstream_blocks (
    protocol      TEXT    NOT NULL,
    upstream      TEXT    NOT NULL,
    failures      INTEGER NOT NULL DEFAULT 0,
    blocked_until TEXT,
    PRIMARY KEY (protocol, upstream)
);

-- +goose Down
DROP TABLE IF EXISTS upstream_blocks;
DROP INDEX IF EXISTS idx_stats_series_protocol_unix;
DROP TABLE IF EXISTS stats_series_samples;
