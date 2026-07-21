package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/verify"
)

func quarantineWithDigest(t *testing.T, content []byte) *artifact.Artifact {
	t.Helper()
	sum := sha256.Sum256(content)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	f, err := os.CreateTemp(t.TempDir(), "test-quar-*")
	require.NoError(t, err)
	_, err = f.Write(content)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return &artifact.Artifact{
		Path:   f.Name(),
		Digest: digest,
		Size:   int64(len(content)),
		Meta:   artifact.UpstreamMeta{Upstream: "test-mirror"},
	}
}

func storeNamed(t *testing.T, m *manager, name string, content []byte, createdAt time.Time) artifact.ArtifactRef {
	t.Helper()
	ref := artifact.ArtifactRef{Protocol: "oci", Name: name, Version: "v1"}
	art := quarantineWithDigest(t, content)
	m.verifyFn = passVerify(artifact.TierChecksum)
	_, err := m.Store(context.Background(), ref, art)
	require.NoError(t, err)

	// Backdate CreatedAt so eviction order is deterministic (Store sets Now).
	m.meta.(*fakeMetaStore).mu.Lock()
	e := m.meta.(*fakeMetaStore).entries[entryKey(ref)]
	e.CreatedAt = createdAt
	e.VerifiedAt = createdAt
	m.meta.(*fakeMetaStore).mu.Unlock()
	return ref
}

func TestEnforceCapacity_Unlimited_NoEviction(t *testing.T) {
	m, fb, fm := newTestManager(t, nil)
	// maxBytes default 0
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	storeNamed(t, m, "a", []byte("aaaaaaaaaa"), base)
	storeNamed(t, m, "b", []byte("bbbbbbbbbb"), base.Add(time.Second))

	require.Len(t, fm.entries, 2)
	n, err := fb.UsageBytes(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(20), n)
}

func TestEnforceCapacity_EvictsOldestUnpinned(t *testing.T) {
	m, fb, fm := newTestManager(t, nil)
	m.maxBytes = 25 // three 10-byte blobs → over; keep newest two (20) after evict oldest

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	old := storeNamed(t, m, "old", []byte("aaaaaaaaaa"), base)
	storeNamed(t, m, "mid", []byte("bbbbbbbbbb"), base.Add(time.Second))
	storeNamed(t, m, "new", []byte("cccccccccc"), base.Add(2*time.Second))

	// After third store, oldest should be gone.
	_, ok := fm.entries[entryKey(old)]
	assert.False(t, ok, "oldest entry must be evicted")
	assert.Len(t, fm.entries, 2)

	total, err := m.totalCachedBytes(context.Background())
	require.NoError(t, err)
	assert.LessOrEqual(t, total, m.maxBytes)

	n, err := fb.UsageBytes(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(20), n, "evicted blob bytes must be freed")
}

func TestEnforceCapacity_SkipsPinned(t *testing.T) {
	m, _, fm := newTestManager(t, nil)
	m.maxBytes = 15

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	pinnedRef := storeNamed(t, m, "pinned", []byte("aaaaaaaaaa"), base)
	require.NoError(t, fm.SetPinned(context.Background(), pinnedRef, true))

	storeNamed(t, m, "mid", []byte("bbbbbbbbbb"), base.Add(time.Second))
	storeNamed(t, m, "new", []byte("cccccccccc"), base.Add(2*time.Second))

	_, ok := fm.entries[entryKey(pinnedRef)]
	assert.True(t, ok, "pinned entry must survive eviction")
	// mid should be gone (oldest unpinned); new kept; pinned kept → may be over max
	assert.NotContains(t, fm.entries, entryKey(artifact.ArtifactRef{Protocol: "oci", Name: "mid", Version: "v1"}))
	assert.Contains(t, fm.entries, entryKey(artifact.ArtifactRef{Protocol: "oci", Name: "new", Version: "v1"}))
}

func TestEnforceCapacity_ProtectsJustStored(t *testing.T) {
	m, _, fm := newTestManager(t, nil)
	m.maxBytes = 10 // only room for one 10-byte entry

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	storeNamed(t, m, "old", []byte("aaaaaaaaaa"), base)

	ref := artifact.ArtifactRef{Protocol: "oci", Name: "fresh", Version: "v1"}
	art := quarantineWithDigest(t, []byte("bbbbbbbbbb"))
	m.verifyFn = passVerify(artifact.TierChecksum)
	_, err := m.Store(context.Background(), ref, art)
	require.NoError(t, err)

	assert.Contains(t, fm.entries, entryKey(ref), "just-stored entry must not be evicted")
	assert.NotContains(t, fm.entries, entryKey(artifact.ArtifactRef{Protocol: "oci", Name: "old", Version: "v1"}))
}

func TestEnforceCapacity_EvictHook(t *testing.T) {
	m, _, _ := newTestManager(t, nil)
	m.maxBytes = 15
	var hooked []string
	m.onEvict = func(_ context.Context, protocol string, size int64) {
		hooked = append(hooked, fmt.Sprintf("%s:%d", protocol, size))
	}

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	storeNamed(t, m, "a", []byte("aaaaaaaaaa"), base)
	storeNamed(t, m, "b", []byte("bbbbbbbbbb"), base.Add(time.Second))
	storeNamed(t, m, "c", []byte("cccccccccc"), base.Add(2*time.Second))

	require.NotEmpty(t, hooked)
	assert.Equal(t, "oci:10", hooked[0])
}

func TestWithMaxBytes_Option(t *testing.T) {
	fb := newFakeBlob(nil)
	fm := newFakeMeta(nil)
	cm := New(fb, fm, verify.NewChain(), WithMaxBytes(100))
	require.Equal(t, int64(100), cm.(*manager).maxBytes)

	cm2 := New(fb, fm, verify.NewChain(), WithMaxBytes(-5))
	require.Equal(t, int64(0), cm2.(*manager).maxBytes)
}
