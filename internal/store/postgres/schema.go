// Package postgres provides a concurrency-safe MetadataStore backed by
// PostgreSQL via jackc/pgx/v5 (ON CONFLICT upserts). This is the HA backend;
// multiple Specula instances can share the same database safely.
package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ApplySchema creates the cache_entries and mutable_entries tables if they
// do not already exist. It is idempotent and suitable for tests and
// single-instance deployments. Production deployments should run the goose
// migrations in the migrations/ subdirectory via internal/migrate.Up.
func ApplySchema(ctx context.Context, pool *pgxpool.Pool) error {
	for _, stmt := range schemaDDL {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("postgres: apply schema: %w", err)
		}
	}
	return nil
}

// schemaDDL is the ordered list of DDL statements that bring an empty
// database up to the current schema. Every statement is idempotent
// (CREATE … IF NOT EXISTS / CREATE INDEX IF NOT EXISTS).
var schemaDDL = []string{
	// Immutable tier: one row per (protocol, name, version).
	// size is recorded at write time so CacheSizeByProtocol is an O(1) SUM,
	// never an FS walk (architecture §10 / G7).
	`CREATE TABLE IF NOT EXISTS cache_entries (
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
	)`,

	// Index for GROUP BY aggregation in CacheSizeByProtocol.
	`CREATE INDEX IF NOT EXISTS idx_cache_entries_protocol
		ON cache_entries (protocol)`,

	// Index for fast lookups by digest (GC / eviction).
	`CREATE INDEX IF NOT EXISTS idx_cache_entries_digest
		ON cache_entries (digest)`,

	// Mutable tier: short-TTL records (tag→digest, index pages, packuments).
	// payload stores the body when the whole response must be cached
	// (e.g. index.yaml, /simple/<pkg>/ page).
	`CREATE TABLE IF NOT EXISTS mutable_entries (
		key           TEXT        NOT NULL PRIMARY KEY,
		protocol      TEXT        NOT NULL DEFAULT '',
		digest        TEXT        NOT NULL DEFAULT '',
		payload       BYTEA       NOT NULL DEFAULT ''::bytea,
		etag          TEXT        NOT NULL DEFAULT '',
		last_modified TEXT        NOT NULL DEFAULT '',
		ttl_seconds   BIGINT      NOT NULL DEFAULT 0,
		upstream      TEXT        NOT NULL DEFAULT '',
		fetched_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`,
}
