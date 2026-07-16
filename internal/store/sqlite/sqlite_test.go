package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/store/sqlite"
)

// newTestStore creates a SQLiteStore backed by a temp file that is cleaned up
// automatically after the test.
func newTestStore(t *testing.T) *sqlite.SQLiteStore {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "specula_test.db")
	s, err := sqlite.NewSQLiteStore(dsn)
	require.NoError(t, err, "NewSQLiteStore must succeed")
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// cacheRef builds a test ArtifactRef.
func cacheRef(proto, name, ver string) artifact.ArtifactRef {
	return artifact.ArtifactRef{
		Protocol: proto,
		Name:     name,
		Version:  ver,
		Digest:   "sha256:abc123",
		Upstream: "upstream1",
		Mutable:  false,
	}
}

// ---- cache_entries (immutable tier) ----------------------------------------

func TestCacheEntry_GetNotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	got, err := s.Get(ctx, cacheRef("oci", "nginx", "1.0"))
	require.NoError(t, err)
	assert.Nil(t, got, "absent entry must return nil")
}

func TestCacheEntry_PutGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	ref := cacheRef("oci", "nginx", "1.25")
	now := time.Now().UTC().Truncate(time.Second)
	entry := artifact.CacheEntry{
		Ref:        ref,
		Digest:     "sha256:deadbeef",
		Size:       1024,
		Protocol:   "oci",
		Tier:       artifact.TierTofu,
		Upstream:   "registry-cn.example.com",
		ETag:       `"abc"`,
		VerifiedAt: now,
		CreatedAt:  now,
	}

	require.NoError(t, s.Put(ctx, entry))

	got, err := s.Get(ctx, ref)
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, entry.Digest, got.Digest)
	assert.Equal(t, entry.Size, got.Size)
	assert.Equal(t, entry.Tier, got.Tier)
	assert.Equal(t, entry.Upstream, got.Upstream)
	assert.Equal(t, entry.ETag, got.ETag)
	assert.Equal(t, entry.Protocol, got.Protocol)
	assert.Equal(t, now.Unix(), got.VerifiedAt.Unix())
	assert.Equal(t, now.Unix(), got.CreatedAt.Unix())
}

func TestCacheEntry_PutIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	ref := cacheRef("oci", "alpine", "3.19")
	// Use an explicit CreatedAt so we can verify ON CONFLICT preserves it.
	createdAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	base := artifact.CacheEntry{
		Ref:       ref,
		Digest:    "sha256:aaa",
		Size:      512,
		Protocol:  "oci",
		Tier:      artifact.TierChecksum,
		CreatedAt: createdAt,
	}
	require.NoError(t, s.Put(ctx, base))

	// Second put with updated digest + tier; created_at must stay from first write.
	updated := base
	updated.Digest = "sha256:bbb"
	updated.Tier = artifact.TierSigned
	updated.CreatedAt = createdAt.Add(time.Hour) // should be ignored by upsert
	require.NoError(t, s.Put(ctx, updated))

	got, err := s.Get(ctx, ref)
	require.NoError(t, err)
	require.NotNil(t, got)

	// Upsert updated the mutable fields.
	assert.Equal(t, "sha256:bbb", got.Digest)
	assert.Equal(t, artifact.TierSigned, got.Tier)
	// created_at is preserved from the first write (ON CONFLICT leaves it alone).
	assert.Equal(t, createdAt.Unix(), got.CreatedAt.Unix())
}

func TestCacheEntry_Delete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	ref := cacheRef("pypi", "requests", "2.31.0")
	entry := artifact.CacheEntry{
		Ref:      ref,
		Digest:   "sha256:111",
		Size:     200,
		Protocol: "pypi",
		Tier:     artifact.TierChecksum,
	}
	require.NoError(t, s.Put(ctx, entry))

	// Sanity: present before delete.
	got, err := s.Get(ctx, ref)
	require.NoError(t, err)
	require.NotNil(t, got)

	require.NoError(t, s.Delete(ctx, ref))

	// Absent after delete.
	got, err = s.Get(ctx, ref)
	require.NoError(t, err)
	assert.Nil(t, got, "entry must be absent after Delete")
}

func TestCacheEntry_DeleteNonExistent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// Deleting a non-existent row must be a no-op.
	err := s.Delete(ctx, cacheRef("npm", "lodash", "4.17.21"))
	assert.NoError(t, err)
}

// ---- mutable_entries -------------------------------------------------------

func TestMutableEntry_GetNotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	got, err := s.GetMutable(ctx, "oci:nginx:latest")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestMutableEntry_PutGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	key := "oci:nginx:latest"
	now := time.Now().UTC().Truncate(time.Second)
	entry := artifact.MutableEntry{
		Key:          key,
		Protocol:     "oci",
		Digest:       "sha256:cafebabe",
		Payload:      []byte(`{"tag":"latest"}`),
		ETag:         `"etag123"`,
		LastModified: "Wed, 01 Jan 2025 00:00:00 GMT",
		TTLSeconds:   300,
		Upstream:     "registry.cn",
		FetchedAt:    now,
	}

	require.NoError(t, s.PutMutable(ctx, entry))

	got, err := s.GetMutable(ctx, key)
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, entry.Key, got.Key)
	assert.Equal(t, entry.Protocol, got.Protocol)
	assert.Equal(t, entry.Digest, got.Digest)
	assert.Equal(t, entry.Payload, got.Payload)
	assert.Equal(t, entry.ETag, got.ETag)
	assert.Equal(t, entry.LastModified, got.LastModified)
	assert.Equal(t, entry.TTLSeconds, got.TTLSeconds)
	assert.Equal(t, entry.Upstream, got.Upstream)
	assert.Equal(t, now.Unix(), got.FetchedAt.Unix())
}

func TestMutableEntry_PutUpsert(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	key := "npm:lodash:latest-tag"
	first := artifact.MutableEntry{
		Key:        key,
		Protocol:   "npm",
		Digest:     "sha256:v1",
		TTLSeconds: 120,
	}
	require.NoError(t, s.PutMutable(ctx, first))

	second := artifact.MutableEntry{
		Key:        key,
		Protocol:   "npm",
		Digest:     "sha256:v2",
		TTLSeconds: 60,
	}
	require.NoError(t, s.PutMutable(ctx, second))

	got, err := s.GetMutable(ctx, key)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "sha256:v2", got.Digest)
	assert.Equal(t, int64(60), got.TTLSeconds)
}

func TestMutableEntry_TTLSentinels(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// TTL sentinels: -1 = never revalidate, 0 = always revalidate (config pkg).
	tests := []struct {
		name string
		key  string
		ttl  int64
	}{
		{"never revalidate", "k1", -1},
		{"always revalidate", "k2", 0},
		{"positive ttl", "k3", 600},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entry := artifact.MutableEntry{
				Key:        tc.key,
				Protocol:   "gomod",
				Digest:     "sha256:x",
				TTLSeconds: tc.ttl,
			}
			require.NoError(t, s.PutMutable(ctx, entry))
			got, err := s.GetMutable(ctx, tc.key)
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, tc.ttl, got.TTLSeconds)
		})
	}
}

// ---- CacheSizeByProtocol ---------------------------------------------------

func TestCacheSizeByProtocol_Empty(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	result, err := s.CacheSizeByProtocol(ctx)
	require.NoError(t, err)
	assert.Empty(t, result)
}

func TestCacheSizeByProtocol_Aggregation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)

	entries := []artifact.CacheEntry{
		{Ref: cacheRef("oci", "nginx", "1.0"), Digest: "sha256:a", Size: 100, Protocol: "oci", Tier: artifact.TierChecksum, CreatedAt: now},
		{Ref: cacheRef("oci", "alpine", "3.0"), Digest: "sha256:b", Size: 200, Protocol: "oci", Tier: artifact.TierChecksum, CreatedAt: now.Add(time.Second)},
		{Ref: cacheRef("pypi", "requests", "2.0"), Digest: "sha256:c", Size: 50, Protocol: "pypi", Tier: artifact.TierChecksum, CreatedAt: now},
	}
	for _, e := range entries {
		require.NoError(t, s.Put(ctx, e))
	}

	result, err := s.CacheSizeByProtocol(ctx)
	require.NoError(t, err)

	oci, ok := result["oci"]
	require.True(t, ok, "oci protocol must appear")
	assert.Equal(t, int64(300), oci.Bytes)
	assert.Equal(t, int64(2), oci.Objects)
	assert.Equal(t, now.Unix(), oci.Oldest.Unix())
	assert.Equal(t, now.Add(time.Second).Unix(), oci.Newest.Unix())

	pypi, ok := result["pypi"]
	require.True(t, ok, "pypi protocol must appear")
	assert.Equal(t, int64(50), pypi.Bytes)
	assert.Equal(t, int64(1), pypi.Objects)
}

func TestCacheSizeByProtocol_AfterDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	ref := cacheRef("npm", "react", "18.0.0")
	entry := artifact.CacheEntry{
		Ref:      ref,
		Digest:   "sha256:react",
		Size:     999,
		Protocol: "npm",
		Tier:     artifact.TierChecksum,
	}
	require.NoError(t, s.Put(ctx, entry))

	before, err := s.CacheSizeByProtocol(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(999), before["npm"].Bytes)

	require.NoError(t, s.Delete(ctx, ref))

	after, err := s.CacheSizeByProtocol(ctx)
	require.NoError(t, err)
	_, present := after["npm"]
	assert.False(t, present, "npm must vanish from stats after deleting last entry")
}

// ---- multi-protocol isolation ----------------------------------------------

func TestMultiProtocol_Isolation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Same name+version but different protocols must not collide.
	protocols := []string{"oci", "helm", "pypi"}
	for _, p := range protocols {
		e := artifact.CacheEntry{
			Ref:      artifact.ArtifactRef{Protocol: p, Name: "foo", Version: "1.0"},
			Digest:   "sha256:" + p,
			Size:     10,
			Protocol: p,
			Tier:     artifact.TierChecksum,
		}
		require.NoError(t, s.Put(ctx, e))
	}

	for _, p := range protocols {
		ref := artifact.ArtifactRef{Protocol: p, Name: "foo", Version: "1.0"}
		got, err := s.Get(ctx, ref)
		require.NoError(t, err)
		require.NotNil(t, got, "entry for protocol %s must exist", p)
		assert.Equal(t, "sha256:"+p, got.Digest)
	}
}
