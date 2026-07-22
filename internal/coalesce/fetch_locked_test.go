package coalesce_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/coalesce"
)

func TestFetchLocked_NilLocker_FallsBackToFetch(t *testing.T) {
	c := coalesce.NewLocalCoalescer()
	var n atomic.Int32
	v, err := coalesce.FetchLocked(context.Background(), c, nil, "k", 0, nil, func() (string, error) {
		n.Add(1)
		return "ok", nil
	})
	require.NoError(t, err)
	assert.Equal(t, "ok", v)
	assert.Equal(t, int32(1), n.Load())
}

func TestFetchLocked_RecheckHit_SkipsFn(t *testing.T) {
	c := coalesce.NewLocalCoalescer()
	l := coalesce.NewLocalLocker()
	var fnCalls atomic.Int32
	v, err := coalesce.FetchLocked(context.Background(), c, l, "recheck-hit", time.Second,
		func(context.Context) (string, bool, error) {
			return "from-cache", true, nil
		},
		func() (string, error) {
			fnCalls.Add(1)
			return "from-upstream", nil
		},
	)
	require.NoError(t, err)
	assert.Equal(t, "from-cache", v)
	assert.Equal(t, int32(0), fnCalls.Load())
}

func TestFetchLocked_RecheckMiss_RunsFn(t *testing.T) {
	c := coalesce.NewLocalCoalescer()
	l := coalesce.NewLocalLocker()
	v, err := coalesce.FetchLocked(context.Background(), c, l, "recheck-miss", time.Second,
		func(context.Context) (string, bool, error) {
			return "", false, nil
		},
		func() (string, error) {
			return "fetched", nil
		},
	)
	require.NoError(t, err)
	assert.Equal(t, "fetched", v)
}

func TestFetchLocked_SerializesAcrossLeaders(t *testing.T) {
	c1 := coalesce.NewLocalCoalescer()
	c2 := coalesce.NewLocalCoalescer()
	l := coalesce.NewLocalLocker()
	var concurrent atomic.Int32
	var maxConcurrent atomic.Int32
	fn := func() (string, error) {
		cur := concurrent.Add(1)
		for {
			prev := maxConcurrent.Load()
			if cur <= prev || maxConcurrent.CompareAndSwap(prev, cur) {
				break
			}
		}
		time.Sleep(80 * time.Millisecond)
		concurrent.Add(-1)
		return "x", nil
	}
	recheck := func(context.Context) (string, bool, error) { return "", false, nil }

	done := make(chan struct{}, 2)
	go func() {
		_, err := coalesce.FetchLocked(context.Background(), c1, l, "serial", time.Second, recheck, fn)
		assert.NoError(t, err)
		done <- struct{}{}
	}()
	go func() {
		_, err := coalesce.FetchLocked(context.Background(), c2, l, "serial", time.Second, recheck, fn)
		assert.NoError(t, err)
		done <- struct{}{}
	}()
	<-done
	<-done
	assert.Equal(t, int32(1), maxConcurrent.Load(), "distributed lock must serialize leaders")
}
