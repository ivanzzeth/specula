// Package sqlite provides a MetadataStore backed by modernc.org/sqlite (pure
// Go, CGO-free) running in WAL mode. Embedded goose migrations create all
// required tables on first open.
//
// IMPORTANT — single-instance node-local only: SQLite does not support
// concurrent writers across process boundaries. Do NOT use this driver when
// multiple Specula instances share the same storage backend; use the postgres
// driver instead (ARCHITECTURE L2).
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/store/meta"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// SQLiteStore implements meta.MetadataStore over a local SQLite database.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore opens the SQLite database at dsn, enables WAL mode, and
// applies all pending embedded goose migrations. dsn may be a file path or an
// SQLite URI (e.g. "file:specula.db?cache=shared").
func NewSQLiteStore(dsn string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", withPragmas(dsn))
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %q: %w", dsn, err)
	}

	// Limit to one writer connection; SQLite WAL allows concurrent readers.
	db.SetMaxOpenConns(1)

	// Enable WAL journal mode for better read concurrency.
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite: enable WAL: %w", err)
	}

	if err := runMigrations(db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &SQLiteStore{db: db}, nil
}

// runMigrations applies all pending goose SQL migrations from the embedded FS.
//
// Uses the Provider API rather than the package-level goose.SetBaseFS /
// SetDialect / Up helpers: those mutate global state, so this binary — which can
// open SQLite *and* PostgreSQL — would have one dialect clobber the other.
//
// No session locker here, unlike the PostgreSQL path: SQLite is node-local by
// design (see the store's doc comment), so there is no second replica to race
// with. The single-writer connection limit plus busy_timeout covers contention
// within this process.
func runMigrations(db *sql.DB) error {
	fsys, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("sqlite: migrations fs: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db, fsys)
	if err != nil {
		return fmt.Errorf("sqlite: goose provider: %w", err)
	}
	if _, err := provider.Up(context.Background()); err != nil {
		return fmt.Errorf("sqlite: run migrations: %w", err)
	}
	return nil
}

// Close closes the underlying database connection.
func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

// DB returns the raw *sql.DB handle for packages that need direct
// database/sql access (e.g. apikey.SQLStore, org.SQLStore) after migrations
// have been applied by NewSQLiteStore. The caller must not close the
// connection; use Close() for that.
func (s *SQLiteStore) DB() *sql.DB { return s.db }

// Compile-time assertion that SQLiteStore satisfies meta.MetadataStore.
var _ meta.MetadataStore = (*SQLiteStore)(nil)

// Get returns the immutable CacheEntry for ref, or (nil, nil) if absent.
func (s *SQLiteStore) Get(ctx context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	const q = `
		SELECT protocol, name, version, ref_digest, ref_upstream, mutable,
		       digest, size, tier, upstream, etag, verified_at, created_at, origin
		FROM   cache_entries
		WHERE  protocol = ? AND name = ? AND version = ?`

	row := s.db.QueryRowContext(ctx, q, ref.Protocol, ref.Name, ref.Version)

	var e artifact.CacheEntry
	var mutableInt int
	var verifiedAt, createdAt int64
	var origin string

	err := row.Scan(
		&e.Ref.Protocol, &e.Ref.Name, &e.Ref.Version,
		&e.Ref.Digest, &e.Ref.Upstream, &mutableInt,
		&e.Digest, &e.Size, &e.Tier, &e.Upstream, &e.ETag,
		&verifiedAt, &createdAt, &origin,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get cache entry (%s/%s@%s): %w",
			ref.Protocol, ref.Name, ref.Version, err)
	}

	e.Ref.Mutable = mutableInt != 0
	e.Protocol = e.Ref.Protocol
	e.Origin = artifact.NormalizeOrigin(origin)
	e.VerifiedAt = time.Unix(verifiedAt, 0).UTC()
	e.CreatedAt = time.Unix(createdAt, 0).UTC()
	return &e, nil
}

// Put upserts an immutable CacheEntry. On conflict the entry is updated except
// for created_at (first-write wins). Must be called AFTER the blob lands in the
// BlobStore (write ordering M1).
func (s *SQLiteStore) Put(ctx context.Context, entry artifact.CacheEntry) error {
	now := time.Now().UTC()
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = now
	}
	if entry.VerifiedAt.IsZero() {
		entry.VerifiedAt = now
	}

	// Prefer the top-level Protocol field; fall back to Ref.Protocol.
	proto := entry.Protocol
	if proto == "" {
		proto = entry.Ref.Protocol
	}

	mutableInt := 0
	if entry.Ref.Mutable {
		mutableInt = 1
	}
	origin := artifact.NormalizeOrigin(entry.Origin)

	const q = `
		INSERT INTO cache_entries
		    (protocol, name, version, ref_digest, ref_upstream, mutable,
		     digest, size, tier, upstream, etag, verified_at, created_at, origin)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(protocol, name, version) DO UPDATE SET
		    ref_digest   = excluded.ref_digest,
		    ref_upstream = excluded.ref_upstream,
		    mutable      = excluded.mutable,
		    digest       = excluded.digest,
		    size         = excluded.size,
		    tier         = excluded.tier,
		    upstream     = excluded.upstream,
		    etag         = excluded.etag,
		    verified_at  = excluded.verified_at,
		    origin       = CASE
		                     WHEN cache_entries.origin = 'hosted' THEN 'hosted'
		                     ELSE excluded.origin
		                   END`

	_, err := s.db.ExecContext(ctx, q,
		proto, entry.Ref.Name, entry.Ref.Version,
		entry.Ref.Digest, entry.Ref.Upstream, mutableInt,
		entry.Digest, entry.Size, int(entry.Tier),
		entry.Upstream, entry.ETag,
		entry.VerifiedAt.Unix(), entry.CreatedAt.Unix(), origin,
	)
	if err != nil {
		return fmt.Errorf("sqlite: put cache entry (%s/%s@%s): %w",
			proto, entry.Ref.Name, entry.Ref.Version, err)
	}
	return nil
}

// Delete removes the immutable entry for ref. A no-op if absent.
func (s *SQLiteStore) Delete(ctx context.Context, ref artifact.ArtifactRef) error {
	const q = `DELETE FROM cache_entries WHERE protocol = ? AND name = ? AND version = ?`
	if _, err := s.db.ExecContext(ctx, q, ref.Protocol, ref.Name, ref.Version); err != nil {
		return fmt.Errorf("sqlite: delete cache entry (%s/%s@%s): %w",
			ref.Protocol, ref.Name, ref.Version, err)
	}
	return nil
}

// GetMutable returns the short-TTL MutableEntry for key, or (nil, nil) if absent.
// Callers are responsible for checking TTL expiry and conditional revalidation.
func (s *SQLiteStore) GetMutable(ctx context.Context, key string) (*artifact.MutableEntry, error) {
	const q = `
		SELECT key, protocol, digest, payload, etag, last_modified,
		       ttl_seconds, upstream, fetched_at
		FROM   mutable_entries
		WHERE  key = ?`

	row := s.db.QueryRowContext(ctx, q, key)

	var e artifact.MutableEntry
	var fetchedAt int64

	err := row.Scan(
		&e.Key, &e.Protocol, &e.Digest, &e.Payload,
		&e.ETag, &e.LastModified, &e.TTLSeconds, &e.Upstream, &fetchedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: get mutable entry %q: %w", key, err)
	}

	e.FetchedAt = time.Unix(fetchedAt, 0).UTC()
	return &e, nil
}

// DeleteMutable removes the mutable entry for key. A no-op if absent.
func (s *SQLiteStore) DeleteMutable(ctx context.Context, key string) error {
	const q = `DELETE FROM mutable_entries WHERE key = ?`
	if _, err := s.db.ExecContext(ctx, q, key); err != nil {
		return fmt.Errorf("sqlite: delete mutable entry %q: %w", key, err)
	}
	return nil
}

// PutMutable upserts a MutableEntry with its TTL and conditional-revalidation
// state (ETag / Last-Modified). FetchedAt defaults to now if zero.
func (s *SQLiteStore) PutMutable(ctx context.Context, entry artifact.MutableEntry) error {
	if entry.FetchedAt.IsZero() {
		entry.FetchedAt = time.Now().UTC()
	}

	const q = `
		INSERT INTO mutable_entries
		    (key, protocol, digest, payload, etag, last_modified,
		     ttl_seconds, upstream, fetched_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
		    protocol      = excluded.protocol,
		    digest        = excluded.digest,
		    payload       = excluded.payload,
		    etag          = excluded.etag,
		    last_modified = excluded.last_modified,
		    ttl_seconds   = excluded.ttl_seconds,
		    upstream      = excluded.upstream,
		    fetched_at    = excluded.fetched_at`

	_, err := s.db.ExecContext(ctx, q,
		entry.Key, entry.Protocol, entry.Digest, entry.Payload,
		entry.ETag, entry.LastModified, entry.TTLSeconds,
		entry.Upstream, entry.FetchedAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("sqlite: put mutable entry %q: %w", entry.Key, err)
	}
	return nil
}

// CacheSizeByProtocol returns SUM(size), COUNT(*), MIN/MAX(created_at) grouped
// by protocol. This is the O(1) capacity aggregate that powers stats (G7).
func (s *SQLiteStore) CacheSizeByProtocol(ctx context.Context) (map[string]artifact.SizeStat, error) {
	const q = `
		SELECT protocol,
		       COALESCE(SUM(size),  0) AS bytes,
		       COUNT(*)                AS objects,
		       COALESCE(MIN(created_at), 0) AS oldest,
		       COALESCE(MAX(created_at), 0) AS newest
		FROM   cache_entries
		GROUP  BY protocol`

	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("sqlite: cache size by protocol: %w", err)
	}
	defer rows.Close()

	result := make(map[string]artifact.SizeStat)
	for rows.Next() {
		var protocol string
		var stat artifact.SizeStat
		var oldest, newest int64

		if err := rows.Scan(&protocol, &stat.Bytes, &stat.Objects, &oldest, &newest); err != nil {
			return nil, fmt.Errorf("sqlite: scan size stat: %w", err)
		}
		if oldest != 0 {
			stat.Oldest = time.Unix(oldest, 0).UTC()
		}
		if newest != 0 {
			stat.Newest = time.Unix(newest, 0).UTC()
		}
		result[protocol] = stat
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate size stats: %w", err)
	}
	return result, nil
}

// CacheSizeByOrigin returns SUM(size), COUNT(*) grouped by origin (hosted|cached).
func (s *SQLiteStore) CacheSizeByOrigin(ctx context.Context) (map[string]artifact.SizeStat, error) {
	const q = `
		SELECT COALESCE(NULLIF(origin, ''), 'cached') AS origin,
		       COALESCE(SUM(size),  0) AS bytes,
		       COUNT(*)                AS objects,
		       COALESCE(MIN(created_at), 0) AS oldest,
		       COALESCE(MAX(created_at), 0) AS newest
		FROM   cache_entries
		GROUP  BY COALESCE(NULLIF(origin, ''), 'cached')`

	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("sqlite: cache size by origin: %w", err)
	}
	defer rows.Close()

	result := make(map[string]artifact.SizeStat)
	for rows.Next() {
		var origin string
		var stat artifact.SizeStat
		var oldest, newest int64
		if err := rows.Scan(&origin, &stat.Bytes, &stat.Objects, &oldest, &newest); err != nil {
			return nil, fmt.Errorf("sqlite: scan origin size stat: %w", err)
		}
		origin = artifact.NormalizeOrigin(origin)
		if oldest != 0 {
			stat.Oldest = time.Unix(oldest, 0).UTC()
		}
		if newest != 0 {
			stat.Newest = time.Unix(newest, 0).UTC()
		}
		result[origin] = stat
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate origin size stats: %w", err)
	}
	return result, nil
}

// withPragmas appends per-connection PRAGMAs to a SQLite DSN (modernc's
// _pragma/_txlock query parameters). These are production hardening, not tuning:
//
//   - busy_timeout=5000  wait up to 5s for a lock instead of failing instantly.
//     Without it, any momentary contention surfaces as a hard
//     "database is locked" error to a user request.
//   - _txlock=immediate  take the write lock when a write transaction begins,
//     rather than upgrading a read lock mid-transaction — the deferred upgrade
//     is what turns concurrent writers into deadlock-shaped "locked" failures.
//
// A DSN that already carries query parameters keeps them; a bare file path is
// promoted to a file: URI so the parameters are parseable.
func withPragmas(dsn string) string {
	const params = "_pragma=busy_timeout(5000)&_txlock=immediate"

	// Already a URI (file:…) or already parameterised: append.
	if strings.Contains(dsn, "?") {
		return dsn + "&" + params
	}
	if strings.HasPrefix(dsn, "file:") {
		return dsn + "?" + params
	}
	// Bare path (including ":memory:") → file: URI so params are honoured.
	return "file:" + dsn + "?" + params
}
