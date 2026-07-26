package upstream

import (
	"context"
	"time"
)

const (
	// defaultMaxFailures is the number of consecutive transient errors (5xx /
	// network errors) that trigger an auto-block of an upstream.
	defaultMaxFailures = 5

	// defaultBlockDuration is how long a blocked upstream stays blocked before
	// it is automatically re-admitted (failsafe CB half-open delay).
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

// blockTracker is a thin facade over breakerHub (failsafe CircuitBreaker) so
// Runtime / metrics / existing call sites keep a stable API.
type blockTracker struct {
	hub *breakerHub
}

func newBlockTracker() *blockTracker {
	return newBlockTrackerWith(defaultMaxFailures, defaultBlockDuration)
}

func newBlockTrackerWith(maxFailures int, blockDur time.Duration) *blockTracker {
	return newBlockTrackerWithPersister(nil, maxFailures, blockDur)
}

func newBlockTrackerWithPersister(persister BlockPersister, maxFailures int, blockDur time.Duration) *blockTracker {
	return &blockTracker{hub: newBreakerHubWithPersister(persister, maxFailures, blockDur)}
}

func (b *blockTracker) isBlocked(name string) bool {
	return b.hub.isOpen(name)
}

func (b *blockTracker) recordFailure(name string) bool {
	return b.hub.recordFailure(name)
}

func (b *blockTracker) recordSuccess(name string) {
	b.hub.recordSuccess(name)
}

func (b *blockTracker) failureCount(name string) int {
	return b.hub.failureCount(name)
}

func (b *blockTracker) blockedUntilTime(name string) time.Time {
	return b.hub.blockedUntil(name)
}

// unblock forces the circuit closed (operator Unblock / test helper).
func (b *blockTracker) unblock(name string) {
	b.hub.closeBreaker(name)
}
