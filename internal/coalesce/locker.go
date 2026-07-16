package coalesce

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// tokenSeq generates unique, monotonically increasing lock tokens.
var tokenSeq atomic.Int64

func newToken() string {
	return fmt.Sprintf("local-%d", tokenSeq.Add(1))
}

// lockEntry is the in-memory record for a held local lock.
type lockEntry struct {
	token   string
	expires time.Time     // zero means no TTL (no auto-expiry)
	release chan struct{} // closed on release or TTL eviction; wakes up all waiters
	timer   *time.Timer   // non-nil when TTL > 0; stopped on explicit Release
}

// localLocker is a functional in-process Locker. It is not safe for
// cross-process use; a Redis SET NX or PG advisory lock implementation
// should be used in multi-instance deployments.
//
// Semantics:
//   - Acquire blocks until the lock is free (or ctx is cancelled).
//   - If ttl > 0 the lock is auto-evicted after ttl, waking up all waiters so
//     they can retry immediately.
//   - Release is owner-checked: only the holder whose Token matches can release.
//     If ownership was lost (TTL eviction) Release is a silent no-op.
type localLocker struct {
	mu   sync.Mutex
	held map[string]*lockEntry
}

// NewLocalLocker constructs a functional in-process Locker.
func NewLocalLocker() Locker {
	return &localLocker{held: make(map[string]*lockEntry)}
}

var _ Locker = (*localLocker)(nil)

// Acquire blocks until the lock on key is obtained, ctx is cancelled, or
// (if another holder's TTL fires) the wait channel is signalled and the
// lock becomes available again.
func (l *localLocker) Acquire(ctx context.Context, key string, ttl time.Duration) (Lock, error) {
	token := newToken()
	for {
		// Fast-path ctx check before acquiring the mutex.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		l.mu.Lock()
		entry, held := l.held[key]
		if !held {
			// Lock is free; acquire it.
			e := &lockEntry{
				token:   token,
				release: make(chan struct{}),
			}
			if ttl > 0 {
				e.expires = time.Now().Add(ttl)
				e.timer = time.AfterFunc(ttl, func() { l.evictExpired(key, token) })
			}
			l.held[key] = e
			l.mu.Unlock()
			return &localLock{key: key, token: token, locker: l}, nil
		}

		// Lock is held; save the release channel so we can wait on it.
		releaseCh := entry.release
		l.mu.Unlock()

		// Wait for this holder to release or TTL-evict, or for ctx to expire.
		select {
		case <-releaseCh:
			// The lock was released or evicted; loop back and try again.
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// evictExpired is called by the TTL timer. It removes the entry only if the
// given token still matches the current holder (guards against a race where the
// original holder already released and a new holder acquired before the timer fired).
func (l *localLocker) evictExpired(key, token string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	entry, ok := l.held[key]
	if !ok || entry.token != token {
		return // already released or replaced
	}
	// Wake up all waiters, then remove the entry.
	close(entry.release)
	delete(l.held, key)
}

// localLock is the Lock returned by localLocker.Acquire.
type localLock struct {
	key    string
	token  string
	locker *localLocker
}

var _ Lock = (*localLock)(nil)

// Token returns the fencing token for this lock holder.
func (lk *localLock) Token() string { return lk.token }

// Release relinquishes the lock. If the TTL timer evicted the entry before
// Release was called, Release is a silent no-op (ownership was already lost).
// If another caller has since acquired the lock under a new token, Release
// returns an error — the caller should treat this as a fencing violation.
func (lk *localLock) Release(_ context.Context) error {
	lk.locker.mu.Lock()
	defer lk.locker.mu.Unlock()

	entry, ok := lk.locker.held[lk.key]
	if !ok {
		// Entry was evicted by TTL; ownership already lost. No-op.
		return nil
	}
	if entry.token != lk.token {
		// A new holder acquired the lock after our TTL expired. Fenced.
		return errors.New("coalesce: lock ownership lost (fenced release blocked)")
	}
	// Stop the TTL timer so evictExpired does not fire after we've cleaned up.
	if entry.timer != nil {
		entry.timer.Stop()
	}
	// Signal all waiters that the lock is now free.
	close(entry.release)
	delete(lk.locker.held, lk.key)
	return nil
}
