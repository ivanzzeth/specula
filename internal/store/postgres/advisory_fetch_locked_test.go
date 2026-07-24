package postgres

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/coalesce"
)

// TestFetchLocked_PGAdvisoryLocker_SerializesAndRechecks proves the HA
// coalesce path under PGAdvisoryLocker: two leaders with distinct
// singleflight groups share one fn execution; the waiter hits recheck after
// the first store completes (pattern from coalesce/fetch_locked_test.go).
func TestFetchLocked_PGAdvisoryLocker_SerializesAndRechecks(t *testing.T) {
	store := newTestStore(t)
	locker := NewPGAdvisoryLocker(store.Pool())

	c1 := coalesce.NewLocalCoalescer()
	c2 := coalesce.NewLocalCoalescer()

	var fnCalls atomic.Int32
	var concurrent atomic.Int32
	var maxConcurrent atomic.Int32
	var mu sync.Mutex
	cached := false

	recheck := func(context.Context) (string, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		if cached {
			return "from-cache", true, nil
		}
		return "", false, nil
	}
	fn := func() (string, error) {
		fnCalls.Add(1)
		cur := concurrent.Add(1)
		for {
			prev := maxConcurrent.Load()
			if cur <= prev || maxConcurrent.CompareAndSwap(prev, cur) {
				break
			}
		}
		time.Sleep(80 * time.Millisecond)
		concurrent.Add(-1)
		mu.Lock()
		cached = true
		mu.Unlock()
		return "from-upstream", nil
	}

	const key = "pg-advisory-fetch-locked"
	ctx := context.Background()
	done := make(chan struct{}, 2)
	var v1, v2 string
	var err1, err2 error

	go func() {
		v1, err1 = coalesce.FetchLocked(ctx, c1, locker, key, 5*time.Second, recheck, fn)
		done <- struct{}{}
	}()
	go func() {
		v2, err2 = coalesce.FetchLocked(ctx, c2, locker, key, 5*time.Second, recheck, fn)
		done <- struct{}{}
	}()
	<-done
	<-done

	require.NoError(t, err1)
	require.NoError(t, err2)
	assert.Equal(t, int32(1), maxConcurrent.Load(), "PG advisory lock must serialize leaders")
	assert.Equal(t, int32(1), fnCalls.Load(), "only one leader should run fn")
	// One result from upstream, one from post-Acquire recheck hit.
	got := map[string]bool{v1: true, v2: true}
	assert.True(t, got["from-upstream"], "leader must return fn result")
	assert.True(t, got["from-cache"], "waiter must hit recheck after leader stores")
}
