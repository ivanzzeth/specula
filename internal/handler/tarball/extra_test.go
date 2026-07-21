package tarball

// extra_test.go — targeted coverage for tarball option functions, fetchFromURL
// non-404 non-2xx path, and serveFromCache error paths.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
	"github.com/ivanzzeth/specula/internal/store/meta"
	"github.com/ivanzzeth/specula/internal/upstream"
)

// ── fakeMetaStoreTarball — minimal MetadataStore stub ─────────────────────────

type fakeMetaStoreTarball struct{}

var _ meta.MetadataStore = (*fakeMetaStoreTarball)(nil)

func (f *fakeMetaStoreTarball) Get(_ context.Context, _ artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	return nil, nil
}
func (f *fakeMetaStoreTarball) Put(_ context.Context, _ artifact.CacheEntry) error     { return nil }
func (f *fakeMetaStoreTarball) Delete(_ context.Context, _ artifact.ArtifactRef) error { return nil }
func (f *fakeMetaStoreTarball) GetMutable(_ context.Context, _ string) (*artifact.MutableEntry, error) {
	return nil, nil
}
func (f *fakeMetaStoreTarball) PutMutable(_ context.Context, _ artifact.MutableEntry) error {
	return nil
}
func (f *fakeMetaStoreTarball) DeleteMutable(_ context.Context, _ string) error { return nil }
func (f *fakeMetaStoreTarball) CacheSizeByProtocol(_ context.Context) (map[string]artifact.SizeStat, error) {
	return nil, nil
}

func (f *fakeMetaStoreTarball) CacheSizeByOrigin(_ context.Context) (map[string]artifact.SizeStat, error) {
	return map[string]artifact.SizeStat{}, nil
}
func (f *fakeMetaStoreTarball) ListEntries(_ context.Context, _ string, _ meta.EntryFilter, _ meta.Page) (meta.EntryPage, error) {
	return meta.EntryPage{}, nil
}
func (f *fakeMetaStoreTarball) SetPinned(_ context.Context, _ artifact.ArtifactRef, _ bool) error {
	return nil
}

// ── errServeTarballCacheManager — Serve returns an error ─────────────────────

type errServeTarballCacheManager struct {
	storingFakeCache
	serveErr error
}

func (e *errServeTarballCacheManager) Serve(_ context.Context, _ artifact.ArtifactRef, _, _ int64) (io.ReadCloser, *artifact.CacheEntry, error) {
	return nil, nil, e.serveErr
}

// ── nilRCTarballCacheManager — Serve returns (nil, nil, nil) ─────────────────

type nilRCTarballCacheManager struct {
	storingFakeCache
}

func (n *nilRCTarballCacheManager) Serve(_ context.Context, _ artifact.ArtifactRef, _, _ int64) (io.ReadCloser, *artifact.CacheEntry, error) {
	return nil, nil, nil
}

// ── lookupErrTarballCacheManager — Lookup returns an error ───────────────────

type lookupErrTarballCacheManager struct {
	storingFakeCache
}

func (l *lookupErrTarballCacheManager) Lookup(_ context.Context, _ artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	return nil, errors.New("lookup error injected for tarball test")
}

// ── Tests: option functions ────────────────────────────────────────────────────

func TestTarballWithMeta(t *testing.T) {
	ms := &fakeMetaStoreTarball{}
	h := NewHandler(&fakeCacheManager{}, WithMeta(ms))
	assert.Same(t, ms, h.meta)
}

func TestTarballWithUpstream(t *testing.T) {
	h := NewHandler(&fakeCacheManager{},
		WithUpstream(upstream.NewClient(), []upstream.Upstream{{Name: "mirror", BaseURL: "http://example.com"}}),
	)
	assert.NotNil(t, h.upstreamClt)
	assert.Len(t, h.upstreams, 1)
}

func TestTarballWithMutableTTL(t *testing.T) {
	h := NewHandler(&fakeCacheManager{}, WithMutableTTL(300))
	assert.Equal(t, int64(300), h.mutableTTLSec)
}

func TestTarballWithLogger(t *testing.T) {
	h := NewHandler(&fakeCacheManager{}, WithLogger(slog.Default()))
	assert.NotNil(t, h)
}

// ── Tests: fetchFromURL non-404 non-2xx → error ───────────────────────────────

func TestFetchFromURL_ServerError_503(t *testing.T) {
	// Upstream returns 503 → fetchFromURL must return an error with the status.
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}))
	defer badSrv.Close()

	host := badSrv.Listener.Addr().String()
	h := NewHandler(newStoringFakeCache(),
		WithAllowedHosts([]string{host}),
		WithScheme("http"),
		WithQuarantineDir(t.TempDir()),
	)

	_, _, err := h.fetchFromURL(context.Background(), host+"/files", "pkg.tar.gz")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "503")
}

func TestFetchFromURL_ServerError_ViaHTTP(t *testing.T) {
	// Upstream returns 500 → handler returns 502.
	fivehundredSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer fivehundredSrv.Close()

	host := fivehundredSrv.Listener.Addr().String()
	h := NewHandler(newStoringFakeCache(),
		WithAllowedHosts([]string{host}),
		WithScheme("http"),
		WithQuarantineDir(t.TempDir()),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/" + host + "/dir/pkg.tar.gz")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode,
		"non-2xx non-404 upstream error must produce 502")
}

// ── Tests: serveFromCache error paths ─────────────────────────────────────────

func TestTarballServeFromCache_ErrCacheMiss_404(t *testing.T) {
	cm := &errServeTarballCacheManager{
		storingFakeCache: *newStoringFakeCache(),
		serveErr:         cache.ErrCacheMiss,
	}
	ref := tarballRef("example.com/files", "hello.tar.gz")
	data := []byte("tarball content")
	digest := sha256sum(data)
	cm.blobs[digest] = data
	cm.entries[cacheKey(ref)] = &artifact.CacheEntry{
		Ref: ref, Digest: digest, Size: int64(len(data)), Protocol: Protocol,
	}

	h := NewHandler(cm, WithAllowedHosts([]string{"example.com"}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/example.com/files/hello.tar.gz")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestTarballServeFromCache_InternalError_500(t *testing.T) {
	cm := &errServeTarballCacheManager{
		storingFakeCache: *newStoringFakeCache(),
		serveErr:         errors.New("I/O error"),
	}
	ref := tarballRef("example.com/files", "hello.tar.gz")
	data := []byte("tarball content")
	digest := sha256sum(data)
	cm.blobs[digest] = data
	cm.entries[cacheKey(ref)] = &artifact.CacheEntry{
		Ref: ref, Digest: digest, Size: int64(len(data)), Protocol: Protocol,
	}

	h := NewHandler(cm, WithAllowedHosts([]string{"example.com"}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/example.com/files/hello.tar.gz")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestTarballServeFromCache_NilRC_404(t *testing.T) {
	cm := &nilRCTarballCacheManager{storingFakeCache: *newStoringFakeCache()}
	ref := tarballRef("example.com/files", "hello.tar.gz")
	data := []byte("tarball content")
	digest := sha256sum(data)
	cm.blobs[digest] = data
	cm.entries[cacheKey(ref)] = &artifact.CacheEntry{
		Ref: ref, Digest: digest, Size: int64(len(data)), Protocol: Protocol,
	}

	h := NewHandler(cm, WithAllowedHosts([]string{"example.com"}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/example.com/files/hello.tar.gz")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ── Tests: serveTarball Lookup error → 500 ────────────────────────────────────

func TestTarballLookupError_500(t *testing.T) {
	cm := &lookupErrTarballCacheManager{storingFakeCache: *newStoringFakeCache()}
	h := NewHandler(cm, WithAllowedHosts([]string{"example.com"}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/example.com/files/pkg.tar.gz")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

// ── Tests: serveTarball quarantine error → 500 ────────────────────────────────

func TestTarballQuarantineError_500(t *testing.T) {
	// Quarantine dir doesn't exist → os.CreateTemp fails → 500.
	content := bytes.Repeat([]byte("data"), 32)
	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	defer fakeUpstream.Close()

	host := fakeUpstream.Listener.Addr().String()
	h := NewHandler(newStoringFakeCache(),
		WithAllowedHosts([]string{host}),
		WithScheme("http"),
		WithQuarantineDir("/nonexistent/path/does/not/exist"),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/" + host + "/files/pkg.tar.gz")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode,
		"quarantine failure must produce 500")
}
