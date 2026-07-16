package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/store/meta"
)

// PostgresStore implements meta.MetadataStore over a pgx connection pool.
// All writes use ON CONFLICT upserts so concurrent instances are safe.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore opens a pgx connection pool against dsn and verifies
// connectivity via Ping. Callers should defer s.Close() when done.
func NewPostgresStore(ctx context.Context, dsn string) (*PostgresStore, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: new pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return &PostgresStore{pool: pool}, nil
}

// Close drains and closes all connections in the pool.
func (s *PostgresStore) Close() { s.pool.Close() }

// Pool returns the underlying pgxpool for callers that need direct access
// (e.g. PGAdvisoryLocker).
func (s *PostgresStore) Pool() *pgxpool.Pool { return s.pool }

// Compile-time assertion: PostgresStore satisfies meta.MetadataStore.
var _ meta.MetadataStore = (*PostgresStore)(nil)

// ─────────────────────────────────────────────────────────────────────────────
// Immutable tier — cache_entries
// ─────────────────────────────────────────────────────────────────────────────

// Get returns the immutable CacheEntry for ref, or (nil, nil) if absent.
func (s *PostgresStore) Get(ctx context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	const q = `
		SELECT
			protocol, name, version,
			digest, size, tier,
			upstream, etag,
			verified_at, created_at
		FROM cache_entries
		WHERE protocol = $1 AND name = $2 AND version = $3
		LIMIT 1`

	var e artifact.CacheEntry
	var tier int
	var verifiedAt, createdAt time.Time

	err := s.pool.QueryRow(ctx, q, ref.Protocol, ref.Name, ref.Version).Scan(
		&e.Ref.Protocol,
		&e.Ref.Name,
		&e.Ref.Version,
		&e.Digest,
		&e.Size,
		&tier,
		&e.Upstream,
		&e.ETag,
		&verifiedAt,
		&createdAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: get(%s/%s@%s): %w",
			ref.Protocol, ref.Name, ref.Version, err)
	}

	e.Tier = artifact.Tier(tier)
	e.Protocol = e.Ref.Protocol
	e.Ref.Upstream = e.Upstream
	e.VerifiedAt = verifiedAt
	e.CreatedAt = createdAt
	return &e, nil
}

// Put upserts an immutable CacheEntry (ON CONFLICT updates all fields except
// created_at so first-write wins on creation time). Written AFTER the blob
// lands in CAS (architecture M1 write ordering).
func (s *PostgresStore) Put(ctx context.Context, entry artifact.CacheEntry) error {
	const q = `
		INSERT INTO cache_entries
			(protocol, name, version, digest, size, tier,
			 upstream, etag, verified_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (protocol, name, version) DO UPDATE SET
			digest      = EXCLUDED.digest,
			size        = EXCLUDED.size,
			tier        = EXCLUDED.tier,
			upstream    = EXCLUDED.upstream,
			etag        = EXCLUDED.etag,
			verified_at = EXCLUDED.verified_at`

	createdAt := entry.CreatedAt.UTC()
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	verifiedAt := entry.VerifiedAt.UTC()
	if verifiedAt.IsZero() {
		verifiedAt = time.Now().UTC()
	}

	_, err := s.pool.Exec(ctx, q,
		entry.Ref.Protocol,
		entry.Ref.Name,
		entry.Ref.Version,
		entry.Digest,
		entry.Size,
		int(entry.Tier),
		entry.Upstream,
		entry.ETag,
		verifiedAt,
		createdAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: put(%s/%s@%s): %w",
			entry.Ref.Protocol, entry.Ref.Name, entry.Ref.Version, err)
	}
	return nil
}

// Delete removes the immutable entry for ref. A no-op if the entry does not
// exist.
func (s *PostgresStore) Delete(ctx context.Context, ref artifact.ArtifactRef) error {
	const q = `
		DELETE FROM cache_entries
		WHERE protocol = $1 AND name = $2 AND version = $3`

	if _, err := s.pool.Exec(ctx, q, ref.Protocol, ref.Name, ref.Version); err != nil {
		return fmt.Errorf("postgres: delete(%s/%s@%s): %w",
			ref.Protocol, ref.Name, ref.Version, err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Mutable tier — mutable_entries
// ─────────────────────────────────────────────────────────────────────────────

// GetMutable returns the short-TTL MutableEntry for key, or (nil, nil) if absent.
func (s *PostgresStore) GetMutable(ctx context.Context, key string) (*artifact.MutableEntry, error) {
	const q = `
		SELECT
			key, protocol, digest, payload,
			etag, last_modified, ttl_seconds,
			upstream, fetched_at
		FROM mutable_entries
		WHERE key = $1`

	var e artifact.MutableEntry
	var fetchedAt time.Time

	err := s.pool.QueryRow(ctx, q, key).Scan(
		&e.Key,
		&e.Protocol,
		&e.Digest,
		&e.Payload,
		&e.ETag,
		&e.LastModified,
		&e.TTLSeconds,
		&e.Upstream,
		&fetchedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: get_mutable(%s): %w", key, err)
	}

	e.FetchedAt = fetchedAt
	return &e, nil
}

// DeleteMutable removes the mutable entry for key. A no-op if absent.
func (s *PostgresStore) DeleteMutable(ctx context.Context, key string) error {
	const q = `DELETE FROM mutable_entries WHERE key = $1`
	if _, err := s.pool.Exec(ctx, q, key); err != nil {
		return fmt.Errorf("postgres: delete_mutable(%s): %w", key, err)
	}
	return nil
}

// PutMutable upserts a MutableEntry using ON CONFLICT DO UPDATE, replacing
// all fields so callers always hold the freshest revalidation state.
func (s *PostgresStore) PutMutable(ctx context.Context, entry artifact.MutableEntry) error {
	const q = `
		INSERT INTO mutable_entries
			(key, protocol, digest, payload,
			 etag, last_modified, ttl_seconds,
			 upstream, fetched_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (key) DO UPDATE SET
			protocol      = EXCLUDED.protocol,
			digest        = EXCLUDED.digest,
			payload       = EXCLUDED.payload,
			etag          = EXCLUDED.etag,
			last_modified = EXCLUDED.last_modified,
			ttl_seconds   = EXCLUDED.ttl_seconds,
			upstream      = EXCLUDED.upstream,
			fetched_at    = EXCLUDED.fetched_at`

	// Normalise nil payload to empty slice so the NOT NULL column is satisfied.
	payload := entry.Payload
	if payload == nil {
		payload = []byte{}
	}

	fetchedAt := entry.FetchedAt.UTC()
	if fetchedAt.IsZero() {
		fetchedAt = time.Now().UTC()
	}

	_, err := s.pool.Exec(ctx, q,
		entry.Key,
		entry.Protocol,
		entry.Digest,
		payload,
		entry.ETag,
		entry.LastModified,
		entry.TTLSeconds,
		entry.Upstream,
		fetchedAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: put_mutable(%s): %w", entry.Key, err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Capacity statistics — G7
// ─────────────────────────────────────────────────────────────────────────────

// CacheSizeByProtocol returns SUM(size), COUNT(*), MIN/MAX(created_at) grouped
// by protocol from cache_entries. This is an O(1) aggregation — no FS walk —
// because size is recorded at write time (architecture §10).
func (s *PostgresStore) CacheSizeByProtocol(ctx context.Context) (map[string]artifact.SizeStat, error) {
	const q = `
		SELECT
			protocol,
			COALESCE(SUM(size), 0) AS bytes,
			COUNT(*)               AS objects,
			MIN(created_at)        AS oldest,
			MAX(created_at)        AS newest
		FROM cache_entries
		GROUP BY protocol`

	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("postgres: cache_size_by_protocol: %w", err)
	}
	defer rows.Close()

	result := make(map[string]artifact.SizeStat)
	for rows.Next() {
		var protocol string
		var stat artifact.SizeStat
		if err := rows.Scan(
			&protocol,
			&stat.Bytes,
			&stat.Objects,
			&stat.Oldest,
			&stat.Newest,
		); err != nil {
			return nil, fmt.Errorf("postgres: scan size stat: %w", err)
		}
		result[protocol] = stat
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: cache_size_by_protocol rows: %w", err)
	}
	return result, nil
}
