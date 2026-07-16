-- +goose Up
-- R1 multi-tenant kernel: orgs / members / invitations / grants / api_keys / repos.
-- Adapted from ai-sandbox controlplane baseline. Control-plane timestamps are
-- stored as RFC3339 TEXT (matching the ported org/apikey/grant SQL) so the same
-- SQL works verbatim on SQLite and PostgreSQL. Existing users/cache tables from
-- 0001_init are untouched. All statements are IF NOT EXISTS (idempotent).

CREATE TABLE IF NOT EXISTS orgs (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL DEFAULT '',
    slug       TEXT NOT NULL DEFAULT '',
    status     TEXT NOT NULL DEFAULT 'active',
    created_by TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS orgs_slug ON orgs(slug);

CREATE TABLE IF NOT EXISTS org_members (
    id         TEXT PRIMARY KEY,
    org_id     TEXT NOT NULL,
    email      TEXT NOT NULL,
    role       TEXT NOT NULL DEFAULT 'viewer',
    invited_by TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS org_members_org_email ON org_members(org_id, email);
CREATE INDEX IF NOT EXISTS org_members_email ON org_members(email);

CREATE TABLE IF NOT EXISTS org_invitations (
    id         TEXT PRIMARY KEY,
    org_id     TEXT NOT NULL,
    email      TEXT NOT NULL,
    role       TEXT NOT NULL DEFAULT 'viewer',
    invited_by TEXT NOT NULL DEFAULT '',
    token      TEXT NOT NULL,
    status     TEXT NOT NULL DEFAULT 'pending',
    expires_at TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS org_invitations_token ON org_invitations(token);
CREATE INDEX IF NOT EXISTS org_invitations_org ON org_invitations(org_id);

CREATE TABLE IF NOT EXISTS resource_grants (
    resource_type TEXT NOT NULL,
    resource_id   TEXT NOT NULL,
    subject_type  TEXT NOT NULL,
    subject_id    TEXT NOT NULL,
    access        TEXT NOT NULL DEFAULT 'read',
    granted_by    TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (resource_type, resource_id, subject_type, subject_id)
);
CREATE INDEX IF NOT EXISTS resource_grants_res ON resource_grants(resource_type, resource_id);
CREATE INDEX IF NOT EXISTS resource_grants_subject ON resource_grants(subject_type, subject_id);

CREATE TABLE IF NOT EXISTS api_keys (
    key_hash     TEXT PRIMARY KEY,
    id           TEXT NOT NULL,
    label        TEXT NOT NULL DEFAULT '',
    prefix       TEXT NOT NULL DEFAULT '',
    org_id       TEXT NOT NULL DEFAULT '',
    user_id      TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL DEFAULT '',
    last_used_at TEXT,
    expires_at   TEXT,
    revoked      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_api_keys_org  ON api_keys(org_id);
CREATE INDEX IF NOT EXISTS idx_api_keys_user ON api_keys(org_id, user_id);

CREATE TABLE IF NOT EXISTS repos (
    id            TEXT PRIMARY KEY,
    org_id        TEXT NOT NULL,
    name          TEXT NOT NULL,
    visibility    TEXT NOT NULL DEFAULT 'private',
    owner_user_id TEXT NOT NULL DEFAULT '',
    created_at    TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS repos_org_name ON repos(org_id, name);
CREATE INDEX IF NOT EXISTS repos_org ON repos(org_id);

-- +goose Down
DROP TABLE IF EXISTS repos;
DROP TABLE IF EXISTS api_keys;
DROP TABLE IF EXISTS resource_grants;
DROP TABLE IF EXISTS org_invitations;
DROP TABLE IF EXISTS org_members;
DROP TABLE IF EXISTS orgs;
