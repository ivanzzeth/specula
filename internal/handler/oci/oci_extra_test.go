package oci

// oci_extra_test.go — additional whitebox tests for the OCI handler.
//
// These tests target the branches not reached by handler_test.go:
//   - isMutableExpired (all four TTL cases)
//   - mutableKey format
//   - Option functions (WithMeta, WithMutableTTL, WithQuarantineDir, WithLogger, WithOwnedNamespaceResolver)
//   - resolveManifestDigest with h.meta injected (meta error, expired, found)
//   - OwnedNamespaceResolver (error path, owned=true path)
//   - HostedResolver error path (fail-open)
//   - HostedReadAuthz nil path (fail-open for nil authz)
//   - Blob cache-lookup error → 500 INTERNAL_ERROR
//   - Blob Serve returns error after Lookup success → 404 (DESIGN-REVIEW M1: metadata hit but blob missing = MISS)
//   - Blob Range not satisfiable → 416
//   - Manifest cache-lookup error → 500 INTERNAL_ERROR
//   - Manifest owned-namespace gate → 404, no upstream fallthrough
//   - Blob owned-namespace gate → 404, no upstream fallthrough
//   - Fetch blob from upstream: digest mismatch → 502 (verify-on-write C2 fix)
//   - Fetch manifest: Store failure → 502 (C2: unverified bytes never served)
//   - Fetch manifest: PutMutable failure is non-fatal → 200
//   - ServeHTTP /v2 without trailing slash → 200
//   - ServeHTTP method-not-allowed on manifests, blobs, and version probe
//   - ServeHTTP missing name segments → 400
//   - Blob request with tag ref instead of digest → 400 DIGEST_INVALID

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/store/meta"
	"github.com/ivanzzeth/specula/internal/upstream"
)

// ── fakeMetaStore ─────────────────────────────────────────────────────────────

// fakeMetaStore is an in-memory MetadataStore for whitebox OCI handler tests.
// Supports error injection on GetMutable (getMutErr) and PutMutable (putMutErr).
type fakeMetaStore struct {
	mutable   map[string]artifact.MutableEntry
	getMutErr error
	putMutErr error
}

func newFakeMetaStore() *fakeMetaStore {
	return &fakeMetaStore{mutable: map[string]artifact.MutableEntry{}}
}

var _ meta.MetadataStore = (*fakeMetaStore)(nil)

func (*fakeMetaStore) Get(_ context.Context, _ artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	return nil, nil
}
func (*fakeMetaStore) Put(_ context.Context, _ artifact.CacheEntry) error     { return nil }
func (*fakeMetaStore) Delete(_ context.Context, _ artifact.ArtifactRef) error { return nil }
func (*fakeMetaStore) DeleteMutable(_ context.Context, _ string) error        { return nil }

func (m *fakeMetaStore) GetMutable(_ context.Context, key string) (*artifact.MutableEntry, error) {
	if m.getMutErr != nil {
		return nil, m.getMutErr
	}
	e, ok := m.mutable[key]
	if !ok {
		return nil, nil
	}
	return &e, nil
}

func (m *fakeMetaStore) PutMutable(_ context.Context, e artifact.MutableEntry) error {
	if m.putMutErr != nil {
		return m.putMutErr
	}
	m.mutable[e.Key] = e
	return nil
}

func (*fakeMetaStore) CacheSizeByProtocol(_ context.Context) (map[string]artifact.SizeStat, error) {
	return map[string]artifact.SizeStat{}, nil
}

func (*fakeMetaStore) CacheSizeByOrigin(_ context.Context) (map[string]artifact.SizeStat, error) {
	return map[string]artifact.SizeStat{}, nil
}
func (*fakeMetaStore) ListEntries(_ context.Context, _ string, _ meta.EntryFilter, _ meta.Page) (meta.EntryPage, error) {
	return meta.EntryPage{}, nil
}
func (*fakeMetaStore) SetPinned(_ context.Context, _ artifact.ArtifactRef, _ bool) error { return nil }

// ── alwaysFailStoreCache ──────────────────────────────────────────────────────

// alwaysFailStoreCache is a CacheManager that always misses on Lookup (handler
// enters the upstream fetch path) but fails on Store (simulating the C2
// verify-on-write failure: bytes fetched but verification rejected them).
type alwaysFailStoreCache struct{}

func (*alwaysFailStoreCache) Lookup(_ context.Context, _ artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	return nil, nil
}
func (*alwaysFailStoreCache) Store(_ context.Context, _ artifact.ArtifactRef, art *artifact.Artifact) (*artifact.CacheEntry, error) {
	if art != nil && art.Path != "" {
		_ = os.Remove(art.Path) // clean up the quarantine file to avoid leaks
	}
	return nil, errors.New("fake: verify-on-write failure — bytes rejected")
}
func (*alwaysFailStoreCache) Serve(_ context.Context, _ artifact.ArtifactRef, _, _ int64) (io.ReadCloser, *artifact.CacheEntry, error) {
	return nil, nil, errors.New("fake: not found")
}

// ── errorLookupCache ──────────────────────────────────────────────────────────

// errorLookupCache always returns an error from Lookup, simulating a metadata
// store failure. The handler must surface 500 INTERNAL_ERROR, never silently
// convert this to a 404 (which would imply the blob/manifest does not exist).
type errorLookupCache struct{}

func (*errorLookupCache) Lookup(_ context.Context, _ artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	return nil, errors.New("fake: cache lookup failure")
}
func (*errorLookupCache) Store(_ context.Context, _ artifact.ArtifactRef, _ *artifact.Artifact) (*artifact.CacheEntry, error) {
	return nil, errors.New("fake: not implemented")
}
func (*errorLookupCache) Serve(_ context.Context, _ artifact.ArtifactRef, _, _ int64) (io.ReadCloser, *artifact.CacheEntry, error) {
	return nil, nil, errors.New("fake: not found")
}

// ── lookupHitServeErrorCache ──────────────────────────────────────────────────

// lookupHitServeErrorCache simulates DESIGN-REVIEW M1: Lookup succeeds (metadata
// says the blob exists) but Serve fails (the underlying bytes are gone, e.g.
// after a partial GC run). The handler must return 404 BLOB_UNKNOWN, not serve
// empty or stale bytes.
type lookupHitServeErrorCache struct {
	entry *artifact.CacheEntry
}

func (c *lookupHitServeErrorCache) Lookup(_ context.Context, _ artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	return c.entry, nil
}
func (c *lookupHitServeErrorCache) Store(_ context.Context, _ artifact.ArtifactRef, _ *artifact.Artifact) (*artifact.CacheEntry, error) {
	return nil, errors.New("fake: not implemented")
}
func (c *lookupHitServeErrorCache) Serve(_ context.Context, _ artifact.ArtifactRef, _, _ int64) (io.ReadCloser, *artifact.CacheEntry, error) {
	return nil, nil, errors.New("fake: underlying blob file missing")
}

// ── OwnedNamespaceResolver test doubles ───────────────────────────────────────

type staticOwnedNS struct{ owned map[string]bool }

func (o *staticOwnedNS) IsOwnedNamespace(_ context.Context, name string) (bool, error) {
	return o.owned[name], nil
}

type errorOwnedNS struct{}

func (*errorOwnedNS) IsOwnedNamespace(_ context.Context, _ string) (bool, error) {
	return false, errors.New("owned-ns resolver: injected error")
}

// ── errorHostedResolver ────────────────────────────────────────────────────────

type errorHostedResolver struct{}

func (*errorHostedResolver) ResolveHosted(_ context.Context, _ string) (bool, error) {
	return false, errors.New("hosted resolver: injected error")
}

// ═══════════════════════════════════════════════════════════════════════════════
// Tests
// ═══════════════════════════════════════════════════════════════════════════════

// ── isMutableExpired ──────────────────────────────────────────────────────────

// TestIsMutableExpired pins all four TTL cases for the gating function that
// decides whether a mutable (tag→digest) cache entry has exceeded its TTL and
// must trigger an upstream re-probe (DESIGN-REVIEW H1).
func TestIsMutableExpired(t *testing.T) {
	t.Run("never-revalidate sentinel (-1)", func(t *testing.T) {
		e := &artifact.MutableEntry{TTLSeconds: ttlNeverRevalidate, FetchedAt: time.Now().Add(-365 * 24 * time.Hour)}
		assert.False(t, isMutableExpired(e), "TTL=-1 must never expire")
	})
	t.Run("always-revalidate sentinel (0)", func(t *testing.T) {
		e := &artifact.MutableEntry{TTLSeconds: ttlAlwaysRevalidate, FetchedAt: time.Now()}
		assert.True(t, isMutableExpired(e), "TTL=0 must always be expired")
	})
	t.Run("within TTL", func(t *testing.T) {
		e := &artifact.MutableEntry{TTLSeconds: 300, FetchedAt: time.Now().Add(-60 * time.Second)}
		assert.False(t, isMutableExpired(e), "fetched 60s ago with 300s TTL must not be expired")
	})
	t.Run("past TTL", func(t *testing.T) {
		e := &artifact.MutableEntry{TTLSeconds: 60, FetchedAt: time.Now().Add(-2 * time.Minute)}
		assert.True(t, isMutableExpired(e), "fetched 2m ago with 60s TTL must be expired")
	})
}

// TestMutableKey verifies the mutable-tier key format used by MetadataStore
// lookups. A key change would break cache hit/miss semantics across restarts.
func TestMutableKey(t *testing.T) {
	assert.Equal(t, "oci:myorg/myapp:latest", mutableKey("myorg/myapp", "latest"))
	assert.Equal(t, "oci:library/nginx:stable", mutableKey("library/nginx", "stable"))
}

// ── Option functions ───────────────────────────────────────────────────────────

// TestWithOptionFunctions exercises every With* constructor option to confirm
// they compile, are applied, and are not permanently at 0% coverage.
func TestWithOptionFunctions(t *testing.T) {
	ms := newFakeMetaStore()
	h := NewHandler(
		&fakeCacheManager{entries: map[string]*artifact.CacheEntry{}, blobs: map[string][]byte{}},
		WithMeta(ms),
		WithMutableTTL(120),
		WithQuarantineDir(t.TempDir()),
		WithLogger(slog.Default()),
		WithOwnedNamespaceResolver(&staticOwnedNS{owned: map[string]bool{}}),
	)
	assert.NotNil(t, h)
	assert.Equal(t, int64(120), h.mutableTTLSec, "WithMutableTTL must set mutableTTLSec")
	assert.NotNil(t, h.meta, "WithMeta must set h.meta")
	assert.NotNil(t, h.owned, "WithOwnedNamespaceResolver must set h.owned")
}

// ── resolveManifestDigest with h.meta ─────────────────────────────────────────

// TestManifestServedFromMeta verifies that when h.meta is injected and holds a
// non-expired tag→digest entry, the manifest is served directly from the CAS
// without any upstream contact (ARCHITECTURE §3 mutable tier, DESIGN-REVIEW H1).
func TestManifestServedFromMeta(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","layers":[]}`)
	mDigest := sha256Digest(manifest)

	fake := &fakeCacheManager{
		entries: map[string]*artifact.CacheEntry{
			"oci:myrepo/img:" + mDigest: {Digest: mDigest, Size: int64(len(manifest)), Protocol: "oci"},
		},
		blobs: map[string][]byte{mDigest: manifest},
	}
	ms := newFakeMetaStore()
	ms.mutable[mutableKey("myrepo/img", "v2")] = artifact.MutableEntry{
		Key:        mutableKey("myrepo/img", "v2"),
		Digest:     mDigest,
		TTLSeconds: 300,
		FetchedAt:  time.Now(),
	}

	srv := httptest.NewServer(NewHandler(fake, WithMeta(ms)))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/myrepo/img/manifests/v2")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, mDigest, resp.Header.Get("Docker-Content-Digest"))
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, manifest, body)
}

// TestManifestMetaGetMutableError verifies that a MetadataStore error in
// resolveManifestDigest surfaces as 500 INTERNAL_ERROR. This must NEVER silently
// degrade to a 404 (which could cause unexpected upstream fetches).
func TestManifestMetaGetMutableError(t *testing.T) {
	ms := newFakeMetaStore()
	ms.getMutErr = errors.New("meta: injected GetMutable failure")

	srv := httptest.NewServer(NewHandler(
		&fakeCacheManager{entries: map[string]*artifact.CacheEntry{}, blobs: map[string][]byte{}},
		WithMeta(ms),
	))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/myrepo/img/manifests/latest")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "INTERNAL_ERROR")
}

// TestManifestMetaExpiredFallsThrough verifies that an expired mutable entry is
// treated as a cache miss (DESIGN-REVIEW H1 fix). Without an upstream configured
// the result is 404 MANIFEST_UNKNOWN.
func TestManifestMetaExpiredFallsThrough(t *testing.T) {
	ms := newFakeMetaStore()
	ms.mutable[mutableKey("myrepo/img", "latest")] = artifact.MutableEntry{
		Key:        mutableKey("myrepo/img", "latest"),
		Digest:     "sha256:" + strings.Repeat("a", 64),
		TTLSeconds: 60,
		FetchedAt:  time.Now().Add(-2 * time.Minute), // expired
	}

	srv := httptest.NewServer(NewHandler(
		&fakeCacheManager{entries: map[string]*artifact.CacheEntry{}, blobs: map[string][]byte{}},
		WithMeta(ms),
	))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/myrepo/img/manifests/latest")
	require.NoError(t, err)
	defer resp.Body.Close()

	// Expired → miss → no upstream → 404.
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "MANIFEST_UNKNOWN")
}

// TestFetchManifestPutMutableError verifies that a PutMutable failure (writing
// the tag→digest mutable pointer after a successful manifest fetch) is non-fatal:
// the manifest must still be served — it is in CAS, just without the fast-TTL
// lookup shortcut (ARCHITECTURE §4).
func TestFetchManifestPutMutableError(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","layers":[]}`)
	mDigest := sha256Digest(manifest)
	upSrv := ociUpstreamServer(t,
		map[string][]byte{"latest": manifest, mDigest: manifest}, nil, "")
	defer upSrv.Close()

	ms := newFakeMetaStore()
	ms.putMutErr = errors.New("meta: injected PutMutable failure")

	fc := newStoringFakeCache()
	srv := httptest.NewServer(NewHandler(fc,
		WithUpstream(upstream.NewClient(), []upstream.Upstream{{Name: "fake", BaseURL: upSrv.URL, Priority: 1}}),
		WithMeta(ms),
		WithQuarantineDir(t.TempDir()),
	))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/library/img/manifests/latest")
	require.NoError(t, err)
	defer resp.Body.Close()

	// PutMutable failure is non-fatal: manifest still served from CAS.
	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"PutMutable failure must not block the response — manifest is already in CAS")
	assert.Equal(t, mDigest, resp.Header.Get("Docker-Content-Digest"))
}

// ── Manifest cache-lookup error ────────────────────────────────────────────────

// TestManifestCacheLookupError verifies that a cache.Lookup failure during
// manifest resolution surfaces as 500 INTERNAL_ERROR.
func TestManifestCacheLookupError(t *testing.T) {
	srv := httptest.NewServer(NewHandler(&errorLookupCache{}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/myrepo/img/manifests/latest")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "INTERNAL_ERROR")
}

// ── Blob cache-lookup error ────────────────────────────────────────────────────

// TestBlobCacheLookupError verifies that a cache.Lookup failure during blob
// serving surfaces as 500 INTERNAL_ERROR.
func TestBlobCacheLookupError(t *testing.T) {
	srv := httptest.NewServer(NewHandler(&errorLookupCache{}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/myrepo/img/blobs/sha256:" + strings.Repeat("a", 64))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "INTERNAL_ERROR")
}

// ── DESIGN-REVIEW M1: metadata hit but blob missing ───────────────────────────

// TestBlobMetadataHitButBlobMissing validates DESIGN-REVIEW M1:
// "metadata hit but blob missing must be treated as a MISS → 404 BLOB_UNKNOWN".
//
// When Lookup returns a non-nil CacheEntry but Serve returns an error (the CAS
// bytes are gone — e.g. partial GC or storage failure), the response must be
// 404, never a 200 with empty or corrupted data.
func TestBlobMetadataHitButBlobMissing(t *testing.T) {
	bDigest := "sha256:" + strings.Repeat("c", 64)
	srv := httptest.NewServer(NewHandler(&lookupHitServeErrorCache{
		entry: &artifact.CacheEntry{Digest: bDigest, Size: 42, Protocol: "oci"},
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/myrepo/img/blobs/" + bDigest)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode,
		"DESIGN-REVIEW M1: metadata hit with missing CAS bytes must be 404, not 200")
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "BLOB_UNKNOWN")
}

// ── Range not satisfiable ─────────────────────────────────────────────────────

// TestBlobRangeNotSatisfiable verifies that a Range header whose start offset
// exceeds the blob size returns 416 with the correct Content-Range: bytes */size
// header (ARCHITECTURE §6 M2 — containerd uses ranged GETs for chunked pulls).
func TestBlobRangeNotSatisfiable(t *testing.T) {
	blobData := bytes.Repeat([]byte("R"), 512)
	bDigest := sha256Digest(blobData)

	fake := &fakeCacheManager{
		entries: map[string]*artifact.CacheEntry{
			"oci:myrepo/img:" + bDigest: {Digest: bDigest, Size: int64(len(blobData)), Protocol: "oci"},
		},
		blobs: map[string][]byte{bDigest: blobData},
	}
	srv := httptest.NewServer(NewHandler(fake))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/myrepo/img/blobs/"+bDigest, nil)
	req.Header.Set("Range", "bytes=9999-99999") // start well past EOF
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusRequestedRangeNotSatisfiable, resp.StatusCode)
	assert.Equal(t, fmt.Sprintf("bytes */%d", len(blobData)), resp.Header.Get("Content-Range"))
}

// TestBlobInvalidRangeUnit verifies an unsupported range unit (e.g. "items=")
// is rejected with 416.
func TestBlobInvalidRangeUnit(t *testing.T) {
	blobData := []byte("content")
	bDigest := sha256Digest(blobData)

	fake := &fakeCacheManager{
		entries: map[string]*artifact.CacheEntry{
			"oci:myrepo/img:" + bDigest: {Digest: bDigest, Size: int64(len(blobData)), Protocol: "oci"},
		},
		blobs: map[string][]byte{bDigest: blobData},
	}
	srv := httptest.NewServer(NewHandler(fake))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/myrepo/img/blobs/"+bDigest, nil)
	req.Header.Set("Range", "items=0-3") // wrong unit
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusRequestedRangeNotSatisfiable, resp.StatusCode)
}

// ── OwnedNamespaceResolver paths ─────────────────────────────────────────────

// TestManifestOwnedNamespaceNoUpstream verifies REGISTRY-DESIGN §0:
// "Owned-namespace names are authoritative-local: a cache miss must return 404
// and MUST NOT fall through to a configured upstream mirror."
func TestManifestOwnedNamespaceNoUpstream(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2}`)
	mDigest := sha256Digest(manifest)
	upSrv := ociUpstreamServer(t,
		map[string][]byte{"latest": manifest, mDigest: manifest}, nil, "")
	defer upSrv.Close()

	fc := newStoringFakeCache()
	srv := httptest.NewServer(NewHandler(fc,
		WithUpstream(upstream.NewClient(), []upstream.Upstream{{Name: "fake", BaseURL: upSrv.URL, Priority: 1}}),
		WithOwnedNamespaceResolver(&staticOwnedNS{owned: map[string]bool{"myorg/secret": true}}),
	))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/myorg/secret/manifests/latest")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode,
		"owned-namespace miss must never fall through to upstream (dependency-confusion guard)")
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "MANIFEST_UNKNOWN")
}

// TestBlobOwnedNamespaceNoUpstream verifies the blob path equivalent of the
// owned-namespace gate: a blob miss for an owned-namespace repo must return 404,
// never fetch from upstream.
func TestBlobOwnedNamespaceNoUpstream(t *testing.T) {
	bDigest := "sha256:" + strings.Repeat("d", 64)
	upSrv := ociUpstreamServer(t, nil, map[string][]byte{bDigest: []byte("upstream bytes")}, "")
	defer upSrv.Close()

	fc := newStoringFakeCache()
	srv := httptest.NewServer(NewHandler(fc,
		WithUpstream(upstream.NewClient(), []upstream.Upstream{{Name: "fake", BaseURL: upSrv.URL, Priority: 1}}),
		WithOwnedNamespaceResolver(&staticOwnedNS{owned: map[string]bool{"myorg/secret": true}}),
	))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/myorg/secret/blobs/" + bDigest)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode,
		"owned-namespace blob miss must not fall through to upstream")
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "BLOB_UNKNOWN")
}

// TestOwnedNamespaceResolverError verifies that when the OwnedNamespaceResolver
// errors, the handler fails open to the upstream pull path — availability is
// preserved when the namespace store is temporarily unavailable.
func TestOwnedNamespaceResolverError(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","layers":[]}`)
	mDigest := sha256Digest(manifest)
	upSrv := ociUpstreamServer(t,
		map[string][]byte{"latest": manifest, mDigest: manifest}, nil, "")
	defer upSrv.Close()

	fc := newStoringFakeCache()
	srv := httptest.NewServer(NewHandler(fc,
		WithUpstream(upstream.NewClient(), []upstream.Upstream{{Name: "fake", BaseURL: upSrv.URL, Priority: 1}}),
		WithOwnedNamespaceResolver(&errorOwnedNS{}),
	))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/library/img/manifests/latest")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"owned-namespace resolver error must fail open to preserve pull availability")
}

// TestHostedResolverError verifies that when the HostedResolver errors, the
// handler fails open to the upstream pull path.
func TestHostedResolverError(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","layers":[]}`)
	mDigest := sha256Digest(manifest)
	upSrv := ociUpstreamServer(t,
		map[string][]byte{"latest": manifest, mDigest: manifest}, nil, "")
	defer upSrv.Close()

	fc := newStoringFakeCache()
	srv := httptest.NewServer(NewHandler(fc,
		WithUpstream(upstream.NewClient(), []upstream.Upstream{{Name: "fake", BaseURL: upSrv.URL, Priority: 1}}),
		WithHostedResolver(&errorHostedResolver{}),
	))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/library/img/manifests/latest")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"hosted-resolver error must fail open to preserve pull availability")
}

// TestHostedReadAuthzNilAllowsAll verifies that when no HostedReadAuthz is
// injected (h.hostedAuthz == nil), hosted repos are readable without an access
// check — the fail-open safety net for dev/test deployments.
func TestHostedReadAuthzNilAllowsAll(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2}`)
	mDigest := sha256Digest(manifest)

	fake := &fakeCacheManager{
		entries: map[string]*artifact.CacheEntry{
			"oci:acme/pub:latest":     {Digest: mDigest, Size: int64(len(manifest)), Protocol: "oci"},
			"oci:acme/pub:" + mDigest: {Digest: mDigest, Size: int64(len(manifest)), Protocol: "oci"},
		},
		blobs: map[string][]byte{mDigest: manifest},
	}
	// HostedResolver says repo is hosted, but NO HostedReadAuthz is injected.
	srv := httptest.NewServer(NewHandler(fake,
		WithHostedResolver(&fakeHostedResolver{hosted: map[string]bool{"acme/pub": true}}),
	))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/acme/pub/manifests/latest")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"nil HostedReadAuthz must allow all reads (fail-open for unconfigured deployments)")
}

// ── Verify-on-write (C2) regression tests ─────────────────────────────────────

// TestFetchBlobDigestMismatch verifies DESIGN-REVIEW C2 / ARCHITECTURE §4:
// "Unverified bytes are NEVER served."
//
// When the upstream serves bytes that do not hash to the requested digest, the
// handler must return 502 Bad Gateway. The wrong bytes must never reach the
// client or land in the CAS.
func TestFetchBlobDigestMismatch(t *testing.T) {
	requestedDigest := "sha256:" + strings.Repeat("a", 64)
	// Map the requested digest key to wrong bytes — simulates a malicious or
	// bit-rotted mirror that claims to serve sha256:aaa… but doesn't.
	wrongBytes := []byte("impostor content — sha256 of this is NOT aaa...")
	upSrv := ociUpstreamServer(t, nil, map[string][]byte{requestedDigest: wrongBytes}, "")
	defer upSrv.Close()

	fc := newStoringFakeCache()
	srv := httptest.NewServer(NewHandler(fc,
		WithUpstream(upstream.NewClient(), []upstream.Upstream{{Name: "fake", BaseURL: upSrv.URL, Priority: 1}}),
		WithQuarantineDir(t.TempDir()),
	))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/library/img/blobs/" + requestedDigest)
	require.NoError(t, err)
	defer resp.Body.Close()

	// DESIGN-REVIEW C2: digest mismatch → wrong bytes must be rejected → 502.
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode,
		"DESIGN-REVIEW C2: upstream digest mismatch must return 502 — wrong bytes must never be served")
}

// TestFetchManifestStoreError verifies DESIGN-REVIEW C2 for manifests:
// a Store failure (simulating verification rejection) must block the response
// with 502 — unverified bytes must never be served.
func TestFetchManifestStoreError(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","layers":[]}`)
	mDigest := sha256Digest(manifest)
	upSrv := ociUpstreamServer(t,
		map[string][]byte{"latest": manifest, mDigest: manifest}, nil, "")
	defer upSrv.Close()

	srv := httptest.NewServer(NewHandler(&alwaysFailStoreCache{},
		WithUpstream(upstream.NewClient(), []upstream.Upstream{{Name: "fake", BaseURL: upSrv.URL, Priority: 1}}),
		WithQuarantineDir(t.TempDir()),
	))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/library/img/manifests/latest")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadGateway, resp.StatusCode,
		"DESIGN-REVIEW C2: Store failure must block manifest response (verify-on-write)")
}

// ── ServeHTTP routing edge cases ──────────────────────────────────────────────

// TestVersionProbeNoTrailingSlash verifies GET /v2 (no trailing slash) returns
// the same 200 response as GET /v2/ (the OCI version probe).
func TestVersionProbeNoTrailingSlash(t *testing.T) {
	_, srv := newTestServer(&fakeCacheManager{entries: map[string]*artifact.CacheEntry{}, blobs: map[string][]byte{}})
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "registry/2.0", resp.Header.Get("Docker-Distribution-Api-Version"))
}

// TestVersionProbeMethodNotAllowed verifies POST on /v2/ returns 405.
func TestVersionProbeMethodNotAllowed(t *testing.T) {
	_, srv := newTestServer(&fakeCacheManager{entries: map[string]*artifact.CacheEntry{}, blobs: map[string][]byte{}})
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v2/", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

// TestManifestMethodNotAllowed verifies POST on a manifests URL returns 405.
func TestManifestMethodNotAllowed(t *testing.T) {
	_, srv := newTestServer(&fakeCacheManager{entries: map[string]*artifact.CacheEntry{}, blobs: map[string][]byte{}})
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v2/myrepo/img/manifests/latest", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

// TestBlobMethodNotAllowed verifies POST on a blobs URL returns 405.
func TestBlobMethodNotAllowed(t *testing.T) {
	_, srv := newTestServer(&fakeCacheManager{entries: map[string]*artifact.CacheEntry{}, blobs: map[string][]byte{}})
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v2/myrepo/img/blobs/sha256:"+strings.Repeat("a", 64), nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

// TestMissingImageNameManifest verifies /v2//manifests/v1 (empty name) → 400.
func TestMissingImageNameManifest(t *testing.T) {
	_, srv := newTestServer(&fakeCacheManager{entries: map[string]*artifact.CacheEntry{}, blobs: map[string][]byte{}})
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2//manifests/v1")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "NAME_INVALID")
}

// TestMissingImageNameBlob verifies /v2//blobs/sha256:... (empty name) → 400.
func TestMissingImageNameBlob(t *testing.T) {
	_, srv := newTestServer(&fakeCacheManager{entries: map[string]*artifact.CacheEntry{}, blobs: map[string][]byte{}})
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2//blobs/sha256:" + strings.Repeat("a", 64))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "DIGEST_INVALID")
}

// TestBlobInvalidDigestRef verifies that requesting a blob with a plain tag
// name (not a content digest) returns 400 DIGEST_INVALID.
func TestBlobInvalidDigestRef(t *testing.T) {
	_, srv := newTestServer(&fakeCacheManager{entries: map[string]*artifact.CacheEntry{}, blobs: map[string][]byte{}})
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/myrepo/img/blobs/latest")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "DIGEST_INVALID")
}

// TestNonV2PathReturns404 verifies that paths outside /v2/ return 404.
func TestNonV2PathReturns404(t *testing.T) {
	_, srv := newTestServer(&fakeCacheManager{entries: map[string]*artifact.CacheEntry{}, blobs: map[string][]byte{}})
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}
