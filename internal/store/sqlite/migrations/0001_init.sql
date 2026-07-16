-- +goose Up

-- users: control-plane authentication (first user becomes admin).
CREATE TABLE IF NOT EXISTS users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    email         TEXT    NOT NULL UNIQUE,
    password_hash TEXT    NOT NULL DEFAULT '',
    system_role   TEXT    NOT NULL DEFAULT 'user',
    token_gen     INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL DEFAULT 0
);

-- cache_entries: immutable CAS tier.
-- Keyed by (protocol, name, version); digest is the CAS sha256 key.
-- size is recorded at write time so CacheSizeByProtocol is an O(1) SUM (G7).
-- Single-instance node-local only — NOT shared across Specula instances (L2).
CREATE TABLE IF NOT EXISTS cache_entries (
    protocol     TEXT    NOT NULL,
    name         TEXT    NOT NULL,
    version      TEXT    NOT NULL,
    ref_digest   TEXT    NOT NULL DEFAULT '',
    ref_upstream TEXT    NOT NULL DEFAULT '',
    mutable      INTEGER NOT NULL DEFAULT 0,
    digest       TEXT    NOT NULL DEFAULT '',
    size         INTEGER NOT NULL DEFAULT 0,
    tier         INTEGER NOT NULL DEFAULT 0,
    upstream     TEXT    NOT NULL DEFAULT '',
    etag         TEXT    NOT NULL DEFAULT '',
    verified_at  INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (protocol, name, version)
);

CREATE INDEX IF NOT EXISTS idx_cache_entries_digest   ON cache_entries(digest);
CREATE INDEX IF NOT EXISTS idx_cache_entries_protocol ON cache_entries(protocol);

-- mutable_entries: short-TTL tier (tag->digest, index.yaml, packuments, refs).
-- TTLSeconds sentinels: -1 = never revalidate, 0 = always revalidate, >0 = seconds.
-- revalidate_at = fetched_at + ttl_seconds (unix); stored for fast staleness check.
CREATE TABLE IF NOT EXISTS mutable_entries (
    key           TEXT    NOT NULL PRIMARY KEY,
    protocol      TEXT    NOT NULL DEFAULT '',
    digest        TEXT    NOT NULL DEFAULT '',
    payload       BLOB,
    etag          TEXT    NOT NULL DEFAULT '',
    last_modified TEXT    NOT NULL DEFAULT '',
    ttl_seconds   INTEGER NOT NULL DEFAULT 0,
    upstream      TEXT    NOT NULL DEFAULT '',
    fetched_at    INTEGER NOT NULL DEFAULT 0
);

-- +goose Down

DROP TABLE IF EXISTS mutable_entries;
DROP TABLE IF EXISTS cache_entries;
DROP TABLE IF EXISTS users;
