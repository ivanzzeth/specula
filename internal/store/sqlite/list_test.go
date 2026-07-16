package sqlite_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/store/meta"
	"github.com/ivanzzeth/specula/internal/store/sqlite"
)

// seedEntries writes a small, deliberately heterogeneous fixture: two protocols,
// three tiers, two upstreams, ascending sizes and creation times. Every
// ListEntries test below slices this same set, so a filter that silently matched
// everything would be visible as a wrong Total.
func seedEntries(t *testing.T, s *sqlite.SQLiteStore) {
	t.Helper()
	ctx := context.Background()
	base := time.Unix(1_700_000_000, 0).UTC()

	entries := []artifact.CacheEntry{
		{Ref: cacheRef("oci", "library/nginx", "1.25"), Digest: "sha256:a", Size: 100,
			Protocol: "oci", Tier: artifact.TierSigned, Upstream: "docker.io", CreatedAt: base},
		{Ref: cacheRef("oci", "library/redis", "7"), Digest: "sha256:b", Size: 300,
			Protocol: "oci", Tier: artifact.TierTofu, Upstream: "daocloud", CreatedAt: base.Add(time.Hour)},
		{Ref: cacheRef("oci", "bitnami/kafka", "3"), Digest: "sha256:c", Size: 200,
			Protocol: "oci", Tier: artifact.TierChecksum, Upstream: "docker.io", CreatedAt: base.Add(2 * time.Hour)},
		{Ref: cacheRef("pypi", "requests", "2.31.0"), Digest: "sha256:d", Size: 50,
			Protocol: "pypi", Tier: artifact.TierTofu, Upstream: "pypi.org", CreatedAt: base.Add(3 * time.Hour)},
	}
	for _, e := range entries {
		require.NoError(t, s.Put(ctx, e))
	}
}

// names extracts the ordered entry names from a page for concise assertions.
func names(p meta.EntryPage) []string {
	out := make([]string, 0, len(p.Entries))
	for _, e := range p.Entries {
		out = append(out, e.Ref.Name)
	}
	return out
}

func TestListEntries_ScopesToProtocol(t *testing.T) {
	s := newTestStore(t)
	seedEntries(t, s)

	page, err := s.ListEntries(context.Background(), "oci", meta.EntryFilter{}, meta.Page{})
	require.NoError(t, err)

	assert.Equal(t, int64(3), page.Total, "Total must count only the oci rows")
	assert.Len(t, page.Entries, 3)
	for _, e := range page.Entries {
		assert.Equal(t, "oci", e.Ref.Protocol)
	}
}

func TestListEntries_EmptyProtocolReturnsAllProtocols(t *testing.T) {
	s := newTestStore(t)
	seedEntries(t, s)

	page, err := s.ListEntries(context.Background(), "", meta.EntryFilter{}, meta.Page{})
	require.NoError(t, err)
	assert.Equal(t, int64(4), page.Total)
}

func TestListEntries_HydratesEntryFields(t *testing.T) {
	s := newTestStore(t)
	seedEntries(t, s)

	page, err := s.ListEntries(context.Background(), "pypi", meta.EntryFilter{}, meta.Page{})
	require.NoError(t, err)
	require.Len(t, page.Entries, 1)

	e := page.Entries[0]
	assert.Equal(t, "requests", e.Ref.Name)
	assert.Equal(t, "2.31.0", e.Ref.Version)
	assert.Equal(t, "sha256:d", e.Digest)
	assert.Equal(t, int64(50), e.Size)
	assert.Equal(t, artifact.TierTofu, e.Tier)
	assert.Equal(t, "pypi.org", e.Upstream)
	assert.Equal(t, "pypi", e.Protocol)
	assert.False(t, e.Pinned, "entries default to unpinned")
	assert.Equal(t, meta.EncodeEntryID(e.Ref), e.ID, "ID must round-trip to the row's key")
}

func TestListEntries_FilterByNameSubstring(t *testing.T) {
	s := newTestStore(t)
	seedEntries(t, s)

	page, err := s.ListEntries(context.Background(), "oci",
		meta.EntryFilter{NameContains: "library/"}, meta.Page{})
	require.NoError(t, err)

	assert.Equal(t, int64(2), page.Total)
	assert.ElementsMatch(t, []string{"library/nginx", "library/redis"}, names(page))
}

// A user-typed "_" must be a literal, not a single-character wildcard.
func TestListEntries_FilterNameEscapesLikeWildcards(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	require.NoError(t, s.Put(ctx, artifact.CacheEntry{
		Ref: cacheRef("npm", "a_b", "1"), Protocol: "npm", Size: 1,
	}))
	require.NoError(t, s.Put(ctx, artifact.CacheEntry{
		Ref: cacheRef("npm", "axb", "1"), Protocol: "npm", Size: 1,
	}))

	page, err := s.ListEntries(ctx, "npm", meta.EntryFilter{NameContains: "a_b"}, meta.Page{})
	require.NoError(t, err)
	assert.Equal(t, int64(1), page.Total, "'_' must not act as a wildcard")
	assert.Equal(t, []string{"a_b"}, names(page))
}

func TestListEntries_FilterByTier(t *testing.T) {
	s := newTestStore(t)
	seedEntries(t, s)

	// TierChecksum is the zero value — the pointer filter must still apply it
	// rather than treating it as "unset".
	checksum := artifact.TierChecksum
	page, err := s.ListEntries(context.Background(), "oci",
		meta.EntryFilter{Tier: &checksum}, meta.Page{})
	require.NoError(t, err)

	assert.Equal(t, int64(1), page.Total)
	assert.Equal(t, []string{"bitnami/kafka"}, names(page))
}

func TestListEntries_FilterByUpstream(t *testing.T) {
	s := newTestStore(t)
	seedEntries(t, s)

	page, err := s.ListEntries(context.Background(), "oci",
		meta.EntryFilter{Upstream: "docker.io"}, meta.Page{})
	require.NoError(t, err)
	assert.Equal(t, int64(2), page.Total)
}

func TestListEntries_SortBySizeDesc(t *testing.T) {
	s := newTestStore(t)
	seedEntries(t, s)

	page, err := s.ListEntries(context.Background(), "oci", meta.EntryFilter{},
		meta.Page{Sort: meta.SortSize, Desc: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"library/redis", "bitnami/kafka", "library/nginx"}, names(page))
}

func TestListEntries_SortByNameAsc(t *testing.T) {
	s := newTestStore(t)
	seedEntries(t, s)

	page, err := s.ListEntries(context.Background(), "oci", meta.EntryFilter{},
		meta.Page{Sort: meta.SortName})
	require.NoError(t, err)
	assert.Equal(t, []string{"bitnami/kafka", "library/nginx", "library/redis"}, names(page))
}

// An unknown sort field must fall back to created_at rather than reaching the
// SQL text or producing unordered output.
func TestListEntries_UnknownSortFallsBackToCreatedAt(t *testing.T) {
	s := newTestStore(t)
	seedEntries(t, s)

	page, err := s.ListEntries(context.Background(), "oci", meta.EntryFilter{},
		meta.Page{Sort: meta.SortField("size; DROP TABLE cache_entries")})
	require.NoError(t, err)
	assert.Equal(t, []string{"library/nginx", "library/redis", "bitnami/kafka"}, names(page))
}

func TestListEntries_PaginationWindowAndTotal(t *testing.T) {
	s := newTestStore(t)
	seedEntries(t, s)
	ctx := context.Background()

	first, err := s.ListEntries(ctx, "oci", meta.EntryFilter{},
		meta.Page{Limit: 2, Sort: meta.SortName})
	require.NoError(t, err)
	assert.Equal(t, int64(3), first.Total, "Total ignores the page window")
	assert.Equal(t, []string{"bitnami/kafka", "library/nginx"}, names(first))

	second, err := s.ListEntries(ctx, "oci", meta.EntryFilter{},
		meta.Page{Limit: 2, Offset: 2, Sort: meta.SortName})
	require.NoError(t, err)
	assert.Equal(t, int64(3), second.Total)
	assert.Equal(t, []string{"library/redis"}, names(second))
}

func TestListEntries_ClampsPageBounds(t *testing.T) {
	s := newTestStore(t)
	seedEntries(t, s)
	ctx := context.Background()

	page, err := s.ListEntries(ctx, "oci", meta.EntryFilter{}, meta.Page{Limit: 0, Offset: -5})
	require.NoError(t, err)
	assert.Equal(t, meta.DefaultLimit, page.Limit, "limit<=0 clamps to DefaultLimit")
	assert.Equal(t, 0, page.Offset, "negative offset clamps to 0")

	page, err = s.ListEntries(ctx, "oci", meta.EntryFilter{}, meta.Page{Limit: 10_000})
	require.NoError(t, err)
	assert.Equal(t, meta.MaxLimit, page.Limit, "limit must be capped at MaxLimit")
}

func TestListEntries_OffsetBeyondEndReturnsEmptyPageWithTotal(t *testing.T) {
	s := newTestStore(t)
	seedEntries(t, s)

	page, err := s.ListEntries(context.Background(), "oci", meta.EntryFilter{},
		meta.Page{Offset: 500})
	require.NoError(t, err)
	assert.Empty(t, page.Entries)
	assert.Equal(t, int64(3), page.Total, "pager still needs the true count")
}

func TestSetPinned_RoundTripsAndFilters(t *testing.T) {
	s := newTestStore(t)
	seedEntries(t, s)
	ctx := context.Background()
	ref := cacheRef("oci", "library/nginx", "1.25")

	require.NoError(t, s.SetPinned(ctx, ref, true))

	pinned := true
	page, err := s.ListEntries(ctx, "oci", meta.EntryFilter{Pinned: &pinned}, meta.Page{})
	require.NoError(t, err)
	require.Equal(t, int64(1), page.Total)
	assert.Equal(t, "library/nginx", page.Entries[0].Ref.Name)
	assert.True(t, page.Entries[0].Pinned)

	// The inverse filter must exclude it — proving false is a real predicate
	// and not an "unset" no-op.
	unpinned := false
	page, err = s.ListEntries(ctx, "oci", meta.EntryFilter{Pinned: &unpinned}, meta.Page{})
	require.NoError(t, err)
	assert.Equal(t, int64(2), page.Total)

	require.NoError(t, s.SetPinned(ctx, ref, false))
	page, err = s.ListEntries(ctx, "oci", meta.EntryFilter{Pinned: &pinned}, meta.Page{})
	require.NoError(t, err)
	assert.Equal(t, int64(0), page.Total, "unpinning must clear the flag")
}

// Put must not clobber an operator's pin: the upsert path rewrites an entry's
// metadata on every re-fetch, and a pin that silently evaporated on refresh
// would let GC delete data an operator explicitly protected.
func TestSetPinned_SurvivesEntryUpsert(t *testing.T) {
	s := newTestStore(t)
	seedEntries(t, s)
	ctx := context.Background()
	ref := cacheRef("oci", "library/nginx", "1.25")

	require.NoError(t, s.SetPinned(ctx, ref, true))
	require.NoError(t, s.Put(ctx, artifact.CacheEntry{
		Ref: ref, Digest: "sha256:new", Size: 999, Protocol: "oci", Upstream: "docker.io",
	}))

	pinned := true
	page, err := s.ListEntries(ctx, "oci", meta.EntryFilter{Pinned: &pinned}, meta.Page{})
	require.NoError(t, err)
	assert.Equal(t, int64(1), page.Total, "re-Put of a pinned entry must preserve the pin")
}

func TestSetPinned_MissingEntryIsNoOp(t *testing.T) {
	s := newTestStore(t)
	err := s.SetPinned(context.Background(), cacheRef("oci", "ghost", "1"), true)
	assert.NoError(t, err, "pinning an absent row must not error")
}

func TestEncodeDecodeEntryID_RoundTrip(t *testing.T) {
	// Names with slashes are the norm (OCI, Go modules, npm scopes) and are
	// exactly why the ID is opaque rather than a path segment.
	ref := artifact.ArtifactRef{Protocol: "oci", Name: "library/nginx", Version: "1.25"}

	decoded, err := meta.DecodeEntryID(meta.EncodeEntryID(ref))
	require.NoError(t, err)
	assert.Equal(t, ref, decoded)
}

func TestDecodeEntryID_RejectsMalformed(t *testing.T) {
	for name, id := range map[string]string{
		"not base64":     "!!!!",
		"too few parts":  "b2NpAG5naW54", // "oci\x00nginx" — missing version
		"empty protocol": meta.EncodeEntryID(artifact.ArtifactRef{Name: "n", Version: "v"}),
		"empty name":     meta.EncodeEntryID(artifact.ArtifactRef{Protocol: "oci", Version: "v"}),
		"empty string":   "",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := meta.DecodeEntryID(id)
			assert.ErrorIs(t, err, meta.ErrBadEntryID)
		})
	}
}
