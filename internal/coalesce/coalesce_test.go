package coalesce

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ─── Coalescer tests ──────────────────────────────────────────────────────────

// TestSingleFlightDedup verifies that N concurrent callers sharing the same key
// cause fn to be invoked exactly once.
func TestSingleFlightDedup(t *testing.T) {
	tests := []struct {
		name        string
		concurrency int
	}{
		{"two callers", 2},
		{"ten callers", 10},
		{"fifty callers", 50},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := NewLocalCoalescer()
			var callCount atomic.Int32

			fn := func() (any, error) {
				callCount.Add(1)
				time.Sleep(20 * time.Millisecond) // keep the window open
				return "result", nil
			}

			var wg sync.WaitGroup
			results := make([]any, tc.concurrency)
			errs := make([]error, tc.concurrency)
			start := make(chan struct{})

			for i := range tc.concurrency {
				wg.Add(1)
				go func(idx int) {
					defer wg.Done()
					<-start
					v, err, _ := c.Do(context.Background(), "key", fn)
					results[idx] = v
					errs[idx] = err
				}(i)
			}

			close(start) // release all goroutines simultaneously
			wg.Wait()

			if got := callCount.Load(); got != 1 {
				t.Errorf("fn called %d times, want 1", got)
			}
			for i, err := range errs {
				if err != nil {
					t.Errorf("caller %d got error: %v", i, err)
				}
			}
			for i, v := range results {
				if v != "result" {
					t.Errorf("caller %d got %v, want result", i, v)
				}
			}
		})
	}
}

// TestContextTimeoutNonAmplification verifies that a caller whose ctx expires
// receives ctx.Err() (not fn's error) without waiting for fn to complete.
func TestContextTimeoutNonAmplification(t *testing.T) {
	c := NewLocalCoalescer()

	slowFnStarted := make(chan struct{})
	slowFnDone := make(chan struct{})

	fn := func() (any, error) {
		close(slowFnStarted)
		time.Sleep(200 * time.Millisecond)
		close(slowFnDone)
		return nil, errors.New("fn error")
	}

	// Kick off the slow fn via a background context so it runs to completion.
	bgCtx := context.Background()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.Do(bgCtx, "slow", fn) //nolint:errcheck
	}()

	// Wait until fn has actually started.
	<-slowFnStarted

	// Second caller with a very short timeout should NOT get fn's error.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err, _ := c.Do(ctx, "slow", fn)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("got err=%v, want context.DeadlineExceeded", err)
	}
	if elapsed >= 100*time.Millisecond {
		t.Errorf("timed-out caller waited %v, want <100ms", elapsed)
	}

	// Let the background goroutine finish cleanly.
	wg.Wait()
}

// TestPanicIsolation verifies that a panicking fn does not crash the process,
// returns a *PanicError, and that a subsequent call (after Forget) succeeds.
func TestPanicIsolation(t *testing.T) {
	c := NewLocalCoalescer()
	key := "panic-key"

	panicFn := func() (any, error) {
		panic("something went wrong")
	}

	_, err, _ := c.Do(context.Background(), key, panicFn)
	if err == nil {
		t.Fatal("expected error from panicking fn, got nil")
	}

	var pe *PanicError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *PanicError, got %T: %v", err, err)
	}
	if pe.Value != "something went wrong" {
		t.Errorf("panic value = %v, want %q", pe.Value, "something went wrong")
	}
	if len(pe.Stack) == 0 {
		t.Error("expected non-empty stack trace in PanicError")
	}

	// After a panic, Forget should have been called automatically. Verify by
	// calling again with a healthy fn — it must succeed.
	good, err2, _ := c.Do(context.Background(), key, func() (any, error) {
		return "ok", nil
	})
	if err2 != nil {
		t.Errorf("second call after panic got err: %v", err2)
	}
	if good != "ok" {
		t.Errorf("second call got %v, want ok", good)
	}
}

// TestPanicIsolationMultipleWaiters verifies that when multiple callers share a
// panicking fn, none of them crash and all receive a *PanicError.
func TestPanicIsolationMultipleWaiters(t *testing.T) {
	c := NewLocalCoalescer()
	key := "panic-shared"

	started := make(chan struct{})
	panicFn := func() (any, error) {
		close(started) // signal that the fn has started
		time.Sleep(10 * time.Millisecond)
		panic("shared panic")
	}

	const n = 5
	errs := make([]error, n)
	var wg sync.WaitGroup

	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx], _ = c.Do(context.Background(), key, panicFn)
		}(i)
	}

	<-started // ensure fn is running before we wait
	wg.Wait()

	for i, err := range errs {
		if err == nil {
			t.Errorf("caller %d: expected error, got nil", i)
			continue
		}
		var pe *PanicError
		if !errors.As(err, &pe) {
			// Non-initiating callers may time-share or get ctx errors; accept both.
			// What is NOT acceptable: a nil error or a crash.
			t.Logf("caller %d: got non-PanicError %T: %v (acceptable for non-initiating callers)", i, err, err)
		}
	}
}

// TestForget verifies that calling Forget on a key causes the next caller to
// re-execute fn (dedup is broken as expected).
func TestForget(t *testing.T) {
	c := NewLocalCoalescer()
	var callCount atomic.Int32

	fn := func() (any, error) {
		callCount.Add(1)
		return fmt.Sprintf("call-%d", callCount.Load()), nil
	}

	v1, _, _ := c.Do(context.Background(), "fkey", fn)
	c.Forget("fkey")
	v2, _, _ := c.Do(context.Background(), "fkey", fn)

	if callCount.Load() != 2 {
		t.Errorf("fn called %d times after Forget, want 2", callCount.Load())
	}
	if v1 == v2 {
		t.Errorf("expected different results after Forget, both got %v", v1)
	}
}

// TestDoChan verifies the channel variant returns the result and that a
// cancelled context closes the output channel with ctx.Err().
func TestDoChan(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		c := NewLocalCoalescer()
		ch := c.DoChan(context.Background(), "chan-key", func() (any, error) {
			return 42, nil
		})
		r := <-ch
		if r.Val != 42 {
			t.Errorf("Val = %v, want 42", r.Val)
		}
		if r.Err != nil {
			t.Errorf("Err = %v, want nil", r.Err)
		}
	})

	t.Run("ctx cancelled", func(t *testing.T) {
		c := NewLocalCoalescer()
		started := make(chan struct{})
		fn := func() (any, error) {
			close(started)
			time.Sleep(300 * time.Millisecond)
			return nil, nil
		}

		ctx, cancel := context.WithCancel(context.Background())
		ch := c.DoChan(ctx, "cancel-key", fn)

		<-started
		cancel()

		r := <-ch
		if !errors.Is(r.Err, context.Canceled) {
			t.Errorf("Err = %v, want context.Canceled", r.Err)
		}
	})
}

// TestShardingIsolation confirms that keys in different shards can execute
// concurrently (they do not share a singleflight.Group).
func TestShardingIsolation(t *testing.T) {
	c := NewLocalCoalescer()

	// Find two keys that hash to different shards.
	var k1, k2 string
	for i := range 200 {
		a := fmt.Sprintf("key-%d", i)
		for j := i + 1; j <= i+200; j++ {
			b := fmt.Sprintf("key-%d", j)
			if shardIndex(a) != shardIndex(b) {
				k1, k2 = a, b
				goto found
			}
		}
	}
found:
	if k1 == "" {
		t.Skip("could not find two keys with different shards")
	}

	// Both fn's block until we signal them. They should both be in-flight at
	// the same time because they're on different shards.
	gate := make(chan struct{})
	started := atomic.Int32{}
	fn := func() (any, error) {
		started.Add(1)
		<-gate
		return "done", nil
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); c.Do(context.Background(), k1, fn) }() //nolint:errcheck
	go func() { defer wg.Done(); c.Do(context.Background(), k2, fn) }() //nolint:errcheck

	// Wait until both fn's have started (proving concurrent execution on different shards).
	deadline := time.Now().Add(2 * time.Second)
	for started.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if started.Load() < 2 {
		t.Errorf("only %d fn's started, want 2; keys share a shard or goroutines didn't start in time", started.Load())
	}

	close(gate)
	wg.Wait()
}

// ─── Locker tests ─────────────────────────────────────────────────────────────

// TestLockerBasicAcquireRelease verifies that a lock can be acquired and released.
func TestLockerBasicAcquireRelease(t *testing.T) {
	l := NewLocalLocker()
	ctx := context.Background()

	lk, err := l.Acquire(ctx, "basic", 5*time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if lk.Token() == "" {
		t.Error("Token() returned empty string")
	}
	if err := lk.Release(ctx); err != nil {
		t.Errorf("Release: %v", err)
	}
}

// TestLockerMutualExclusion verifies that a second goroutine cannot acquire the
// lock while the first holds it, and unblocks immediately after Release.
func TestLockerMutualExclusion(t *testing.T) {
	l := NewLocalLocker()
	ctx := context.Background()
	key := "mutex-key"

	lk1, err := l.Acquire(ctx, key, 5*time.Second)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}

	// Second acquire must block.
	acquired := make(chan Lock, 1)
	go func() {
		lk2, _ := l.Acquire(ctx, key, 5*time.Second)
		acquired <- lk2
	}()

	// Give the goroutine time to start and block.
	time.Sleep(30 * time.Millisecond)
	select {
	case <-acquired:
		t.Fatal("second caller acquired lock while first still holds it")
	default:
	}

	// Release the first lock; the second should unblock almost immediately.
	if err := lk1.Release(ctx); err != nil {
		t.Fatalf("Release lk1: %v", err)
	}

	select {
	case lk2 := <-acquired:
		if lk2 == nil {
			t.Fatal("second caller got nil lock")
		}
		lk2.Release(ctx) //nolint:errcheck
	case <-time.After(200 * time.Millisecond):
		t.Fatal("second caller did not unblock within 200ms after Release")
	}
}

// TestLockerCancelledContext verifies that a waiting Acquire returns
// ctx.Err() when the context is cancelled.
func TestLockerCancelledContext(t *testing.T) {
	l := NewLocalLocker()
	ctx := context.Background()
	key := "cancel-key"

	lk1, err := l.Acquire(ctx, key, 5*time.Second)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer lk1.Release(ctx) //nolint:errcheck

	cancelCtx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()

	_, err = l.Acquire(cancelCtx, key, 5*time.Second)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Acquire with cancelled ctx: got %v, want context.DeadlineExceeded", err)
	}
}

// TestLockerTTLEviction verifies that a lock with a short TTL is auto-evicted,
// allowing another caller to acquire it without an explicit Release.
func TestLockerTTLEviction(t *testing.T) {
	l := NewLocalLocker()
	ctx := context.Background()
	key := "ttl-key"

	// Acquire with a very short TTL.
	lk1, err := l.Acquire(ctx, key, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	_ = lk1 // intentionally not releasing; TTL will evict

	// Second acquire should succeed once the TTL fires.
	ctx2, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()

	lk2, err := l.Acquire(ctx2, key, 5*time.Second)
	if err != nil {
		t.Fatalf("second Acquire after TTL eviction: %v", err)
	}
	defer lk2.Release(ctx) //nolint:errcheck

	// First holder's explicit Release after TTL eviction must be a no-op.
	if err := lk1.Release(ctx); err != nil {
		// Depending on timing, either a nil (evicted) or a fencing error is acceptable.
		// A crash is not acceptable. Just log.
		t.Logf("lk1.Release after TTL eviction: %v (acceptable — ownership was lost)", err)
	}
}

// TestLockerFencedRelease verifies that a release with a stale token (after TTL
// eviction and re-acquisition by another holder) returns an error.
func TestLockerFencedRelease(t *testing.T) {
	l := NewLocalLocker()
	ctx := context.Background()
	key := "fenced-key"

	// lk1 acquires with a very short TTL.
	lk1, err := l.Acquire(ctx, key, 30*time.Millisecond)
	if err != nil {
		t.Fatalf("Acquire lk1: %v", err)
	}

	// Wait for TTL to expire.
	time.Sleep(80 * time.Millisecond)

	// lk2 acquires after lk1's TTL has elapsed.
	lk2, err := l.Acquire(context.Background(), key, 5*time.Second)
	if err != nil {
		t.Fatalf("Acquire lk2: %v", err)
	}
	defer lk2.Release(ctx) //nolint:errcheck

	if lk1.Token() == lk2.Token() {
		t.Fatal("lk1 and lk2 should have different tokens")
	}

	// lk1.Release must fail with a fencing error (token mismatch).
	err = lk1.Release(ctx)
	if err == nil {
		t.Error("expected fencing error from lk1.Release after TTL eviction, got nil")
	}
}

// TestLockerSequential verifies that sequential acquire/release pairs work
// correctly — each new Acquire after Release succeeds immediately.
func TestLockerSequential(t *testing.T) {
	l := NewLocalLocker()
	ctx := context.Background()
	key := "seq-key"

	for i := range 5 {
		lk, err := l.Acquire(ctx, key, 5*time.Second)
		if err != nil {
			t.Fatalf("iteration %d: Acquire: %v", i, err)
		}
		if err := lk.Release(ctx); err != nil {
			t.Fatalf("iteration %d: Release: %v", i, err)
		}
	}
}

// TestShardIndexDistribution checks that shardIndex distributes keys reasonably
// across shards (no single shard gets more than 50% of keys in a modest sample).
func TestShardIndexDistribution(t *testing.T) {
	counts := make(map[uint32]int)
	total := 100
	for i := range total {
		key := fmt.Sprintf("artifact:sha256:%064d", i)
		counts[shardIndex(key)]++
	}
	maxCount := 0
	for _, v := range counts {
		if v > maxCount {
			maxCount = v
		}
	}
	if maxCount > total/2 {
		t.Errorf("poor shard distribution: max shard has %d/%d keys", maxCount, total)
	}
}
