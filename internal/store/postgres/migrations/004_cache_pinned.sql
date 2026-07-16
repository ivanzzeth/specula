-- +goose Up
-- R3 cache browser: operator "pin/protect" flag on immutable cache entries.
--
-- A pinned entry is exempt from GC/eviction. This is deliberately a column on
-- cache_entries rather than a separate table: eviction already scans this table,
-- so the exemption must be readable in the same pass (a join per eviction
-- candidate would be the only alternative, for a flag that is false ~always).
--
-- Distinct from "hosted" (repos/repo_tags): hosted data is authoritative and
-- never evictable by definition, whereas pinned is an override on cached data.
ALTER TABLE cache_entries ADD COLUMN IF NOT EXISTS pinned BOOLEAN NOT NULL DEFAULT FALSE;

-- Partial index: eviction asks "which entries are NOT pinned", and the browser
-- filters on pinned=true. Indexing only the rare true rows keeps it tiny.
CREATE INDEX IF NOT EXISTS idx_cache_entries_pinned
    ON cache_entries(pinned) WHERE pinned;

-- Sort/filter support for the cache browser: it pages by protocol ordered on
-- created_at or size, which is otherwise a seq scan + sort per request.
CREATE INDEX IF NOT EXISTS idx_cache_entries_proto_created
    ON cache_entries(protocol, created_at);
CREATE INDEX IF NOT EXISTS idx_cache_entries_proto_size
    ON cache_entries(protocol, size);

-- +goose Down
DROP INDEX IF EXISTS idx_cache_entries_proto_size;
DROP INDEX IF EXISTS idx_cache_entries_proto_created;
DROP INDEX IF EXISTS idx_cache_entries_pinned;
ALTER TABLE cache_entries DROP COLUMN IF EXISTS pinned;
