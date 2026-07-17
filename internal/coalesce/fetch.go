package coalesce

import (
	"context"
	"fmt"
	"strings"
)

// FetchKey builds the collapse key for a COLD FETCH: the request identity.
//
// This is the honest key, and picking it correctly is the whole fix.
// cache.Store coalesces on the content DIGEST, which is only knowable AFTER the
// download has already happened — so it collapses the cheap tail (verify +
// promote) and never the expensive head (the upstream round trip). ARCHITECTURE
// §7 promises collapse of the round trip; PRD §G5 (CN-first) is why that
// matters, since an uncollapsed round trip is a slow one.
//
// Before the bytes arrive, the only thing N concurrent callers demonstrably
// share is what they ASKED for: protocol + name + version.
//
// digest is the caller's optional PIN, and it belongs in the key even though it
// is not known-in-advance content. A pin is an assertion ("serve me these bytes
// or fail"), so two callers pinning different digests are NOT making the same
// request and must not share one result — collapsing them would hand a caller
// an artifact contradicting its own pin, trading a performance bug for a
// correctness one. An empty digest means "no pin" and collapses freely with
// other unpinned callers, which is the overwhelmingly common case.
func FetchKey(protocol, name, version, digest string) string {
	var b strings.Builder
	b.Grow(len(protocol) + len(name) + len(version) + len(digest) + 8)
	b.WriteString("fetch|")
	b.WriteString(protocol)
	b.WriteByte('|')
	b.WriteString(name)
	b.WriteByte('|')
	b.WriteString(version)
	b.WriteByte('|')
	b.WriteString(digest)
	return b.String()
}

// Fetch collapses concurrent cold fetches sharing key onto one execution of fn,
// returning fn's typed result to every caller.
//
// It is a thin typed wrapper over Coalescer.Do so the six protocol handlers do
// not each hand-roll the same `any` type assertion — a cast that, done wrong in
// one handler, would silently return a nil entry instead of failing.
//
// # Failure semantics (deliberate)
//
// When the leader's fn FAILS, every follower receives the leader's error rather
// than re-fetching. This is the decision that matters, so it is stated
// explicitly:
//
//   - Followers re-fetching would re-create the stampede under exactly the
//     upstream-trouble conditions where it hurts most: the upstream is already
//     failing, and N callers pile on to fail again. The leader's fn already
//     performed the configured retry + multi-upstream fallback internally, so a
//     follower's independent attempt is not a fresh chance at success — it is
//     the same attempt, repeated, against the same struggling upstream.
//   - This does NOT turn one blip into N failures: sharing the error returns
//     each follower to its own handler, which independently applies serve-stale
//     (ARCHITECTURE §3). Degradation stays per-request and costs zero further
//     upstream contact. A collapsed fetch and serve-stale compose precisely
//     because the collapse ends at the error and the fallback happens after it.
//   - Where no stale copy exists, all N callers would have failed anyway; the
//     collapse merely spares the upstream N-1 doomed round trips.
//
// The error is not cached: singleflight drops the in-flight entry as soon as fn
// returns, so the next request after a failure is a fresh leader. A transient
// failure therefore self-heals on the following request rather than being
// pinned for a TTL.
//
// # Bounded waiting
//
// A leader that hangs must not pin followers forever. Two independent bounds
// apply and neither depends on the leader being well-behaved:
//
//   - Each follower selects on its OWN ctx (Coalescer.Do), so a client that
//     disconnects or times out is released immediately with ctx.Err(),
//     regardless of what the leader is doing.
//   - The upstream client imposes a hard 30s whole-request timeout, which bounds
//     the leader itself. "The leader is slow" is an ordinary state, not an edge
//     case — 27 kB/s has been measured on a real CN link — so the bound has to
//     come from the request deadline rather than from optimism.
//
// Note that ARCHITECTURE §7's "waiter times out and fetches for itself" is
// deliberately NOT implemented for the in-process tier: a waiter that gives up
// and fetches independently is exactly the stampede this collapses, just
// delayed by the timeout. The follower's own ctx is the honest bound.
func Fetch[T any](ctx context.Context, c Coalescer, key string, fn func() (T, error)) (T, error) {
	var zero T
	v, err, _ := c.Do(ctx, key, func() (any, error) {
		return fn()
	})
	if err != nil {
		return zero, err
	}
	// A nil-valued success is meaningful, not an error: handlers return
	// (nil, nil) for "the upstream says this does not exist". Type-asserting a
	// nil interface must not panic, and must not be mistaken for a failure.
	if v == nil {
		return zero, nil
	}
	t, ok := v.(T)
	if !ok {
		// Unreachable while fn is the only producer of v — but if it ever became
		// reachable, returning (zero, nil) would render a type confusion as a
		// perfectly ordinary 404, which is precisely the class of self-consistent
		// lie this whole change exists to remove. Fail loudly instead.
		return zero, fmt.Errorf("coalesce: fetch %q: result type %T is not %T", key, v, zero)
	}
	return t, nil
}
