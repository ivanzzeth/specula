// Package coalesce provides two-tier stampede protection: an in-process
// singleflight Coalescer keyed by immutable identity, plus a cross-instance
// distributed Locker with owner-checked (fenced) release and bounded waiting.
//
// Design notes (ARCHITECTURE.md §7):
//
//   - In-process: sharded singleflight.Group. Sharding reduces mutex contention
//     under high QPS. Key is hashed (FNV-1a) to a shard index.
//
//   - Error amplification defence: DoChan + ctx select; callers time out
//     individually without sharing fn's error.
//
//   - Panic isolation: fn is wrapped to recover() any panic and return it as a
//     *panicError. singleflight never re-panics; the entry is Forgotten so the
//     next caller starts fresh.
//
//   - Distributed Locker: interface defined here; redsync (Redis) is the HA
//     production path. NewLocalLocker() provides a functional in-process
//     implementation with TTL + owner-checked (fenced) Release semantics.
package coalesce

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"runtime/debug"
	"time"

	"golang.org/x/sync/singleflight"
)

// numShards is the number of independent singleflight.Group instances used by
// the local Coalescer. Must be a power of two.
const numShards = 16

// Result is the outcome of a coalesced call.
type Result struct {
	Val    any   // value produced by fn
	Err    error // error produced by fn (may be a *PanicError for recovered panics)
	Shared bool  // true when result was shared with other concurrent callers
}

// Coalescer deduplicates concurrent in-process calls sharing the same key.
type Coalescer interface {
	// Do executes fn once for concurrent callers sharing key. It returns early
	// with ctx.Err() when ctx is cancelled rather than propagating fn's error.
	Do(ctx context.Context, key string, fn func() (any, error)) (any, error, bool)
	// DoChan is the channel variant; the caller selects on the returned channel
	// alongside any other ctx or deadline.
	DoChan(ctx context.Context, key string, fn func() (any, error)) <-chan Result
	// Forget removes a potentially-poisoned in-flight key so the next call
	// re-executes fn.
	Forget(key string)
}

// Lock is a held distributed lock. Release must be owner-checked / fenced.
type Lock interface {
	// Release relinquishes the lock; no-ops safely if ownership has been lost
	// (TTL expiry evicted the entry while the holder was still running).
	Release(ctx context.Context) error
	// Token returns the opaque fencing token proving current ownership.
	Token() string
}

// Locker acquires cross-instance distributed locks with a TTL.
// HA production path: NewRedsyncLocker (go-redsync/redsync over go-redis).
type Locker interface {
	Acquire(ctx context.Context, key string, ttl time.Duration) (Lock, error)
}

// PanicError wraps a recovered panic value, allowing the Coalescer to return
// panics as normal errors instead of crashing the process or all waiters.
type PanicError struct {
	Value any
	Stack []byte
}

func (e *PanicError) Error() string {
	return fmt.Sprintf("coalesce: panic recovered: %v", e.Value)
}

// wrapFn returns a wrapper around fn that catches any panic and converts it to
// a *PanicError. This prevents singleflight from ever seeing a panic, so it
// never re-panics shared goroutines.
func wrapFn(fn func() (any, error)) func() (any, error) {
	return func() (val any, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = &PanicError{Value: r, Stack: debug.Stack()}
			}
		}()
		return fn()
	}
}

// shardIndex maps a key to a shard index using FNV-1a.
func shardIndex(key string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return h.Sum32() & (numShards - 1)
}

// shardedCoalescer is the in-process Coalescer.
type shardedCoalescer struct {
	shards [numShards]singleflight.Group
}

// NewLocalCoalescer constructs an in-process Coalescer backed by sharded
// golang.org/x/sync/singleflight.
func NewLocalCoalescer() Coalescer { return &shardedCoalescer{} }

var _ Coalescer = (*shardedCoalescer)(nil)

func (c *shardedCoalescer) group(key string) *singleflight.Group {
	return &c.shards[shardIndex(key)]
}

// Do executes fn via the singleflight group for key. The call uses DoChan so
// ctx cancellation returns ctx.Err() without waiting for fn to finish. If fn
// panics, the panic is caught, Forget is called so the next caller retries, and
// a *PanicError is returned.
func (c *shardedCoalescer) Do(ctx context.Context, key string, fn func() (any, error)) (any, error, bool) {
	g := c.group(key)
	src := g.DoChan(key, wrapFn(fn))
	select {
	case r := <-src:
		if isPanic(r.Err) {
			g.Forget(key)
		}
		return r.Val, r.Err, r.Shared
	case <-ctx.Done():
		return nil, ctx.Err(), false
	}
}

// DoChan is the channel variant of Do.
func (c *shardedCoalescer) DoChan(ctx context.Context, key string, fn func() (any, error)) <-chan Result {
	g := c.group(key)
	src := g.DoChan(key, wrapFn(fn))
	out := make(chan Result, 1)
	go func() {
		select {
		case r := <-src:
			if isPanic(r.Err) {
				g.Forget(key)
			}
			out <- Result{Val: r.Val, Err: r.Err, Shared: r.Shared}
		case <-ctx.Done():
			out <- Result{Err: ctx.Err()}
		}
	}()
	return out
}

// Forget removes the in-flight entry so the next Do/DoChan for key re-executes.
func (c *shardedCoalescer) Forget(key string) {
	c.group(key).Forget(key)
}

// isPanic reports whether err is a *PanicError produced by wrapFn.
func isPanic(err error) bool {
	var pe *PanicError
	return errors.As(err, &pe)
}
