-- +goose Up
-- Per-key registry scopes (pull / push). Empty/NULL means default pull+push
-- (backward compatible with pre-scope keys).
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS scopes TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE api_keys DROP COLUMN IF EXISTS scopes;
