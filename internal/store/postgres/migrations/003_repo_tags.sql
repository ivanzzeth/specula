-- +goose Up
-- R2 hosted registry: tag→digest pointers for org-owned hosted repos.
-- The repos table already exists (002_multitenant). A dedicated repo_tags table
-- (rather than reusing the mutable metadata tier) keeps ListTags a simple
-- indexed scan and scopes tag names per repo via the (repo_id, tag) primary key.
-- Timestamps are RFC3339 TEXT to match the control-plane convention, so the same
-- SQL runs verbatim on PostgreSQL and SQLite. Idempotent (IF NOT EXISTS).

CREATE TABLE IF NOT EXISTS repo_tags (
    repo_id    TEXT NOT NULL,
    tag        TEXT NOT NULL,
    digest     TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (repo_id, tag)
);
CREATE INDEX IF NOT EXISTS repo_tags_repo ON repo_tags(repo_id);

-- +goose Down
DROP TABLE IF EXISTS repo_tags;
