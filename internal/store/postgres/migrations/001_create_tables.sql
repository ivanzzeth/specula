-- +goose Up
-- Specula PostgreSQL schema v1
-- cache_entries: immutable CAS metadata (one row per protocol/name/version).
-- mutable_entries: short-TTL mutable tier (tag→digest, index pages, packuments).

-- users: control-plane authentication (first registered user becomes admin).
CREATE TABLE IF NOT EXISTS users (
    id            BIGSERIAL   PRIMARY KEY,
    email         TEXT        NOT NULL UNIQUE,
    name          TEXT        NOT NULL DEFAULT '',
    password_hash TEXT        NOT NULL DEFAULT '',
    system_role   TEXT        NOT NULL DEFAULT 'user',
    token_gen     BIGINT      NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS cache_entries (
    protocol    TEXT        NOT NULL,
    name        TEXT        NOT NULL,
    version     TEXT        NOT NULL,
    digest      TEXT        NOT NULL DEFAULT '',
    size        BIGINT      NOT NULL DEFAULT 0,
    tier        INTEGER     NOT NULL DEFAULT 0,
    upstream    TEXT        NOT NULL DEFAULT '',
    etag        TEXT        NOT NULL DEFAULT '',
    verified_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (protocol, name, version)
);

-- idx_cache_entries_protocol supports GROUP BY in CacheSizeByProtocol (G7 O(1) stats).
CREATE INDEX IF NOT EXISTS idx_cache_entries_protocol ON cache_entries (protocol);

-- idx_cache_entries_digest supports GC / eviction lookups by digest.
CREATE INDEX IF NOT EXISTS idx_cache_entries_digest   ON cache_entries (digest);

CREATE TABLE IF NOT EXISTS mutable_entries (
    key           TEXT        NOT NULL PRIMARY KEY,
    protocol      TEXT        NOT NULL DEFAULT '',
    digest        TEXT        NOT NULL DEFAULT '',
    payload       BYTEA       NOT NULL DEFAULT ''::bytea,
    etag          TEXT        NOT NULL DEFAULT '',
    last_modified TEXT        NOT NULL DEFAULT '',
    ttl_seconds   BIGINT      NOT NULL DEFAULT 0,
    upstream      TEXT        NOT NULL DEFAULT '',
    fetched_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- +goose Down
DROP TABLE IF EXISTS mutable_entries;
DROP INDEX IF EXISTS idx_cache_entries_digest;
DROP INDEX IF EXISTS idx_cache_entries_protocol;
DROP TABLE IF EXISTS cache_entries;
DROP TABLE IF EXISTS users;
