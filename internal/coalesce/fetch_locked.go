package coalesce

import (
	"context"
	"fmt"
	"time"
)

// DefaultLockTTL bounds how long a cold-fetch leader may hold the distributed
// lock (redsync expiry) while the upstream runs. Matches the upstream client's
// whole-request timeout order of magnitude.
const DefaultLockTTL = 30 * time.Second

// FetchLocked is Fetch plus a cross-instance Locker (ARCHITECTURE §7 tier 2).
//
// Flow for the in-process singleflight leader:
//
//  1. Acquire(key) — other replicas block here instead of double-fetching.
//  2. recheck(ctx) — MUST re-query the shared cache after the lock; another
//     replica may have populated it while we waited.
//  3. On miss, run fn (upstream + store).
//  4. Release.
//
// Followers on the SAME process still collapse via Coalescer and never touch
// the Locker. A nil locker falls back to plain Fetch (recheck unused).
//
// recheck returns (value, hit, err). hit=true means serve the value without fn.
func FetchLocked[T any](
	ctx context.Context,
	c Coalescer,
	locker Locker,
	key string,
	lockTTL time.Duration,
	recheck func(context.Context) (T, bool, error),
	fn func() (T, error),
) (T, error) {
	if locker == nil {
		return Fetch(ctx, c, key, fn)
	}
	if lockTTL <= 0 {
		lockTTL = DefaultLockTTL
	}
	return Fetch(ctx, c, key, func() (T, error) {
		var zero T
		lk, err := locker.Acquire(ctx, key, lockTTL)
		if err != nil {
			return zero, fmt.Errorf("coalesce: lock %q: %w", key, err)
		}
		defer func() { _ = lk.Release(context.WithoutCancel(ctx)) }()

		if recheck != nil {
			v, hit, rerr := recheck(ctx)
			if rerr != nil {
				return zero, rerr
			}
			if hit {
				return v, nil
			}
		}
		return fn()
	})
}
