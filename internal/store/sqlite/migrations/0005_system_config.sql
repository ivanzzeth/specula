-- +goose Up
-- Encrypted runtime configuration KV (internal/configstore), the storage
-- substrate under internal/settings.
--
-- `value` holds AES-256-GCM ciphertext (nonce||ciphertext), never plaintext.
-- The master key lives only in the process configuration
-- (auth.config_secret / SPECULA_AUTH__CONFIG_SECRET) and is deliberately NOT in
-- this database: a database dump alone must not yield the session signing secret
-- or the registry token key.
--
-- Why a table rather than a file on disk: this is what makes a generated secret
-- survive a restart AND be shared automatically by every HA replica. A PEM/file
-- is node-local, so replica B cannot verify a session replica A issued.
CREATE TABLE IF NOT EXISTS system_config (
    key        TEXT NOT NULL PRIMARY KEY,
    value      BLOB NOT NULL,
    updated_at TEXT NOT NULL
);

-- +goose Down
DROP TABLE IF EXISTS system_config;
