package admin

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/auth"
	"github.com/ivanzzeth/specula/internal/store/meta"
)

// newCacheHarness builds an admin server over a seeded in-memory metadata store
// and returns both, so tests can assert against the store's real post-state.
func newCacheHarness(t *testing.T) (*harness, *fakeMetaStore) {
	t.Helper()
	store := newFakeUserStore()
	verifier := auth.NewHS256Verifier([]byte("test-secret-32-bytes-minimum!!!"))
	hasher := &fakeHasher{}
	metaStore := &fakeMetaStore{}

	srv := New(Deps{
		Stats:  newFakeStatsCollector(),
		Meta:   metaStore,
		Users:  store,
		Auth:   auth.NewService(store, hasher, verifier, false, nil),
		Tokens: verifier,
		Config: testConfig(),
		Blobs:  &fakeBlobReporter{usedBytes: 999},
	})
	srv.hasher = hasher

	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return &harness{srv: srv, mux: mux, store: store, verifier: verifier, hasher: hasher}, metaStore
}

// entry builds a seed entry.
func entry(proto, name, ver string, size int64, tier artifact.Tier, up string, created time.Time) meta.Entry {
	return meta.Entry{
		CacheEntry: artifact.CacheEntry{
			Ref:        artifact.ArtifactRef{Protocol: proto, Name: name, Version: ver},
			Digest:     "sha256:" + name,
			Size:       size,
			Protocol:   proto,
			Tier:       tier,
			Upstream:   up,
			VerifiedAt: created,
			CreatedAt:  created,
		},
	}
}

// seedCache loads the standard fixture: three oci entries across three tiers and
// two upstreams, plus one pypi entry to prove protocol scoping.
func seedCache(m *fakeMetaStore) {
	base := time.Unix(1_700_000_000, 0).UTC()
	m.seed(
		entry("oci", "library/nginx", "1.25", 100, artifact.TierSigned, "docker.io", base),
		entry("oci", "library/redis", "7", 300, artifact.TierTofu, "daocloud", base.Add(time.Hour)),
		entry("oci", "bitnami/kafka", "3", 200, artifact.TierChecksum, "docker.io", base.Add(2*time.Hour)),
		entry("pypi", "requests", "2.31.0", 50, artifact.TierTofu, "pypi.org", base.Add(3*time.Hour)),
	)
}

// entryNames extracts the ordered names from a cache response.
func entryNames(resp CacheEntriesResponse) []string {
	out := make([]string, 0, len(resp.Entries))
	for _, e := range resp.Entries {
		out = append(out, e.Name)
	}
	return out
}

func TestListCache_ScopesToProtocolAndProjectsFields(t *testing.T) {
	h, m := newCacheHarness(t)
	seedCache(m)
	_, tok := h.mustCreateAdmin(t)

	rr := h.do("GET", "/api/v1/admin/cache/pypi", tok, nil)
	require.Equal(t, http.StatusOK, rr.Code)

	var resp CacheEntriesResponse
	decodeJSON(t, rr, &resp)
	require.Len(t, resp.Entries, 1)
	assert.Equal(t, int64(1), resp.Total)

	e := resp.Entries[0]
	assert.Equal(t, "pypi", e.Protocol)
	assert.Equal(t, "requests", e.Name)
	assert.Equal(t, "2.31.0", e.Version)
	assert.Equal(t, int64(50), e.Size)
	// The tier must be the human name the UI colours by, not a raw enum ordinal.
	assert.Equal(t, "tofu", e.Tier)
	assert.Equal(t, "pypi.org", e.Upstream)
	assert.False(t, e.Pinned)
	assert.NotEmpty(t, e.ID)
	assert.NotZero(t, e.FirstCachedUnix)
}

// Every tier must round-trip as its documented name; the UI colours by these.
func TestListCache_TierNames(t *testing.T) {
	h, m := newCacheHarness(t)
	seedCache(m)
	_, tok := h.mustCreateAdmin(t)

	var resp CacheEntriesResponse
	decodeJSON(t, h.do("GET", "/api/v1/admin/cache/oci?sort=name&order=asc", tok, nil), &resp)

	got := map[string]string{}
	for _, e := range resp.Entries {
		got[e.Name] = e.Tier
	}
	assert.Equal(t, map[string]string{
		"bitnami/kafka": "checksum",
		"library/nginx": "signed",
		"library/redis": "tofu",
	}, got)
}

func TestListCache_DefaultsToNewestFirst(t *testing.T) {
	h, m := newCacheHarness(t)
	seedCache(m)
	_, tok := h.mustCreateAdmin(t)

	var resp CacheEntriesResponse
	decodeJSON(t, h.do("GET", "/api/v1/admin/cache/oci", tok, nil), &resp)
	assert.Equal(t, []string{"bitnami/kafka", "library/redis", "library/nginx"}, entryNames(resp))
}

func TestListCache_Filters(t *testing.T) {
	h, m := newCacheHarness(t)
	seedCache(m)
	_, tok := h.mustCreateAdmin(t)

	for name, tc := range map[string]struct {
		query string
		want  []string
	}{
		"by name substring": {"?name=library/&sort=name&order=asc",
			[]string{"library/nginx", "library/redis"}},
		"by tier":     {"?tier=signed", []string{"library/nginx"}},
		"by upstream": {"?upstream=daocloud", []string{"library/redis"}},
		// checksum is the zero-value tier: it must still be a real filter.
		"by zero-value tier": {"?tier=checksum", []string{"bitnami/kafka"}},
	} {
		t.Run(name, func(t *testing.T) {
			var resp CacheEntriesResponse
			decodeJSON(t, h.do("GET", "/api/v1/admin/cache/oci"+tc.query, tok, nil), &resp)
			assert.Equal(t, tc.want, entryNames(resp))
			assert.Equal(t, int64(len(tc.want)), resp.Total)
		})
	}
}

func TestListCache_SortAndPagination(t *testing.T) {
	h, m := newCacheHarness(t)
	seedCache(m)
	_, tok := h.mustCreateAdmin(t)

	var resp CacheEntriesResponse
	decodeJSON(t, h.do("GET", "/api/v1/admin/cache/oci?sort=size&order=desc", tok, nil), &resp)
	assert.Equal(t, []string{"library/redis", "bitnami/kafka", "library/nginx"}, entryNames(resp))

	decodeJSON(t, h.do("GET", "/api/v1/admin/cache/oci?sort=name&order=asc&limit=2", tok, nil), &resp)
	assert.Equal(t, []string{"bitnami/kafka", "library/nginx"}, entryNames(resp))
	assert.Equal(t, int64(3), resp.Total, "Total must ignore the page window")
	assert.Equal(t, 2, resp.Limit)

	decodeJSON(t, h.do("GET", "/api/v1/admin/cache/oci?sort=name&order=asc&limit=2&offset=2", tok, nil), &resp)
	assert.Equal(t, []string{"library/redis"}, entryNames(resp))
	assert.Equal(t, 2, resp.Offset)
}

// A bad filter must 400. Silently ignoring it would render rows that contradict
// the filter chips the operator is looking at.
func TestListCache_RejectsBadQueryParams(t *testing.T) {
	h, m := newCacheHarness(t)
	seedCache(m)
	_, tok := h.mustCreateAdmin(t)

	for name, q := range map[string]string{
		"bad tier":   "?tier=platinum",
		"bad sort":   "?sort=whatever",
		"bad order":  "?order=sideways",
		"bad pinned": "?pinned=maybe",
		"bad limit":  "?limit=abc",
		"bad offset": "?offset=xyz",
	} {
		t.Run(name, func(t *testing.T) {
			rr := h.do("GET", "/api/v1/admin/cache/oci"+q, tok, nil)
			assert.Equal(t, http.StatusBadRequest, rr.Code)
		})
	}
}

// An unknown protocol must 404, not render an empty (and therefore misleading)
// "nothing is cached" page.
func TestListCache_UnknownProtocolIs404(t *testing.T) {
	h, m := newCacheHarness(t)
	seedCache(m)
	_, tok := h.mustCreateAdmin(t)

	rr := h.do("GET", "/api/v1/admin/cache/cobol", tok, nil)
	assert.Equal(t, http.StatusNotFound, rr.Code)
}

func TestListCache_StoreErrorIs500(t *testing.T) {
	h, m := newCacheHarness(t)
	m.listErr = errors.New("db exploded")
	_, tok := h.mustCreateAdmin(t)

	rr := h.do("GET", "/api/v1/admin/cache/oci", tok, nil)
	assert.Equal(t, http.StatusInternalServerError, rr.Code)
}

func TestDeleteCacheEntry_EvictsTheRow(t *testing.T) {
	h, m := newCacheHarness(t)
	seedCache(m)
	_, tok := h.mustCreateAdmin(t)

	id := meta.EncodeEntryID(artifact.ArtifactRef{
		Protocol: "oci", Name: "library/nginx", Version: "1.25",
	})
	rr := h.do("DELETE", "/api/v1/admin/cache/oci/"+id, tok, nil)
	require.Equal(t, http.StatusNoContent, rr.Code)

	var resp CacheEntriesResponse
	decodeJSON(t, h.do("GET", "/api/v1/admin/cache/oci", tok, nil), &resp)
	assert.Equal(t, int64(2), resp.Total)
	assert.NotContains(t, entryNames(resp), "library/nginx")
}

func TestDeleteCacheEntry_RejectsMalformedID(t *testing.T) {
	h, m := newCacheHarness(t)
	seedCache(m)
	_, tok := h.mustCreateAdmin(t)

	rr := h.do("DELETE", "/api/v1/admin/cache/oci/!!!not-base64!!!", tok, nil)
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

// An id minted from another protocol's tab must not delete across protocols.
func TestDeleteCacheEntry_RejectsCrossProtocolID(t *testing.T) {
	h, m := newCacheHarness(t)
	seedCache(m)
	_, tok := h.mustCreateAdmin(t)

	ociID := meta.EncodeEntryID(artifact.ArtifactRef{
		Protocol: "oci", Name: "library/nginx", Version: "1.25",
	})
	rr := h.do("DELETE", "/api/v1/admin/cache/pypi/"+ociID, tok, nil)
	assert.Equal(t, http.StatusBadRequest, rr.Code)

	// The oci row must be untouched.
	var resp CacheEntriesResponse
	decodeJSON(t, h.do("GET", "/api/v1/admin/cache/oci", tok, nil), &resp)
	assert.Contains(t, entryNames(resp), "library/nginx")
}

func TestPinCacheEntry_SetsAndClearsAndFilters(t *testing.T) {
	h, m := newCacheHarness(t)
	seedCache(m)
	_, tok := h.mustCreateAdmin(t)

	id := meta.EncodeEntryID(artifact.ArtifactRef{
		Protocol: "oci", Name: "library/nginx", Version: "1.25",
	})

	rr := h.do("POST", "/api/v1/admin/cache/oci/"+id+"/pin", tok,
		jsonBody(PinCacheEntryRequest{Pinned: true}))
	require.Equal(t, http.StatusNoContent, rr.Code)

	var resp CacheEntriesResponse
	decodeJSON(t, h.do("GET", "/api/v1/admin/cache/oci?pinned=true", tok, nil), &resp)
	require.Len(t, resp.Entries, 1)
	assert.Equal(t, "library/nginx", resp.Entries[0].Name)
	assert.True(t, resp.Entries[0].Pinned)

	// The inverse filter must exclude it: pinned=false is a real predicate.
	decodeJSON(t, h.do("GET", "/api/v1/admin/cache/oci?pinned=false", tok, nil), &resp)
	assert.Equal(t, int64(2), resp.Total)

	rr = h.do("POST", "/api/v1/admin/cache/oci/"+id+"/pin", tok,
		jsonBody(PinCacheEntryRequest{Pinned: false}))
	require.Equal(t, http.StatusNoContent, rr.Code)
	decodeJSON(t, h.do("GET", "/api/v1/admin/cache/oci?pinned=true", tok, nil), &resp)
	assert.Zero(t, resp.Total)
}

func TestPinCacheEntry_RejectsBadBody(t *testing.T) {
	h, m := newCacheHarness(t)
	seedCache(m)
	_, tok := h.mustCreateAdmin(t)

	id := meta.EncodeEntryID(artifact.ArtifactRef{
		Protocol: "oci", Name: "library/nginx", Version: "1.25",
	})
	rr := h.do("POST", "/api/v1/admin/cache/oci/"+id+"/pin", tok, strings.NewReader("{not json"))
	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

// The cache spans every tenant, so browsing and mutating it is system-admin only.
func TestCacheEndpoints_RequireSystemAdmin(t *testing.T) {
	h, m := newCacheHarness(t)
	seedCache(m)
	_, tok := h.mustCreateUser(t, "plain@example.com")

	id := meta.EncodeEntryID(artifact.ArtifactRef{
		Protocol: "oci", Name: "library/nginx", Version: "1.25",
	})
	assert.Equal(t, http.StatusForbidden, h.do("GET", "/api/v1/admin/cache/oci", tok, nil).Code)
	assert.Equal(t, http.StatusForbidden, h.do("DELETE", "/api/v1/admin/cache/oci/"+id, tok, nil).Code)
	assert.Equal(t, http.StatusForbidden,
		h.do("POST", "/api/v1/admin/cache/oci/"+id+"/pin", tok, jsonBody(PinCacheEntryRequest{Pinned: true})).Code)
}

func TestCacheEndpoints_RequireAuth(t *testing.T) {
	h, m := newCacheHarness(t)
	seedCache(m)

	assert.Equal(t, http.StatusUnauthorized, h.do("GET", "/api/v1/admin/cache/oci", "", nil).Code)
}
