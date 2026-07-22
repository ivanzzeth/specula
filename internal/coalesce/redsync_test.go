package coalesce_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/coalesce"
)

func TestNewRedsyncLocker_RequiresAddr(t *testing.T) {
	_, _, err := coalesce.NewRedsyncLocker(coalesce.RedisOptions{})
	require.Error(t, err)
}

func TestRedsyncLocker_AcquireRelease(t *testing.T) {
	mr := miniredis.RunT(t)
	locker, closeFn, err := coalesce.NewRedsyncLocker(coalesce.RedisOptions{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = closeFn() })

	ctx := context.Background()
	lk, err := locker.Acquire(ctx, "k1", time.Second)
	require.NoError(t, err)
	require.NotEmpty(t, lk.Token())
	require.NoError(t, lk.Release(ctx))
}

func TestRedsyncLocker_Serializes(t *testing.T) {
	mr := miniredis.RunT(t)
	locker, closeFn, err := coalesce.NewRedsyncLocker(coalesce.RedisOptions{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = closeFn() })

	var concurrent atomic.Int32
	var maxSeen atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			lk, err := locker.Acquire(ctx, "serial", 2*time.Second)
			require.NoError(t, err)
			n := concurrent.Add(1)
			for {
				cur := maxSeen.Load()
				if n <= cur || maxSeen.CompareAndSwap(cur, n) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			concurrent.Add(-1)
			require.NoError(t, lk.Release(ctx))
		}()
	}
	wg.Wait()
	require.Equal(t, int32(1), maxSeen.Load(), "redsync must serialize holders")
}

func TestFetchLocked_WithRedsync_RecheckHit(t *testing.T) {
	mr := miniredis.RunT(t)
	locker, closeFn, err := coalesce.NewRedsyncLocker(coalesce.RedisOptions{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = closeFn() })

	c := coalesce.NewLocalCoalescer()
	calls := 0
	v, err := coalesce.FetchLocked(context.Background(), c, locker, "rk", time.Second,
		func(context.Context) (string, bool, error) { return "from-cache", true, nil },
		func() (string, error) {
			calls++
			return "from-fn", nil
		},
	)
	require.NoError(t, err)
	require.Equal(t, "from-cache", v)
	require.Equal(t, 0, calls)
}
