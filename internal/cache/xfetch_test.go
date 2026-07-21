package cache

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// TestLookup_SoftExpired_SWRHit proves XFetch soft-expiry returns the entry
// with SoftExpired=true (serve immediately) while hard expiry still misses.
func TestLookup_SoftExpired_SWRHit(t *testing.T) {
	m, _, fm := newTestManager(t, nil)
	ref := artifact.ArtifactRef{Protocol: "oci", Name: "idx", Version: "latest", Mutable: true}
	now := time.Now()

	old := xfetchUniform
	xfetchUniform = func() float64 { return 1e-12 }
	t.Cleanup(func() { xfetchUniform = old })

	require.NoError(t, fm.PutMutable(context.Background(), artifact.MutableEntry{
		Key:        mutableKey(ref),
		Protocol:   "oci",
		Payload:    []byte("index"),
		TTLSeconds: 3600,
		FetchedAt:  now.Add(-59 * time.Minute),
	}))

	entry, err := m.Lookup(context.Background(), ref)
	require.NoError(t, err)
	require.NotNil(t, entry, "soft-expired must SWR-serve via Lookup")
	assert.True(t, entry.SoftExpired)

	// Hard-expired: past absolute TTL.
	require.NoError(t, fm.PutMutable(context.Background(), artifact.MutableEntry{
		Key:        mutableKey(ref),
		Protocol:   "oci",
		Payload:    []byte("index"),
		TTLSeconds: 60,
		FetchedAt:  now.Add(-2 * time.Minute),
	}))
	entry, err = m.Lookup(context.Background(), ref)
	require.NoError(t, err)
	assert.Nil(t, entry, "hard-expired must miss")

	stale, err := m.LookupStale(context.Background(), ref)
	require.NoError(t, err)
	require.NotNil(t, stale)
}

func TestStartBackgroundRefresh_RunsOnce(t *testing.T) {
	var n atomic.Int32
	done := make(chan struct{}, 1)
	fn := func(ctx context.Context) error {
		if n.Add(1) == 1 {
			close(done)
		}
		time.Sleep(30 * time.Millisecond) // hold singleflight so the second call coalesces
		return nil
	}
	StartBackgroundRefresh("swr-test-key-"+t.Name(), fn)
	StartBackgroundRefresh("swr-test-key-"+t.Name(), fn)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh did not run")
	}
	time.Sleep(80 * time.Millisecond)
	assert.Equal(t, int32(1), n.Load())
}
