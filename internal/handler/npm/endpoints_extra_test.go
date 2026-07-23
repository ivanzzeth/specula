package npm

// endpoints_extra_test.go — coverage for npm endpoints.go error paths,
// mutable-TTL helpers, and stale-serve pipeline.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
	"github.com/ivanzzeth/specula/internal/store/meta"
	"github.com/ivanzzeth/specula/internal/upstream"
)

// ── fakeNpmMetaStore ─────────────────────────────────────────────────────────

type fakeNpmMetaStore struct {
	mu      sync.Mutex
	mutable map[string]*artifact.MutableEntry
}

func newFakeNpmMetaStore() *fakeNpmMetaStore {
	return &fakeNpmMetaStore{mutable: make(map[string]*artifact.MutableEntry)}
}

var _ meta.MetadataStore = (*fakeNpmMetaStore)(nil)

func (f *fakeNpmMetaStore) Get(_ context.Context, _ artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	return nil, nil
}
func (f *fakeNpmMetaStore) Put(_ context.Context, _ artifact.CacheEntry) error     { return nil }
func (f *fakeNpmMetaStore) Delete(_ context.Context, _ artifact.ArtifactRef) error { return nil }

func (f *fakeNpmMetaStore) GetMutable(_ context.Context, key string) (*artifact.MutableEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.mutable[key]
	if e == nil {
		return nil, nil
	}
	cp := *e
	return &cp, nil
}

func (f *fakeNpmMetaStore) PutMutable(_ context.Context, entry artifact.MutableEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := entry
	f.mutable[entry.Key] = &cp
	return nil
}

func (f *fakeNpmMetaStore) DeleteMutable(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.mutable, key)
	return nil
}

func (f *fakeNpmMetaStore) CacheSizeByProtocol(_ context.Context) (map[string]artifact.SizeStat, error) {
	return nil, nil
}

func (f *fakeNpmMetaStore) CacheSizeByOrigin(_ context.Context) (map[string]artifact.SizeStat, error) {
	return map[string]artifact.SizeStat{}, nil
}

func (f *fakeNpmMetaStore) ListEntries(_ context.Context, _ string, _ meta.EntryFilter, _ meta.Page) (meta.EntryPage, error) {
	return meta.EntryPage{}, nil
}

func (f *fakeNpmMetaStore) SetPinned(_ context.Context, _ artifact.ArtifactRef, _ bool) error {
	return nil
}

// ── errServeCacheManager — Serve always returns an error ─────────────────────

type errServeCacheManager struct {
	npmTestCache
	serveErr error
}

func (e *errServeCacheManager) Serve(_ context.Context, _ artifact.ArtifactRef, _, _ int64) (io.ReadCloser, *artifact.CacheEntry, error) {
	return nil, nil, e.serveErr
}

// ServeEntry mirrors the Serve injection: serveFromCache reaches the cache via
// ServeEntry whenever it holds an entry.
func (e *errServeCacheManager) ServeEntry(_ context.Context, _ *artifact.CacheEntry, _, _ int64) (io.ReadCloser, error) {
	return nil, e.serveErr
}

// ── nilRCNpmCacheManager — Serve returns (nil, nil, nil) ─────────────────────

type nilRCNpmCacheManager struct {
	npmTestCache
}

func (n *nilRCNpmCacheManager) Serve(_ context.Context, _ artifact.ArtifactRef, _, _ int64) (io.ReadCloser, *artifact.CacheEntry, error) {
	return nil, nil, nil
}

// ServeEntry mirrors the nil-reader injection on the path serveFromCache uses.
func (n *nilRCNpmCacheManager) ServeEntry(_ context.Context, _ *artifact.CacheEntry, _, _ int64) (io.ReadCloser, error) {
	return nil, nil
}

// ── lookupErrNpmCacheManager — Lookup returns error ──────────────────────────

type lookupErrNpmCacheManager struct {
	npmTestCache
}

func (l *lookupErrNpmCacheManager) Lookup(_ context.Context, _ artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	return nil, errors.New("lookup error injected for npm test")
}

// ── Tests: getMutableUpstreamMeta ─────────────────────────────────────────────

func TestNpmGetMutableUpstreamMeta_NoMeta(t *testing.T) {
	h := NewHandler(newNpmTestCache())
	ref := packumentRef("react")
	_, ok := h.getMutableUpstreamMeta(context.Background(), ref)
	assert.False(t, ok)
}

func TestNpmGetMutableUpstreamMeta_NotPresent(t *testing.T) {
	ms := newFakeNpmMetaStore()
	h := NewHandler(newNpmTestCache(), WithMeta(ms))
	_, ok := h.getMutableUpstreamMeta(context.Background(), packumentRef("react"))
	assert.False(t, ok)
}

func TestNpmGetMutableUpstreamMeta_NoETag(t *testing.T) {
	ms := newFakeNpmMetaStore()
	ref := packumentRef("react")
	_ = ms.PutMutable(context.Background(), artifact.MutableEntry{
		Key:      npmMutableKey(ref),
		Protocol: Protocol,
		// No ETag or LastModified
	})
	h := NewHandler(newNpmTestCache(), WithMeta(ms))
	_, ok := h.getMutableUpstreamMeta(context.Background(), ref)
	assert.False(t, ok, "entry with no ETag/LastModified must return false")
}

func TestNpmGetMutableUpstreamMeta_WithETag(t *testing.T) {
	ms := newFakeNpmMetaStore()
	ref := packumentRef("lodash")
	_ = ms.PutMutable(context.Background(), artifact.MutableEntry{
		Key:      npmMutableKey(ref),
		Protocol: Protocol,
		ETag:     `"etag-xyz"`,
		Upstream: "npm-mirror",
	})
	h := NewHandler(newNpmTestCache(), WithMeta(ms))
	umeta, ok := h.getMutableUpstreamMeta(context.Background(), ref)
	require.True(t, ok)
	assert.Equal(t, `"etag-xyz"`, umeta.ETag)
	assert.Equal(t, "npm-mirror", umeta.Upstream)
}

// ── Tests: extendMutableTTL ───────────────────────────────────────────────────

func TestNpmExtendMutableTTL_NoMeta(t *testing.T) {
	h := NewHandler(newNpmTestCache())
	// Must not panic with nil meta.
	h.extendMutableTTL(context.Background(), packumentRef("react"), artifact.UpstreamMeta{})
}

func TestNpmExtendMutableTTL_NotPresent(t *testing.T) {
	ms := newFakeNpmMetaStore()
	h := NewHandler(newNpmTestCache(), WithMeta(ms))
	// Should not panic or create a new entry.
	h.extendMutableTTL(context.Background(), packumentRef("react"), artifact.UpstreamMeta{ETag: `"new"`})
}

func TestNpmExtendMutableTTL_UpdatesFetchedAt(t *testing.T) {
	ms := newFakeNpmMetaStore()
	ref := packumentRef("lodash")
	before := time.Now().Add(-5 * time.Minute)
	_ = ms.PutMutable(context.Background(), artifact.MutableEntry{
		Key:       npmMutableKey(ref),
		Protocol:  Protocol,
		ETag:      `"old-etag"`,
		FetchedAt: before,
	})

	h := NewHandler(newNpmTestCache(), WithMeta(ms))
	h.extendMutableTTL(context.Background(), ref, artifact.UpstreamMeta{ETag: `"new-etag"`})

	got, err := ms.GetMutable(context.Background(), npmMutableKey(ref))
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, `"new-etag"`, got.ETag, "ETag must be updated")
	assert.True(t, got.FetchedAt.After(before), "FetchedAt must be refreshed")
}

// ── Tests: fetchBodyAndStore writes mutable TTL ───────────────────────────────

func TestNpmFetchBodyAndStore_WritesMeta(t *testing.T) {
	ms := newFakeNpmMetaStore()
	cm := newNpmTestCache()
	tmp := t.TempDir()
	h := NewHandler(cm, WithMeta(ms), WithMutableTTL(120), WithQuarantineDir(tmp))

	ref := packumentRef("react")
	body := bytes.NewReader([]byte(`{"name":"react","versions":{}}`))
	umeta := artifact.UpstreamMeta{ETag: `"etag-new"`, Upstream: "fake-npm"}

	entry, err := h.fetchBodyAndStore(context.Background(), ref, body, umeta)
	require.NoError(t, err)
	assert.NotNil(t, entry)

	got, err := ms.GetMutable(context.Background(), npmMutableKey(ref))
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, int64(120), got.TTLSeconds)
	assert.Equal(t, `"etag-new"`, got.ETag)
}

// ── Tests: serveFromCache error paths (packument) ─────────────────────────────

func TestNpmServeFromCache_ServeError_500(t *testing.T) {
	cm := &errServeCacheManager{
		npmTestCache: *newNpmTestCache(),
		serveErr:     errors.New("disk error"),
	}
	ref := packumentRef("react")
	cm.seed(ref, []byte(`{"name":"react","versions":{}}`))

	h := NewHandler(cm)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/react")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestNpmServeFromCache_ErrCacheMiss_404(t *testing.T) {
	cm := &errServeCacheManager{
		npmTestCache: *newNpmTestCache(),
		serveErr:     cache.ErrCacheMiss,
	}
	ref := packumentRef("react")
	cm.seed(ref, []byte(`{"name":"react","versions":{}}`))

	h := NewHandler(cm)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/react")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestNpmServeFromCache_NilRC_404(t *testing.T) {
	cm := &nilRCNpmCacheManager{npmTestCache: *newNpmTestCache()}
	ref := packumentRef("react")
	cm.seed(ref, []byte(`{"name":"react","versions":{}}`))

	h := NewHandler(cm)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/react")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ── Tests: serveTarballFromCache error paths ──────────────────────────────────

func TestNpmServeTarballFromCache_ServeError_500(t *testing.T) {
	cm := &errServeCacheManager{
		npmTestCache: *newNpmTestCache(),
		serveErr:     errors.New("I/O failure"),
	}
	ref := tarballRef("react", "react-18.2.0.tgz")
	cm.seed(ref, bytes.Repeat([]byte("TGZ"), 64))

	h := NewHandler(cm)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/react/-/react-18.2.0.tgz")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestNpmServeTarballFromCache_ErrCacheMiss_404(t *testing.T) {
	cm := &errServeCacheManager{
		npmTestCache: *newNpmTestCache(),
		serveErr:     cache.ErrCacheMiss,
	}
	ref := tarballRef("react", "react-18.2.0.tgz")
	cm.seed(ref, bytes.Repeat([]byte("TGZ"), 64))

	h := NewHandler(cm)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/react/-/react-18.2.0.tgz")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestNpmServeTarballFromCache_NilRC_404(t *testing.T) {
	cm := &nilRCNpmCacheManager{npmTestCache: *newNpmTestCache()}
	ref := tarballRef("react", "react-18.2.0.tgz")
	cm.seed(ref, bytes.Repeat([]byte("TGZ"), 64))

	h := NewHandler(cm)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/react/-/react-18.2.0.tgz")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ── Tests: serveMutable lookup error → 500 ───────────────────────────────────

func TestNpmServeMutable_LookupError_500(t *testing.T) {
	cm := &lookupErrNpmCacheManager{npmTestCache: *newNpmTestCache()}

	h := NewHandler(cm)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/react")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

// ── Tests: serveImmutable lookup error → 500 ─────────────────────────────────

func TestNpmServeImmutable_LookupError_500(t *testing.T) {
	cm := &lookupErrNpmCacheManager{npmTestCache: *newNpmTestCache()}

	h := NewHandler(cm)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/react/-/react-18.2.0.tgz")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

// ── Tests: serveImmutable upstream 404 → 404 ─────────────────────────────────

func TestNpmServeImmutable_Upstream404_NotFound(t *testing.T) {
	// Upstream returns 404 → tarball fetch error → 404
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer failSrv.Close()

	cm := newNpmTestCache()
	h := newNpmHandlerWithUpstream(cm, failSrv.URL)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/react/-/react-18.2.0.tgz")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ── Tests: privateDownServeStale ──────────────────────────────────────────────

func TestNpmPrivateDownServeStale_FailClosed(t *testing.T) {
	h := NewHandler(newNpmTestCache(),
		WithPrivateUnscoped([]string{"secret-pkg"}),
		WithFailClosed(true),
	)
	assert.False(t, h.privateDownServeStale(), "FailClosed=true must not serve stale")
}

func TestNpmPrivateDownServeStale_FailOpen(t *testing.T) {
	h := &Handler{
		failClosed:      false,
		privateUnscoped: []string{"secret-pkg"},
	}
	assert.True(t, h.privateDownServeStale(), "FailClosed=false must serve stale when available")
}

// ── Tests: selectUpstreams ────────────────────────────────────────────────────

func TestNpmSelectUpstreams_PublicPkg(t *testing.T) {
	pubUpstream := upstream.Upstream{Name: "public", BaseURL: "https://registry.npmjs.org"}
	h := NewHandler(newNpmTestCache(),
		WithPrivateScopes([]string{"@corp"}),
		WithUpstream(upstream.NewClient(), []upstream.Upstream{pubUpstream}),
	)
	ups, err := h.selectUpstreams("react")
	require.NoError(t, err)
	assert.Len(t, ups, 1)
	assert.Equal(t, "public", ups[0].Name)
}

func TestNpmSelectUpstreams_PrivateWithUpstream(t *testing.T) {
	privUpstream := upstream.Upstream{Name: "private", BaseURL: "https://private.registry"}
	h := NewHandler(newNpmTestCache(),
		WithPrivateScopes([]string{"@corp"}),
		WithPrivateUpstream(privUpstream),
	)
	ups, err := h.selectUpstreams("@corp/sdk")
	require.NoError(t, err)
	assert.Len(t, ups, 1)
	assert.Equal(t, "private", ups[0].Name)
}
