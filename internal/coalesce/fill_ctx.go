package coalesce

import (
	"context"
	"time"
)

// DefaultFillTimeout bounds a cold-cache fill after it has been detached from
// the HTTP request that triggered it. Client disconnect / containerd header
// timeout must not abort verify-on-write (that deletes quarantine progress and
// forces every retry to restart from byte 0). Large OCI layers on slow CN
// links routinely exceed the former 30s lock-order-of-magnitude window.
const DefaultFillTimeout = 30 * time.Minute

// FillContext derives the context a cold upstream→CAS fill runs under: detached
// from the caller's cancellation, bounded by timeout (DefaultFillTimeout when
// timeout <= 0).
//
// Same pattern as git mirror syncContext: a hang-up cancels a response; it must
// not corrupt shared cache state mid-quarantine. Waiters still bound themselves
// via Coalescer.Do's select on their own ctx.
func FillContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		timeout = DefaultFillTimeout
	}
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}
