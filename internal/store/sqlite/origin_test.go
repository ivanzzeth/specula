package sqlite_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/store/meta"
	"github.com/ivanzzeth/specula/internal/store/sqlite"
)

func TestHostedOrigin_NeverListedForEviction(t *testing.T) {
	s, err := sqlite.NewSQLiteStore(t.TempDir() + "/origin.db")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	hosted := artifact.CacheEntry{
		Ref:      artifact.ArtifactRef{Protocol: "oci", Name: "acme/app", Version: "sha256:h"},
		Digest:   "sha256:h",
		Size:     1000,
		Protocol: "oci",
		Origin:   artifact.OriginHosted,
	}
	cached := artifact.CacheEntry{
		Ref:      artifact.ArtifactRef{Protocol: "oci", Name: "library/nginx", Version: "sha256:c"},
		Digest:   "sha256:c",
		Size:     500,
		Protocol: "oci",
		Origin:   artifact.OriginCached,
	}
	require.NoError(t, s.Put(ctx, hosted))
	require.NoError(t, s.Put(ctx, cached))

	got, err := s.Get(ctx, hosted.Ref)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, artifact.OriginHosted, got.Origin)

	unpinned := false
	page, err := s.ListEntries(ctx, "oci", meta.EntryFilter{
		Pinned: &unpinned,
		Origin: artifact.OriginCached,
	}, meta.Page{Limit: 50})
	require.NoError(t, err)
	require.Len(t, page.Entries, 1)
	assert.Equal(t, "library/nginx", page.Entries[0].Ref.Name)

	byOrigin, err := s.CacheSizeByOrigin(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1000), byOrigin[artifact.OriginHosted].Bytes)
	assert.Equal(t, int64(500), byOrigin[artifact.OriginCached].Bytes)

	// Cached Put must not demote hosted.
	hosted.Origin = artifact.OriginCached
	hosted.Size = 999
	require.NoError(t, s.Put(ctx, hosted))
	got, err = s.Get(ctx, hosted.Ref)
	require.NoError(t, err)
	assert.Equal(t, artifact.OriginHosted, got.Origin)
}
