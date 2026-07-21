-- +goose Up
-- Hosted vs cached lifecycle: GC/eviction only targets origin='cached'.
ALTER TABLE cache_entries ADD COLUMN IF NOT EXISTS origin TEXT NOT NULL DEFAULT 'cached';

CREATE INDEX IF NOT EXISTS idx_cache_entries_origin
    ON cache_entries(origin);

-- +goose Down
DROP INDEX IF EXISTS idx_cache_entries_origin;
ALTER TABLE cache_entries DROP COLUMN IF EXISTS origin;
