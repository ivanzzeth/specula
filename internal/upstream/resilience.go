package upstream

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/circuitbreaker"
	"github.com/failsafe-go/failsafe-go/retrypolicy"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// defaultAttemptHeaderTimeout is retained as documentation of the header-wait
// budget. failsafe Timeout is NOT composed around tryOnce: cancelling the
// execution context would cancel the HTTP request and kill the streaming body.
// Header waits are enforced by Transport.ResponseHeaderTimeout instead.
const defaultAttemptHeaderTimeout = 35 * time.Second

// fetchAttempt is the success payload of one upstream HTTP attempt (headers
// only). The response body is streamed outside failsafe Retry/CB.
type fetchAttempt struct {
	Body    io.ReadCloser
	Meta    artifact.UpstreamMeta
	Latency time.Duration
}

// breakerHub owns per-upstream failsafe CircuitBreakers, shared across Fetch
// calls and (when wired) the operator Runtime. One breaker per mirror name.
type breakerHub struct {
	mu          sync.Mutex
	breakers    map[string]circuitbreaker.CircuitBreaker[fetchAttempt]
	consecutive map[string]int // operator-facing streak (Snapshot probing)
	persister   BlockPersister
	maxFails    uint
	blockDur    time.Duration
}

func newBreakerHub(maxFails int, blockDur time.Duration) *breakerHub {
	return newBreakerHubWithPersister(nil, maxFails, blockDur)
}

func newBreakerHubWithPersister(persister BlockPersister, maxFails int, blockDur time.Duration) *breakerHub {
	if maxFails <= 0 {
		maxFails = defaultMaxFailures
	}
	if blockDur <= 0 {
		blockDur = defaultBlockDuration
	}
	return &breakerHub{
		breakers:    make(map[string]circuitbreaker.CircuitBreaker[fetchAttempt]),
		consecutive: make(map[string]int),
		persister:   persister,
		maxFails:    uint(maxFails),
		blockDur:    blockDur,
	}
}

func (h *breakerHub) breaker(name string) circuitbreaker.CircuitBreaker[fetchAttempt] {
	h.mu.Lock()
	if b, ok := h.breakers[name]; ok {
		h.mu.Unlock()
		return b
	}
	upName := name
	hub := h
	b := circuitbreaker.NewBuilder[fetchAttempt]().
		WithFailureThreshold(h.maxFails).
		WithDelay(h.blockDur).
		WithSuccessThreshold(1).
		HandleIf(func(_ fetchAttempt, err error) bool {
			return isTransientExecError(err)
		}).
		OnStateChanged(func(e circuitbreaker.StateChangedEvent) {
			hub.persistState(upName, e)
		}).
		Build()
	h.breakers[name] = b
	h.mu.Unlock()
	// Hydrate outside the lock: Open() fires OnStateChanged → persistState.
	h.hydrate(upName, b)
	return b
}

func (h *breakerHub) hydrate(name string, b circuitbreaker.CircuitBreaker[fetchAttempt]) {
	if h.persister == nil {
		return
	}
	st, err := h.persister.Load(context.Background(), name)
	if err != nil || st.BlockedUntil.IsZero() {
		return
	}
	if time.Now().Before(st.BlockedUntil) {
		b.Open()
	} else {
		_ = h.persister.Delete(context.Background(), name)
	}
}

func (h *breakerHub) persistState(name string, e circuitbreaker.StateChangedEvent) {
	if h.persister == nil {
		return
	}
	ctx := context.Background()
	switch e.NewState {
	case circuitbreaker.OpenState:
		delay := h.blockDur
		h.mu.Lock()
		b := h.breakers[name]
		h.mu.Unlock()
		if b != nil {
			if d := b.RemainingDelay(); d > 0 {
				delay = d
			}
		}
		_ = h.persister.Save(ctx, name, BlockState{
			Failures:     int(h.maxFails),
			BlockedUntil: time.Now().Add(delay),
		})
	case circuitbreaker.ClosedState:
		_ = h.persister.Delete(ctx, name)
	}
}

func (h *breakerHub) isOpen(name string) bool {
	b := h.breaker(name)

	// Cross-runtime sync: persister is the source of truth when present.
	if h.persister != nil {
		st, err := h.persister.Load(context.Background(), name)
		if err == nil {
			if st.BlockedUntil.IsZero() || !time.Now().Before(st.BlockedUntil) {
				if b.IsOpen() {
					b.Close()
				}
				h.mu.Lock()
				h.consecutive[name] = 0
				h.mu.Unlock()
				if !st.BlockedUntil.IsZero() {
					_ = h.persister.Delete(context.Background(), name)
				}
				return false
			}
			if !b.IsOpen() {
				b.Open()
			}
			return true
		}
	}

	if !b.IsOpen() {
		return false
	}
	// Lazy admit once the open delay has elapsed so the chain can probe again
	// (failsafe transitions Open→HalfOpen on the next TryAcquirePermit).
	if b.RemainingDelay() <= 0 {
		h.mu.Lock()
		h.consecutive[name] = 0
		h.mu.Unlock()
		return false
	}
	return true
}

func (h *breakerHub) closeBreaker(name string) {
	h.mu.Lock()
	h.consecutive[name] = 0
	h.mu.Unlock()
	h.breaker(name).Close()
	if h.persister != nil {
		_ = h.persister.Delete(context.Background(), name)
	}
}

func (h *breakerHub) recordFailure(name string) bool {
	h.mu.Lock()
	h.consecutive[name]++
	h.mu.Unlock()
	b := h.breaker(name)
	wasOpen := b.IsOpen()
	b.RecordFailure()
	return !wasOpen && b.IsOpen()
}

func (h *breakerHub) recordSuccess(name string) {
	h.mu.Lock()
	h.consecutive[name] = 0
	h.mu.Unlock()
	h.breaker(name).Close()
}

func (h *breakerHub) failureCount(name string) int {
	// Ensure breaker exists (hydrate side effects) without racing consecutive.
	_ = h.breaker(name)
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.consecutive[name]
}

func (h *breakerHub) blockedUntil(name string) time.Time {
	b := h.breaker(name)
	if !b.IsOpen() {
		return time.Time{}
	}
	d := b.RemainingDelay()
	if d <= 0 {
		return time.Time{}
	}
	return time.Now().Add(d)
}

// runFetchAttempt executes fn under Retry + CircuitBreaker.
// maxAttempts is total tries (1 = no retry). Composition: Retry(CB(fn)).
//
// Header-wait limiting uses Transport.ResponseHeaderTimeout — a failsafe
// Timeout must not wrap the HTTP request context or it cancels the body stream.
func (h *breakerHub) runFetchAttempt(
	ctx context.Context,
	upName string,
	maxAttempts int,
	backoffBase time.Duration,
	fn func(context.Context) (fetchAttempt, error),
) (fetchAttempt, bool, error) {
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	if backoffBase <= 0 {
		backoffBase = defaultBackoffBase
	}

	breaker := h.breaker(upName)
	retry := retrypolicy.NewBuilder[fetchAttempt]().
		HandleIf(func(_ fetchAttempt, err error) bool {
			return isTransientExecError(err)
		}).
		WithMaxRetries(maxAttempts-1).
		WithBackoff(backoffBase, 2*time.Second).
		Build()

	out, err := failsafe.With(retry, breaker).WithContext(ctx).Get(func() (fetchAttempt, error) {
		// Use the caller ctx (not a failsafe child): the HTTP response body must
		// outlive the policy execution. A cancelled execution context would
		// abort mid-stream reads after headers return.
		return fn(ctx)
	})
	if err != nil {
		if isTransientExecError(err) {
			h.mu.Lock()
			h.consecutive[upName]++
			h.mu.Unlock()
		}
		transient := isTransientExecError(err) ||
			errors.Is(err, circuitbreaker.ErrOpen) ||
			errors.Is(err, retrypolicy.ErrExceeded)
		return fetchAttempt{}, transient, err
	}
	h.mu.Lock()
	h.consecutive[upName] = 0
	h.mu.Unlock()
	return out, false, nil
}

// isTransientExecError reports whether err should count toward retry / CB.
// Definitive *StatusError and parent-context cancellation are never transient.
// Dial/TLS/header deadlines wrapped with asTransient ARE transient (CN CDN).
func isTransientExecError(err error) bool {
	if err == nil {
		return false
	}
	var te *transientError
	if errors.As(err, &te) {
		return true
	}
	if isContextError(err) {
		return false
	}
	var se *StatusError
	if errors.As(err, &se) {
		return false
	}
	if errors.Is(err, circuitbreaker.ErrOpen) {
		return false
	}
	if errors.Is(err, retrypolicy.ErrExceeded) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "HTTP 429") ||
		strings.Contains(msg, "rate limited") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "reset by peer") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "timeout awaiting response headers") ||
		strings.Contains(msg, "EOF")
}

// transientError marks a retryable upstream failure for failsafe HandleIf.
type transientError struct {
	err error
}

func (e *transientError) Error() string { return e.err.Error() }
func (e *transientError) Unwrap() error { return e.err }

func asTransient(err error) error {
	if err == nil {
		return nil
	}
	return &transientError{err: err}
}
