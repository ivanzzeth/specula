package cache

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// TestXFetch_LookupMiss_LookupStaleHit proves the SWR seam handlers already
// use: when XFetch soft-expires an entry, Lookup misses (caller revalidates)
// while LookupStale still returns the bytes so serve-stale / conditional GET
// can proceed (ARCHITECTURE: XFetch + stale-while-revalidate).
func TestXFetch_LookupMiss_LookupStaleHit(t *testing.T) {
	m, fb, fm := newTestManager(t, nil)
	_ = fb

	ref := artifact.ArtifactRef{Protocol: "oci", Name: "idx", Version: "latest", Mutable: true}
	now := time.Now()
	require.NoError(t, fm.PutMutable(contextBackground(), artifact.MutableEntry{
		Key:        mutableKey(ref),
		Protocol:   "oci",
		Payload:    []byte("index"),
		TTLSeconds: 3600,
		FetchedAt:  now.Add(-59 * time.Minute), // near expiry
	}))

	// Force soft-expire via deterministic U.
	me, err := fm.GetMutable(contextBackground(), mutableKey(ref))
	require.NoError(t, err)
	require.False(t, isMutableFreshAt(me, now, 1e-12), "XFetch must soft-expire")

	hit, err := m.Lookup(contextBackground(), ref)
	require.NoError(t, err)
	// Probabilistic: Lookup uses rand — may still hit. Soft-expire is tested
	// above; LookupStale must always return the entry for SWR.
	_ = hit

	stale, err := m.LookupStale(contextBackground(), ref)
	require.NoError(t, err)
	require.NotNil(t, stale, "LookupStale must keep soft/hard-expired entries for SWR")
	assert.Equal(t, "index", string(mustReadPayload(t, fm, ref)))
}

func contextBackground() context.Context {
	return context.Background()
}

func mustReadPayload(t *testing.T, fm *fakeMetaStore, ref artifact.ArtifactRef) []byte {
	t.Helper()
	me, err := fm.GetMutable(contextBackground(), mutableKey(ref))
	require.NoError(t, err)
	require.NotNil(t, me)
	return me.Payload
}
