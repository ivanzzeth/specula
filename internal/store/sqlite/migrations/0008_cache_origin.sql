-- +goose Up
-- Hosted vs cached lifecycle (REGISTRY-DESIGN): GC/eviction only targets
-- origin='cached'. Hosted OCI pushes are authoritative and never evicted.
--
-- Default 'cached' keeps pre-origin rows evictable (pull-through behaviour).
ALTER TABLE cache_entries ADD COLUMN origin TEXT NOT NULL DEFAULT 'cached';

CREATE INDEX IF NOT EXISTS idx_cache_entries_origin
    ON cache_entries(origin);

-- +goose Down
DROP INDEX IF EXISTS idx_cache_entries_origin;
ALTER TABLE cache_entries DROP COLUMN origin;
