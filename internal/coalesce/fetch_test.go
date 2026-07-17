package coalesce_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/coalesce"
)

// TestFetchKey_RequestIdentity_NotContentDigest documents the key choice that
// is the whole point of the fix: the key is what the caller ASKED for, so it is
// computable BEFORE the download.
func TestFetchKey_RequestIdentity_NotContentDigest(t *testing.T) {
	a := coalesce.FetchKey("gomod", "rsc.io/quote", "v1.5.1.mod", "")
	b := coalesce.FetchKey("gomod", "rsc.io/quote", "v1.5.1.mod", "")
	assert.Equal(t, a, b, "the same request must produce the same key")

	assert.NotEqual(t, a, coalesce.FetchKey("gomod", "rsc.io/quote", "v1.5.2.mod", ""),
		"a different version is a different request")
	assert.NotEqual(t, a, coalesce.FetchKey("npm", "rsc.io/quote", "v1.5.1.mod", ""),
		"a different protocol is a different request")
	assert.NotEqual(t, a, coalesce.FetchKey("gomod", "rsc.io/other", "v1.5.1.mod", ""),
		"a different name is a different request")
}

// TestFetchKey_DifferentPins_DoNotCollapse guards the correctness half of the
// key design. Two callers pinning DIFFERENT digests are not making the same
// request; sharing one result between them would hand a caller bytes that
// contradict its own pin — trading a performance bug for a correctness bug.
func TestFetchKey_DifferentPins_DoNotCollapse(t *testing.T) {
	pinA := coalesce.FetchKey("gomod", "rsc.io/quote", "v1.5.1.mod", "sha256:aaa")
	pinB := coalesce.FetchKey("gomod", "rsc.io/quote", "v1.5.1.mod", "sha256:bbb")
	unpinned := coalesce.FetchKey("gomod", "rsc.io/quote", "v1.5.1.mod", "")

	assert.NotEqual(t, pinA, pinB, "contradicting pins must never share a fetch")
	assert.NotEqual(t, pinA, unpinned, "a pinned request differs from an unpinned one")
}

// TestFetchKey_FieldsCannotBeConfused ensures the separator makes the key
// injective: ("a","b") and ("ab","") must not collide.
func TestFetchKey_FieldsCannotBeConfused(t *testing.T) {
	assert.NotEqual(t,
		coalesce.FetchKey("oci", "a", "b", ""),
		coalesce.FetchKey("oci", "ab", "", ""),
		"field boundaries must be unambiguous")
}

// TestFetch_ConcurrentCallers_RunFnOnce is the primitive-level statement of the
// stampede claim: N concurrent callers, ONE execution, everyone gets the value.
func TestFetch_ConcurrentCallers_RunFnOnce(t *testing.T) {
	c := coalesce.NewLocalCoalescer()
	var calls atomic.Int64
	release := make(chan struct{})

	const n = 20
	var wg sync.WaitGroup
	got := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], errs[i] = coalesce.Fetch(context.Background(), c, "k", func() (string, error) {
				calls.Add(1)
				<-release // hold the leader so all n callers pile up behind it
				return "value", nil
			})
		}(i)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	assert.Equal(t, int64(1), calls.Load(), "fn must execute exactly once for n concurrent callers")
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i])
		assert.Equal(t, "value", got[i], "every caller must receive the leader's result")
	}
}

// TestFetch_LeaderError_SharedWithFollowers_NotRefetched pins the failure
// semantics at the primitive level: one failed execution, N shared errors — not
// N executions against an already-struggling upstream.
func TestFetch_LeaderError_SharedWithFollowers_NotRefetched(t *testing.T) {
	c := coalesce.NewLocalCoalescer()
	var calls atomic.Int64
	release := make(chan struct{})
	sentinel := errors.New("upstream down")

	const n = 10
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = coalesce.Fetch(context.Background(), c, "k", func() (*int, error) {
				calls.Add(1)
				<-release
				return nil, sentinel
			})
		}(i)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	assert.Equal(t, int64(1), calls.Load(),
		"a FAILING leader must still collapse; followers must not each re-fetch")
	for i := 0; i < n; i++ {
		assert.ErrorIs(t, errs[i], sentinel, "every follower receives the leader's error")
	}
}

// TestFetch_ErrorIsNotCached — a transient failure must not poison the key. The
// NEXT request after a failure is a fresh leader, so a blip self-heals rather
// than being pinned for a TTL.
func TestFetch_ErrorIsNotCached(t *testing.T) {
	c := coalesce.NewLocalCoalescer()
	var calls atomic.Int64

	_, err := coalesce.Fetch(context.Background(), c, "k", func() (string, error) {
		calls.Add(1)
		return "", errors.New("transient")
	})
	require.Error(t, err)

	v, err := coalesce.Fetch(context.Background(), c, "k", func() (string, error) {
		calls.Add(1)
		return "recovered", nil
	})
	require.NoError(t, err)
	assert.Equal(t, "recovered", v)
	assert.Equal(t, int64(2), calls.Load(),
		"the second call must really re-execute; a cached error would pin the failure")
}

// TestFetch_FollowerCtxCancel_ReleasesFollowerOnly is the bounded-wait claim: a
// leader that hangs must not pin followers forever. The follower's own ctx is
// the bound, and it does not depend on the leader behaving.
func TestFetch_FollowerCtxCancel_ReleasesFollowerOnly(t *testing.T) {
	c := coalesce.NewLocalCoalescer()
	leaderRunning := make(chan struct{})
	release := make(chan struct{})
	defer close(release)

	go func() {
		_, _ = coalesce.Fetch(context.Background(), c, "k", func() (string, error) {
			close(leaderRunning)
			<-release // the leader hangs
			return "late", nil
		})
	}()
	<-leaderRunning

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := coalesce.Fetch(ctx, c, "k", func() (string, error) {
		t.Fatal("follower must not execute fn; it must wait on the leader")
		return "", nil
	})

	require.ErrorIs(t, err, context.DeadlineExceeded,
		"a follower must be released by its OWN deadline while the leader still hangs")
	assert.Less(t, time.Since(start), 2*time.Second,
		"the follower must not be pinned to the leader's lifetime")
}

// TestFetch_NilResultIsNotAnError — handlers return (nil, nil) for "upstream
// says this does not exist". That must survive the collapse as a nil result,
// not become an error.
func TestFetch_NilResultIsNotAnError(t *testing.T) {
	c := coalesce.NewLocalCoalescer()
	v, err := coalesce.Fetch(context.Background(), c, "k", func() (*int, error) {
		return nil, nil
	})
	require.NoError(t, err, "a nil result is a legitimate 'not found', not a failure")
	assert.Nil(t, v)
}
