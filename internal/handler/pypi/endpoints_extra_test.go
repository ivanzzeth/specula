package pypi

// endpoints_extra_test.go — coverage for pypi endpoints.go error paths,
// mutable-TTL helpers, option functions, and dependency-confusion guard branches
// not reached by the existing handler_test.go tests.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
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

// ── fakePypiMetaStore ─────────────────────────────────────────────────────────

type fakePypiMetaStore struct {
	mu      sync.Mutex
	mutable map[string]*artifact.MutableEntry
}

func newFakePypiMetaStore() *fakePypiMetaStore {
	return &fakePypiMetaStore{mutable: make(map[string]*artifact.MutableEntry)}
}

var _ meta.MetadataStore = (*fakePypiMetaStore)(nil)

func (f *fakePypiMetaStore) Get(_ context.Context, _ artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	return nil, nil
}
func (f *fakePypiMetaStore) Put(_ context.Context, _ artifact.CacheEntry) error     { return nil }
func (f *fakePypiMetaStore) Delete(_ context.Context, _ artifact.ArtifactRef) error { return nil }
func (f *fakePypiMetaStore) GetMutable(_ context.Context, key string) (*artifact.MutableEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.mutable[key]
	if e == nil {
		return nil, nil
	}
	cp := *e
	return &cp, nil
}
func (f *fakePypiMetaStore) PutMutable(_ context.Context, entry artifact.MutableEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := entry
	f.mutable[entry.Key] = &cp
	return nil
}
func (f *fakePypiMetaStore) DeleteMutable(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.mutable, key)
	return nil
}
func (f *fakePypiMetaStore) CacheSizeByProtocol(_ context.Context) (map[string]artifact.SizeStat, error) {
	return nil, nil
}

func (f *fakePypiMetaStore) CacheSizeByOrigin(_ context.Context) (map[string]artifact.SizeStat, error) {
	return map[string]artifact.SizeStat{}, nil
}
func (f *fakePypiMetaStore) ListEntries(_ context.Context, _ string, _ meta.EntryFilter, _ meta.Page) (meta.EntryPage, error) {
	return meta.EntryPage{}, nil
}
func (f *fakePypiMetaStore) SetPinned(_ context.Context, _ artifact.ArtifactRef, _ bool) error {
	return nil
}

// ── errPypiServeCacheManager — Serve always returns an error ──────────────────

type errPypiServeCacheManager struct {
	pypiTestCache
	serveErr error
}

func (e *errPypiServeCacheManager) Serve(_ context.Context, _ artifact.ArtifactRef, _, _ int64) (io.ReadCloser, *artifact.CacheEntry, error) {
	return nil, nil, e.serveErr
}

// ServeEntry mirrors the Serve injection: serveFromCache reaches the cache via
// ServeEntry whenever it holds an entry.
func (e *errPypiServeCacheManager) ServeEntry(_ context.Context, _ *artifact.CacheEntry, _, _ int64) (io.ReadCloser, error) {
	return nil, e.serveErr
}

// ── nilRCPypiCacheManager — Serve returns (nil, nil, nil) ─────────────────────

type nilRCPypiCacheManager struct {
	pypiTestCache
}

func (n *nilRCPypiCacheManager) Serve(_ context.Context, _ artifact.ArtifactRef, _, _ int64) (io.ReadCloser, *artifact.CacheEntry, error) {
	return nil, nil, nil
}

// ServeEntry mirrors the nil-reader injection on the path serveFromCache uses.
func (n *nilRCPypiCacheManager) ServeEntry(_ context.Context, _ *artifact.CacheEntry, _, _ int64) (io.ReadCloser, error) {
	return nil, nil
}

// ── lookupErrPypiCacheManager — Lookup returns error ─────────────────────────

type lookupErrPypiCacheManager struct {
	pypiTestCache
}

func (l *lookupErrPypiCacheManager) Lookup(_ context.Context, _ artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	return nil, errors.New("lookup error injected for pypi test")
}

// ── errPypiStoreCacheManager — Store always returns an error ──────────────────

type errPypiStoreCacheManager struct {
	pypiTestCache
}

func (e *errPypiStoreCacheManager) Store(_ context.Context, _ artifact.ArtifactRef, _ *artifact.Artifact) (*artifact.CacheEntry, error) {
	return nil, errors.New("simulated disk full on Store")
}

// ── errPypiPutMutableMetaStore — PutMutable always returns an error ───────────

type errPypiPutMutableMetaStore struct {
	fakePypiMetaStore
}

func (e *errPypiPutMutableMetaStore) PutMutable(_ context.Context, _ artifact.MutableEntry) error {
	return errors.New("simulated meta write error")
}

// ── Tests: pure function coverage ─────────────────────────────────────────────

func TestContentTypeForRequest_HTML(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/simple/flask/", nil)
	assert.Equal(t, ctSimpleHTML, contentTypeForRequest(r))
}

func TestContentTypeForRequest_JSON(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/simple/flask/", nil)
	r.Header.Set("Accept", ctSimpleJSON)
	assert.Equal(t, ctSimpleJSON, contentTypeForRequest(r))
}

func TestPypiMutableKey(t *testing.T) {
	ref := indexRef("flask")
	key := pypiMutableKey(ref)
	assert.Equal(t, "pypi:flask:simple", key)

	jsonRef := artifact.ArtifactRef{Protocol: Protocol, Name: "flask", Version: indexVersionJSON, Mutable: true}
	jsonKey := pypiMutableKey(jsonRef)
	assert.Equal(t, "pypi:flask:simple-json", jsonKey)
}

func TestNormalizeUpstreamBase_WithSimpleSuffix(t *testing.T) {
	assert.Equal(t, "https://pypi.org", normalizeUpstreamBase("https://pypi.org/simple"))
	assert.Equal(t, "https://pypi.tuna.tsinghua.edu.cn", normalizeUpstreamBase("https://pypi.tuna.tsinghua.edu.cn/simple"))
}

func TestNormalizeUpstreamBase_WithTrailingSlash(t *testing.T) {
	// Trailing slash stripped first, then /simple check.
	assert.Equal(t, "https://pypi.org", normalizeUpstreamBase("https://pypi.org/simple/"))
	// No /simple suffix — only trailing slash stripped.
	assert.Equal(t, "https://pypi.org", normalizeUpstreamBase("https://pypi.org/"))
}

func TestNormalizeUpstreamBase_NoSuffix(t *testing.T) {
	assert.Equal(t, "https://pypi.org", normalizeUpstreamBase("https://pypi.org"))
}

// ── Tests: option functions ───────────────────────────────────────────────────

func TestPypiWithMeta(t *testing.T) {
	ms := newFakePypiMetaStore()
	h := NewHandler(newPypiTestCache(), WithMeta(ms))
	assert.Same(t, ms, h.meta)
}

func TestPypiWithQuarantineDir(t *testing.T) {
	dir := t.TempDir()
	h := NewHandler(newPypiTestCache(), WithQuarantineDir(dir))
	assert.Equal(t, dir, h.quarantineDir)
}

func TestPypiWithLogger(t *testing.T) {
	h := NewHandler(newPypiTestCache(), WithLogger(slog.Default()))
	assert.NotNil(t, h)
}

// ── Tests: isPrivate inline fallback (guard == nil) ───────────────────────────

func TestIsPrivate_InlineFallback_NoGuard(t *testing.T) {
	// Directly construct a Handler with privateNames but no guard to exercise
	// the inline fallback path (guard == nil branch of isPrivate).
	h := &Handler{
		privateNames: []string{"mycompany-sdk", "internal-lib"},
		log:          slog.Default(),
	}
	assert.Nil(t, h.guard)

	assert.True(t, h.isPrivate("mycompany-sdk"))
	assert.True(t, h.isPrivate("internal-lib"))
	assert.False(t, h.isPrivate("flask"))
}

// ── Tests: privateDownServeStale (nil guard path) ─────────────────────────────

func TestPypiPrivateDownServeStale_NilGuard_FailClosed(t *testing.T) {
	h := NewHandler(newPypiTestCache()) // no private names → guard = nil, failClosed = true
	assert.False(t, h.privateDownServeStale())
}

func TestPypiPrivateDownServeStale_NilGuard_FailOpen(t *testing.T) {
	h := NewHandler(newPypiTestCache(), WithFailClosed(false))
	assert.True(t, h.privateDownServeStale())
}

// ── Tests: getMutableUpstreamMeta ─────────────────────────────────────────────

func TestPypiGetMutableUpstreamMeta_NilMeta(t *testing.T) {
	h := NewHandler(newPypiTestCache())
	_, ok := h.getMutableUpstreamMeta(context.Background(), indexRef("flask"))
	assert.False(t, ok)
}

func TestPypiGetMutableUpstreamMeta_NotPresent(t *testing.T) {
	ms := newFakePypiMetaStore()
	h := NewHandler(newPypiTestCache(), WithMeta(ms))
	_, ok := h.getMutableUpstreamMeta(context.Background(), indexRef("flask"))
	assert.False(t, ok)
}

func TestPypiGetMutableUpstreamMeta_NoETagOrLastModified(t *testing.T) {
	ms := newFakePypiMetaStore()
	ref := indexRef("flask")
	_ = ms.PutMutable(context.Background(), artifact.MutableEntry{
		Key:      pypiMutableKey(ref),
		Protocol: Protocol,
		// Neither ETag nor LastModified set
	})
	h := NewHandler(newPypiTestCache(), WithMeta(ms))
	_, ok := h.getMutableUpstreamMeta(context.Background(), ref)
	assert.False(t, ok, "entry without ETag/LastModified must return false")
}

func TestPypiGetMutableUpstreamMeta_WithETag(t *testing.T) {
	ms := newFakePypiMetaStore()
	ref := indexRef("flask")
	_ = ms.PutMutable(context.Background(), artifact.MutableEntry{
		Key:      pypiMutableKey(ref),
		Protocol: Protocol,
		ETag:     `"etag-abc"`,
		Upstream: "pypi-mirror",
	})
	h := NewHandler(newPypiTestCache(), WithMeta(ms))
	umeta, ok := h.getMutableUpstreamMeta(context.Background(), ref)
	require.True(t, ok)
	assert.Equal(t, `"etag-abc"`, umeta.ETag)
	assert.Equal(t, "pypi-mirror", umeta.Upstream)
}

// ── Tests: extendMutableTTL ───────────────────────────────────────────────────

func TestPypiExtendMutableTTL_NilMeta(t *testing.T) {
	h := NewHandler(newPypiTestCache())
	// Must not panic.
	h.extendMutableTTL(context.Background(), indexRef("flask"), artifact.UpstreamMeta{})
}

func TestPypiExtendMutableTTL_NotPresent(t *testing.T) {
	ms := newFakePypiMetaStore()
	h := NewHandler(newPypiTestCache(), WithMeta(ms))
	// Key is absent — must not panic or create entry.
	h.extendMutableTTL(context.Background(), indexRef("flask"), artifact.UpstreamMeta{ETag: `"new"`})
	got, _ := ms.GetMutable(context.Background(), pypiMutableKey(indexRef("flask")))
	assert.Nil(t, got, "extendMutableTTL must not create an absent entry")
}

func TestPypiExtendMutableTTL_UpdatesFetchedAtAndETag(t *testing.T) {
	ms := newFakePypiMetaStore()
	ref := indexRef("flask")
	before := time.Now().Add(-5 * time.Minute)
	_ = ms.PutMutable(context.Background(), artifact.MutableEntry{
		Key:       pypiMutableKey(ref),
		Protocol:  Protocol,
		ETag:      `"old-etag"`,
		FetchedAt: before,
	})

	h := NewHandler(newPypiTestCache(), WithMeta(ms))
	h.extendMutableTTL(context.Background(), ref, artifact.UpstreamMeta{ETag: `"new-etag"`})

	got, err := ms.GetMutable(context.Background(), pypiMutableKey(ref))
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, `"new-etag"`, got.ETag)
	assert.True(t, got.FetchedAt.After(before))
}

// ── Tests: fetchBodyAndStore error paths ──────────────────────────────────────

func TestPypiFetchBodyAndStore_QuarantineError(t *testing.T) {
	h := NewHandler(newPypiTestCache(),
		WithQuarantineDir("/nonexistent/path/that/does/not/exist"),
	)
	body := bytes.NewReader([]byte("<!DOCTYPE html>"))
	_, err := h.fetchBodyAndStore(context.Background(), indexRef("flask"), body, artifact.UpstreamMeta{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "quarantine")
}

func TestPypiFetchBodyAndStore_StoreError(t *testing.T) {
	cm := &errPypiStoreCacheManager{pypiTestCache: *newPypiTestCache()}
	h := NewHandler(cm, WithQuarantineDir(t.TempDir()))
	body := bytes.NewReader([]byte("<!DOCTYPE html>"))
	_, err := h.fetchBodyAndStore(context.Background(), indexRef("flask"), body, artifact.UpstreamMeta{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "store")
}

func TestPypiFetchBodyAndStore_PutMutableNonFatal(t *testing.T) {
	ms := &errPypiPutMutableMetaStore{fakePypiMetaStore: *newFakePypiMetaStore()}
	h := NewHandler(newPypiTestCache(),
		WithMeta(ms),
		WithMutableTTL(300),
		WithQuarantineDir(t.TempDir()),
	)
	body := bytes.NewReader([]byte("<!DOCTYPE html>"))
	entry, err := h.fetchBodyAndStore(context.Background(), indexRef("flask"), body, artifact.UpstreamMeta{ETag: `"abc"`})
	require.NoError(t, err, "PutMutable failure must be non-fatal")
	assert.NotNil(t, entry)
}

func TestPypiFetchBodyAndStore_WritesMeta(t *testing.T) {
	ms := newFakePypiMetaStore()
	h := NewHandler(newPypiTestCache(),
		WithMeta(ms),
		WithMutableTTL(600),
		WithQuarantineDir(t.TempDir()),
	)
	ref := indexRef("flask")
	body := bytes.NewReader([]byte("<!DOCTYPE html>"))
	entry, err := h.fetchBodyAndStore(context.Background(), ref, body, artifact.UpstreamMeta{ETag: `"abc"`, Upstream: "pypi-mirror"})
	require.NoError(t, err)
	assert.NotNil(t, entry)

	got, err2 := ms.GetMutable(context.Background(), pypiMutableKey(ref))
	require.NoError(t, err2)
	require.NotNil(t, got)
	assert.Equal(t, int64(600), got.TTLSeconds)
	assert.Equal(t, `"abc"`, got.ETag)
	assert.Equal(t, "pypi-mirror", got.Upstream)
}

// ── Tests: serveFromCache error paths ─────────────────────────────────────────

func TestPypiServeFromCache_ServeError_500(t *testing.T) {
	cm := &errPypiServeCacheManager{
		pypiTestCache: *newPypiTestCache(),
		serveErr:      errors.New("disk error"),
	}
	ref := indexRef("flask")
	cm.seed(ref, []byte("<!DOCTYPE html>"))

	h := NewHandler(cm)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/simple/flask/")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestPypiServeFromCache_ErrCacheMiss_404(t *testing.T) {
	cm := &errPypiServeCacheManager{
		pypiTestCache: *newPypiTestCache(),
		serveErr:      cache.ErrCacheMiss,
	}
	ref := indexRef("flask")
	cm.seed(ref, []byte("<!DOCTYPE html>"))

	h := NewHandler(cm)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/simple/flask/")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestPypiServeFromCache_NilRC_404(t *testing.T) {
	cm := &nilRCPypiCacheManager{pypiTestCache: *newPypiTestCache()}
	ref := indexRef("flask")
	cm.seed(ref, []byte("<!DOCTYPE html>"))

	h := NewHandler(cm)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/simple/flask/")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ── Tests: serveMutable error paths ──────────────────────────────────────────

func TestPypiServeMutable_LookupError_500(t *testing.T) {
	cm := &lookupErrPypiCacheManager{pypiTestCache: *newPypiTestCache()}
	h := NewHandler(cm)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/simple/flask/")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestPypiServeMutable_UpstreamDown_NoStale_502(t *testing.T) {
	h := NewHandler(newPypiTestCache(),
		WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "pypi", BaseURL: "http://127.0.0.1:0"}, // nothing listening
		}),
		WithMutableTTL(0),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/simple/flask/")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

func TestPypiServeMutable_StoreError_502(t *testing.T) {
	cm := &errPypiStoreCacheManager{pypiTestCache: *newPypiTestCache()}
	indexPage := []byte("<!DOCTYPE html><html><body></body></html>")
	upSrv := fakePyPIServer(t, map[string][]byte{"flask": indexPage}, nil)
	defer upSrv.Close()

	h := NewHandler(cm,
		WithUpstream(upstream.NewClient(), []upstream.Upstream{{Name: "pypi", BaseURL: upSrv.URL}}),
		WithMutableTTL(300),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/simple/flask/")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

// ── Tests: serveImmutable error paths ────────────────────────────────────────

func TestPypiServeImmutable_LookupError_500(t *testing.T) {
	cm := &lookupErrPypiCacheManager{pypiTestCache: *newPypiTestCache()}
	h := NewHandler(cm)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/packages/ab/cd/flask-2.0-py3-none-any.whl")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestPypiServeImmutable_Upstream404_NotFound(t *testing.T) {
	// Upstream always returns 404 → fetch fails → 404.
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer failSrv.Close()

	h := NewHandler(newPypiTestCache(),
		WithUpstream(upstream.NewClient(), []upstream.Upstream{{Name: "pypi", BaseURL: failSrv.URL}}),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/packages/ab/cd/flask-2.0-py3-none-any.whl")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestPypiServeImmutableFromCache_ErrCacheMiss_404(t *testing.T) {
	cm := &errPypiServeCacheManager{
		pypiTestCache: *newPypiTestCache(),
		serveErr:      cache.ErrCacheMiss,
	}
	ref := fileRef("ab/cd", "flask-2.0-py3-none-any.whl")
	cm.seed(ref, bytes.Repeat([]byte("WHL"), 32))

	h := NewHandler(cm)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/packages/ab/cd/flask-2.0-py3-none-any.whl")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestPypiServeImmutableFromCache_ServeError_500(t *testing.T) {
	cm := &errPypiServeCacheManager{
		pypiTestCache: *newPypiTestCache(),
		serveErr:      errors.New("I/O error"),
	}
	ref := fileRef("ab/cd", "flask-2.0-py3-none-any.whl")
	cm.seed(ref, bytes.Repeat([]byte("WHL"), 32))

	h := NewHandler(cm)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/packages/ab/cd/flask-2.0-py3-none-any.whl")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

// ── Tests: serveFile dep-confusion guard ──────────────────────────────────────

func TestPypiServeFile_PrivateName_NoPrivateUpstream_503(t *testing.T) {
	// Wheel filename "corp_internal-1.0.0-py3-none-any.whl":
	// extractProjectFromFile → project = "corp-internal" (PEP 427: underscore → hyphen normalised by PEP 503).
	// Private name, no private upstream → 503.
	h := NewHandler(newPypiTestCache(),
		WithPrivateNames([]string{"corp-internal"}),
		// No WithPrivateUpstream
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Use underscore in filename: PEP 427 says distribution name uses underscores,
	// and extractProjectFromFile splits on first '-', yielding "corp_internal" → normalised to "corp-internal".
	resp, err := http.Get(srv.URL + "/packages/ab/cd/corp_internal-1.0.0-py3-none-any.whl")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func TestPypiServeFile_PrivateName_RoutesToPrivateUpstream(t *testing.T) {
	// Private wheel → served from private upstream only.
	// Filename uses underscore (PEP 427): "corp_internal-1.0.0-py3-none-any.whl"
	// → extractProjectFromFile splits at first '-' → "corp_internal" → norm → "corp-internal" → private.
	whlBytes := bytes.Repeat([]byte("WHL_PRIVATE"), 16)
	privSrv := fakePyPIServer(t, nil, map[string][]byte{
		"corp_internal-1.0.0-py3-none-any.whl": whlBytes,
	})
	defer privSrv.Close()

	pubCallCount := 0
	pubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pubCallCount++
		http.NotFound(w, r)
	}))
	defer pubSrv.Close()

	h := NewHandler(newPypiTestCache(),
		WithPrivateNames([]string{"corp-internal"}),
		WithPrivateUpstream(upstream.Upstream{Name: "private", BaseURL: privSrv.URL}),
		WithUpstream(upstream.NewClient(), []upstream.Upstream{{Name: "public", BaseURL: pubSrv.URL}}),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/packages/ab/cd/corp_internal-1.0.0-py3-none-any.whl")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Zero(t, pubCallCount, "public upstream must not be contacted for private wheel")
}

// ── Tests: serveIndex not-a-simple-path ───────────────────────────────────────

func TestPypiServeHTTP_BadSimplePath_404(t *testing.T) {
	h := NewHandler(newPypiTestCache())
	srv := httptest.NewServer(h)
	defer srv.Close()

	// /simple/ with no project → 404
	resp, err := http.Get(srv.URL + "/simple/")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestPypiServeHTTP_BadPackagePath_404(t *testing.T) {
	h := NewHandler(newPypiTestCache())
	srv := httptest.NewServer(h)
	defer srv.Close()

	// /packages/ with no file component → 404
	resp, err := http.Get(srv.URL + "/packages/ab/")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ── Tests: serve-stale-on-upstream-failure (DESIGN-REVIEW §2 H1) ─────────────
//
// The mutable tier must serve a TTL-expired simple index rather than fail when
// the upstream is unreachable, so `pip install` survives a PyPI outage
// (PRD §G5; devpi behaves the same way).

func TestPypiServeMutable_UpstreamDown_StaleServed(t *testing.T) {
	cm := newPypiTestCache()
	staleIndex := []byte(`<!DOCTYPE html><html><body><a href="flask-2.0.tar.gz">flask-2.0.tar.gz</a></body></html>`)
	cm.seedStale(artifact.ArtifactRef{
		Protocol: Protocol, Name: "flask", Version: indexVersion, Mutable: true,
	}, staleIndex)

	h := NewHandler(cm,
		WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "pypi", BaseURL: "http://127.0.0.1:0"}, // nothing listening
		}),
		WithMutableTTL(300),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/simple/flask/")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode,
		"stale simple index MUST be served when upstream is down (DESIGN-REVIEW §2 H1)")
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, staleIndex, body, "the stale bytes themselves must be served")
}

func TestPypiServeMutable_NoUpstreamConfigured_StaleServed(t *testing.T) {
	cm := newPypiTestCache()
	staleIndex := []byte(`<!DOCTYPE html><html><body><a href="requests-2.0.tar.gz">requests-2.0.tar.gz</a></body></html>`)
	cm.seedStale(artifact.ArtifactRef{
		Protocol: Protocol, Name: "requests", Version: indexVersion, Mutable: true,
	}, staleIndex)

	h := NewHandler(cm, WithMutableTTL(300)) // no upstream wired
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/simple/requests/")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode,
		"stale simple index MUST be served when no upstream is configured")
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, staleIndex, body)
}
