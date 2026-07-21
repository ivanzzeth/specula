-- +goose Up
-- Per-key registry scopes (pull / push). Empty/NULL means default pull+push
-- (backward compatible with pre-scope keys).
--
-- SQLite has no ADD COLUMN IF NOT EXISTS; goose applies each migration exactly
-- once, so the bare ADD COLUMN is safe here.
ALTER TABLE api_keys ADD COLUMN scopes TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE api_keys DROP COLUMN scopes;
