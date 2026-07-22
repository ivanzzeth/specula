package upstream

import (
	"context"
	"sync"
	"time"
)

const (
	// defaultMaxFailures is the number of consecutive transient errors (5xx /
	// network errors) that trigger an auto-block of an upstream.
	defaultMaxFailures = 5

	// defaultBlockDuration is how long a blocked upstream stays blocked before
	// it is automatically re-admitted on the next isBlocked check.
	defaultBlockDuration = 30 * time.Second
)

// BlockState is persisted auto-block circuit breaker state for one upstream mirror.
type BlockState struct {
	Failures     int
	BlockedUntil time.Time // zero when not blocked
}

// BlockPersister stores auto-block state keyed by upstream mirror name within one
// protocol namespace. When nil on a blockTracker, state stays in-memory.
type BlockPersister interface {
	Load(ctx context.Context, upstream string) (BlockState, error)
	Save(ctx context.Context, upstream string, state BlockState) error
	Delete(ctx context.Context, upstream string) error
}

// blockTracker counts consecutive transient failures per upstream name and
// temporarily blocks upstreams that exceed the failure threshold.
// All methods are safe for concurrent use.
type blockTracker struct {
	mu           sync.Mutex
	failures     map[string]int
	blockedUntil map[string]time.Time
	persister    BlockPersister
	maxFailures  int
	blockDur     time.Duration
}

func newBlockTracker() *blockTracker {
	return newBlockTrackerWith(defaultMaxFailures, defaultBlockDuration)
}

func newBlockTrackerWith(maxFailures int, blockDur time.Duration) *blockTracker {
	return newBlockTrackerWithPersister(nil, maxFailures, blockDur)
}

func newBlockTrackerWithPersister(persister BlockPersister, maxFailures int, blockDur time.Duration) *blockTracker {
	return &blockTracker{
		failures:     make(map[string]int),
		blockedUntil: make(map[string]time.Time),
		persister:    persister,
		maxFailures:  maxFailures,
		blockDur:     blockDur,
	}
}

// isBlocked returns true if the upstream named name is in its block period.
// When the block period has elapsed the entry is cleared and false is returned,
// allowing the upstream to be retried (auto-unblock).
func (b *blockTracker) isBlocked(name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.persister != nil {
		return b.isBlockedPersisted(name)
	}
	until, ok := b.blockedUntil[name]
	if !ok {
		return false
	}
	if time.Now().Before(until) {
		return true
	}
	// Block period expired: auto-unblock.
	delete(b.blockedUntil, name)
	delete(b.failures, name)
	return false
}

func (b *blockTracker) isBlockedPersisted(name string) bool {
	ctx := context.Background()
	st, err := b.persister.Load(ctx, name)
	if err != nil || st.BlockedUntil.IsZero() {
		return false
	}
	if time.Now().Before(st.BlockedUntil) {
		return true
	}
	_ = b.persister.Delete(ctx, name)
	return false
}

// recordFailure increments the consecutive-transient-failure counter for name.
// When the counter reaches maxFailures the upstream is blocked for blockDur.
// Returns true when name transitions to blocked.
func (b *blockTracker) recordFailure(name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.persister != nil {
		return b.recordFailurePersisted(name)
	}
	b.failures[name]++
	if b.failures[name] >= b.maxFailures {
		if _, already := b.blockedUntil[name]; !already {
			b.blockedUntil[name] = time.Now().Add(b.blockDur)
			return true
		}
	}
	return false
}

func (b *blockTracker) recordFailurePersisted(name string) bool {
	ctx := context.Background()
	st, err := b.persister.Load(ctx, name)
	if err != nil {
		return false
	}
	st.Failures++
	blocked := false
	if st.Failures >= b.maxFailures {
		if st.BlockedUntil.IsZero() {
			st.BlockedUntil = time.Now().Add(b.blockDur)
			blocked = true
		}
	}
	_ = b.persister.Save(ctx, name, st)
	return blocked
}

// recordSuccess clears any failure state and removes any block for name.
func (b *blockTracker) recordSuccess(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.persister != nil {
		_ = b.persister.Delete(context.Background(), name)
		return
	}
	delete(b.failures, name)
	delete(b.blockedUntil, name)
}

// failureCount returns the current consecutive failure count for name (test helper).
func (b *blockTracker) failureCount(name string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.persister != nil {
		st, err := b.persister.Load(context.Background(), name)
		if err != nil {
			return 0
		}
		return st.Failures
	}
	return b.failures[name]
}

// blockedUntilTime returns when name's block window expires, or the zero time
// when it is not currently blocked. Unlike isBlocked it is a pure read: it does
// not clear an expired entry, so it is safe to call while rendering a snapshot.
// An already-elapsed instant therefore means "expired but not yet reaped" and
// must be treated as unblocked — callers pair this with isBlocked.
func (b *blockTracker) blockedUntilTime(name string) time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.persister != nil {
		st, err := b.persister.Load(context.Background(), name)
		if err != nil {
			return time.Time{}
		}
		return st.BlockedUntil
	}
	return b.blockedUntil[name]
}
