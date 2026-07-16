package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ivanzzeth/specula/internal/coalesce"
)

// PGAdvisoryLocker implements coalesce.Locker using PostgreSQL session-level
// advisory locks. Each acquired lock holds a dedicated pool connection for the
// duration of the lock; the lock is automatically released when that connection
// closes, providing crash-safety without a separate heartbeat.
//
// The ttl parameter passed to Acquire acts as the acquisition wait timeout:
// if the lock is not obtained within ttl, Acquire returns a context error.
// Once held, the lock persists until Release is called explicitly (or the
// process exits and the connection closes).
type PGAdvisoryLocker struct {
	pool *pgxpool.Pool
}

// NewPGAdvisoryLocker returns a Locker backed by PostgreSQL advisory locks.
// pool must remain open for the lifetime of the locker.
func NewPGAdvisoryLocker(pool *pgxpool.Pool) *PGAdvisoryLocker {
	return &PGAdvisoryLocker{pool: pool}
}

// Compile-time assertion: PGAdvisoryLocker satisfies coalesce.Locker.
var _ coalesce.Locker = (*PGAdvisoryLocker)(nil)

// Acquire blocks until the advisory lock for key is obtained, the context is
// cancelled, or the ttl elapses. A non-zero ttl is applied as an acquisition
// deadline; zero or negative ttl defers to ctx only.
//
// The returned Lock holds a dedicated pool connection. Callers MUST call
// Lock.Release to avoid exhausting the pool.
func (l *PGAdvisoryLocker) Acquire(ctx context.Context, key string, ttl time.Duration) (coalesce.Lock, error) {
	k := hashKey(key)

	if ttl > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, ttl)
		defer cancel()
	}

	// Acquire a dedicated connection; pg advisory locks are session-scoped
	// and must stay on the same connection for the duration of the lock.
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("pg advisory lock acquire conn: %w", err)
	}

	const retryPoll = 50 * time.Millisecond
	for {
		var locked bool
		if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", k).Scan(&locked); err != nil {
			conn.Release()
			return nil, fmt.Errorf("pg advisory lock try: %w", err)
		}
		if locked {
			return &pgAdvisoryLock{
				conn:  conn,
				key:   k,
				token: randomToken(),
			}, nil
		}

		// Not yet granted; wait a short interval or honour ctx cancellation.
		select {
		case <-ctx.Done():
			conn.Release()
			return nil, fmt.Errorf("pg advisory lock: %w", ctx.Err())
		case <-time.After(retryPoll):
			// spin
		}
	}
}

// pgAdvisoryLock is a held PostgreSQL session-level advisory lock.
type pgAdvisoryLock struct {
	mu       sync.Mutex
	conn     *pgxpool.Conn
	key      int64
	token    string
	released bool
}

// Compile-time assertion: pgAdvisoryLock satisfies coalesce.Lock.
var _ coalesce.Lock = (*pgAdvisoryLock)(nil)

// Release relinquishes the advisory lock and returns the connection to the
// pool. It is idempotent: multiple calls after the first are no-ops.
func (lk *pgAdvisoryLock) Release(ctx context.Context) error {
	lk.mu.Lock()
	defer lk.mu.Unlock()

	if lk.released {
		return nil
	}
	lk.released = true

	conn := lk.conn
	lk.conn = nil

	// Explicitly unlock before returning the connection so it can be reused
	// without carrying a stale advisory lock on the session.
	_, err := conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", lk.key)
	conn.Release() // always return to pool, even on unlock error
	if err != nil {
		return fmt.Errorf("pg advisory lock release: %w", err)
	}
	return nil
}

// Token returns the random fencing token assigned at acquisition time.
// Callers can pass this token to owner-checked release logic to guard
// against a stale holder releasing a lock it no longer owns.
func (lk *pgAdvisoryLock) Token() string { return lk.token }

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// hashKey maps an arbitrary string to a stable int64 for pg_advisory_lock.
// Uses FNV-1a 64-bit for speed and good distribution. Collisions cause false
// serialisation (no correctness harm) because the originating string key is
// still used as the logical lock identity by the caller.
func hashKey(key string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	// Cast uint64 → int64: wraps for large hashes, negative values are valid
	// inputs to pg_advisory_lock(bigint).
	return int64(h.Sum64())
}

// randomToken produces a 32-hex-character cryptographically random string
// used as a fencing token. Falls back to a nanosecond timestamp on the
// unlikely event that crypto/rand is unavailable.
func randomToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
