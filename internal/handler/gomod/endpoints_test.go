package gomod

// endpoints_test.go — additional coverage for gomod endpoints.go:
//   • gomodMutableKey
//   • fetchBodyAndStore with MetadataStore (covers h.meta path)
//   • getMutableUpstreamMeta paths
//   • extendMutableTTL paths
//   • serveFromCache error paths (ErrCacheMiss, internal error, nil rc)
//   • serveMutable: stale-serve paths, upstream-fail paths, store-fail path
//   • Option functions: WithMeta, WithLogger, WithSumDB, WithQuarantineDir

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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

// ── fakeMetaStore — minimal MetadataStore for gomod unit tests ───────────────

type fakeMetaStore struct {
	mu      sync.Mutex
	mutable map[string]*artifact.MutableEntry
}

func newFakeMetaStore() *fakeMetaStore {
	return &fakeMetaStore{mutable: make(map[string]*artifact.MutableEntry)}
}

var _ meta.MetadataStore = (*fakeMetaStore)(nil)

func (f *fakeMetaStore) Get(_ context.Context, _ artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	return nil, nil
}
func (f *fakeMetaStore) Put(_ context.Context, _ artifact.CacheEntry) error     { return nil }
func (f *fakeMetaStore) Delete(_ context.Context, _ artifact.ArtifactRef) error { return nil }

func (f *fakeMetaStore) GetMutable(_ context.Context, key string) (*artifact.MutableEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.mutable[key]
	if e == nil {
		return nil, nil
	}
	cp := *e
	return &cp, nil
}

func (f *fakeMetaStore) PutMutable(_ context.Context, entry artifact.MutableEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := entry
	f.mutable[entry.Key] = &cp
	return nil
}

func (f *fakeMetaStore) DeleteMutable(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.mutable, key)
	return nil
}

func (f *fakeMetaStore) CacheSizeByProtocol(_ context.Context) (map[string]artifact.SizeStat, error) {
	return nil, nil
}

func (f *fakeMetaStore) CacheSizeByOrigin(_ context.Context) (map[string]artifact.SizeStat, error) {
	return map[string]artifact.SizeStat{}, nil
}

func (f *fakeMetaStore) ListEntries(_ context.Context, _ string, _ meta.EntryFilter, _ meta.Page) (meta.EntryPage, error) {
	return meta.EntryPage{}, nil
}

func (f *fakeMetaStore) SetPinned(_ context.Context, _ artifact.ArtifactRef, _ bool) error {
	return nil
}

// ── errCacheManager — always returns an error from Serve ─────────────────────

type errCacheManager struct {
	gomodTestCache
	serveErr error
}

func (e *errCacheManager) Serve(_ context.Context, ref artifact.ArtifactRef, _, _ int64) (io.ReadCloser, *artifact.CacheEntry, error) {
	if e.serveErr != nil {
		return nil, nil, e.serveErr
	}
	return e.gomodTestCache.Serve(context.Background(), ref, 0, -1)
}

// ServeEntry mirrors the Serve injection: serveFromCache reaches the cache via
// ServeEntry whenever it holds an entry.
func (e *errCacheManager) ServeEntry(ctx context.Context, entry *artifact.CacheEntry, offset, length int64) (io.ReadCloser, error) {
	if e.serveErr != nil {
		return nil, e.serveErr
	}
	return e.gomodTestCache.ServeEntry(ctx, entry, offset, length)
}

// ── nil-rc cache manager — Serve returns (nil, nil, nil) ─────────────────────

type nilRCCacheManager struct {
	gomodTestCache
}

func (n *nilRCCacheManager) Serve(_ context.Context, _ artifact.ArtifactRef, _, _ int64) (io.ReadCloser, *artifact.CacheEntry, error) {
	return nil, nil, nil
}

// ServeEntry mirrors the nil-reader injection on the path serveFromCache uses.
func (n *nilRCCacheManager) ServeEntry(_ context.Context, _ *artifact.CacheEntry, _, _ int64) (io.ReadCloser, error) {
	return nil, nil
}

// ── lookup-error cache manager ────────────────────────────────────────────────

type lookupErrCacheManager struct {
	gomodTestCache
}

func (l *lookupErrCacheManager) Lookup(_ context.Context, _ artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	return nil, errors.New("lookup error injected")
}

// ── Tests: gomodMutableKey ────────────────────────────────────────────────────

func TestGomodMutableKey(t *testing.T) {
	ref := artifact.ArtifactRef{
		Protocol: Protocol,
		Name:     "github.com/foo/bar",
		Version:  listFile,
		Mutable:  true,
	}
	key := gomodMutableKey(ref)
	assert.Equal(t, "gomod:github.com/foo/bar:list", key)

	latRef := latestRef("github.com/baz/qux")
	latKey := gomodMutableKey(latRef)
	assert.Equal(t, "gomod:github.com/baz/qux:@latest", latKey)
}

// ── Tests: option functions ───────────────────────────────────────────────────

func TestWithMetaOption(t *testing.T) {
	ms := newFakeMetaStore()
	h := NewHandler(newGomodTestCache(), WithMeta(ms))
	assert.NotNil(t, h.meta)
}

func TestWithLoggerOption(t *testing.T) {
	// Merely verify the option doesn't panic. slog.Default() returns a valid logger.
	h := NewHandler(newGomodTestCache(), WithLogger(nil))
	// WithLogger(nil) sets h.log to nil; handler still constructs successfully.
	_ = h
}

func TestWithSumDBOption(t *testing.T) {
	s := NewSumDBHandler("https://sum.golang.org")
	h := NewHandler(newGomodTestCache(), WithSumDB(s))
	assert.NotNil(t, h.sumdb)
}

func TestWithQuarantineDirOption(t *testing.T) {
	dir := t.TempDir()
	h := NewHandler(newGomodTestCache(), WithQuarantineDir(dir))
	assert.Equal(t, dir, h.quarantineDir)
}

// ── Tests: getMutableUpstreamMeta ─────────────────────────────────────────────

func TestGetMutableUpstreamMeta_NoMeta(t *testing.T) {
	h := NewHandler(newGomodTestCache()) // meta = nil
	ref := listRef("github.com/foo/bar")
	_, ok := h.getMutableUpstreamMeta(context.Background(), ref)
	assert.False(t, ok, "getMutableUpstreamMeta must return false when no meta store")
}

func TestGetMutableUpstreamMeta_EntryNotPresent(t *testing.T) {
	ms := newFakeMetaStore()
	h := NewHandler(newGomodTestCache(), WithMeta(ms))
	ref := listRef("github.com/foo/bar")
	_, ok := h.getMutableUpstreamMeta(context.Background(), ref)
	assert.False(t, ok, "getMutableUpstreamMeta must return false when key not in store")
}

func TestGetMutableUpstreamMeta_EntryPresentNoETag(t *testing.T) {
	ms := newFakeMetaStore()
	ref := listRef("github.com/foo/bar")
	// Put an entry with no ETag/LastModified — should not return ok=true.
	_ = ms.PutMutable(context.Background(), artifact.MutableEntry{
		Key:      gomodMutableKey(ref),
		Protocol: Protocol,
	})
	h := NewHandler(newGomodTestCache(), WithMeta(ms))
	_, ok := h.getMutableUpstreamMeta(context.Background(), ref)
	assert.False(t, ok, "no ETag/LastModified must produce false")
}

func TestGetMutableUpstreamMeta_EntryWithETag(t *testing.T) {
	ms := newFakeMetaStore()
	ref := listRef("github.com/foo/bar")
	_ = ms.PutMutable(context.Background(), artifact.MutableEntry{
		Key:      gomodMutableKey(ref),
		Protocol: Protocol,
		ETag:     `"abc123"`,
		Upstream: "fake-mirror",
	})
	h := NewHandler(newGomodTestCache(), WithMeta(ms))
	umeta, ok := h.getMutableUpstreamMeta(context.Background(), ref)
	require.True(t, ok)
	assert.Equal(t, `"abc123"`, umeta.ETag)
	assert.Equal(t, "fake-mirror", umeta.Upstream)
}

// ── Tests: extendMutableTTL ───────────────────────────────────────────────────

func TestExtendMutableTTL_NoMeta_NoOp(t *testing.T) {
	// Must not panic when h.meta is nil.
	h := NewHandler(newGomodTestCache())
	h.extendMutableTTL(context.Background(), listRef("github.com/foo/bar"), artifact.UpstreamMeta{})
}

func TestExtendMutableTTL_EntryNotPresent_NoOp(t *testing.T) {
	ms := newFakeMetaStore()
	h := NewHandler(newGomodTestCache(), WithMeta(ms))
	// Should not panic or create a new entry.
	h.extendMutableTTL(context.Background(), listRef("github.com/foo/bar"), artifact.UpstreamMeta{ETag: `"new"`})
	_, err := ms.GetMutable(context.Background(), gomodMutableKey(listRef("github.com/foo/bar")))
	assert.NoError(t, err)
}

func TestExtendMutableTTL_UpdatesFetchedAt(t *testing.T) {
	ms := newFakeMetaStore()
	ref := listRef("github.com/foo/bar")
	before := time.Now().Add(-10 * time.Minute).UTC()
	_ = ms.PutMutable(context.Background(), artifact.MutableEntry{
		Key:       gomodMutableKey(ref),
		Protocol:  Protocol,
		ETag:      `"old"`,
		FetchedAt: before,
	})

	h := NewHandler(newGomodTestCache(), WithMeta(ms))
	h.extendMutableTTL(context.Background(), ref, artifact.UpstreamMeta{ETag: `"new"`})

	got, err := ms.GetMutable(context.Background(), gomodMutableKey(ref))
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, `"new"`, got.ETag, "ETag must be updated")
	assert.True(t, got.FetchedAt.After(before), "FetchedAt must be refreshed")
}

// ── Tests: fetchBodyAndStore with meta store ──────────────────────────────────

func TestFetchBodyAndStore_WritesMetaTTL(t *testing.T) {
	ms := newFakeMetaStore()
	cm := newGomodTestCache()
	tmp := t.TempDir()

	h := NewHandler(cm, WithMeta(ms), WithMutableTTL(120), WithQuarantineDir(tmp))
	ref := listRef("github.com/foo/bar")

	body := bytes.NewReader([]byte("v1.0.0\nv1.1.0\n"))
	umeta := artifact.UpstreamMeta{
		ETag:         `"etag-123"`,
		LastModified: "Thu, 01 Jan 2026 00:00:00 GMT",
		Upstream:     "fake-mirror",
	}

	entry, err := h.fetchBodyAndStore(context.Background(), ref, body, umeta)
	require.NoError(t, err)
	assert.NotNil(t, entry)

	// Meta store should have the TTL entry.
	got, err := ms.GetMutable(context.Background(), gomodMutableKey(ref))
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, int64(120), got.TTLSeconds)
	assert.Equal(t, `"etag-123"`, got.ETag)
	assert.Equal(t, "fake-mirror", got.Upstream)
}

// ── Tests: serveFromCache error paths ─────────────────────────────────────────

func TestServeFromCache_InternalServeError(t *testing.T) {
	// Serve returning a non-ErrCacheMiss error → 500.
	cm := &errCacheManager{
		gomodTestCache: *newGomodTestCache(),
		serveErr:       errors.New("disk I/O error"),
	}
	// Seed so Lookup succeeds (we want to reach Serve).
	ref := immutableRef("github.com/foo/bar", "v1.0.0.info")
	cm.seed(ref, []byte(`{"Version":"v1.0.0"}`))

	h := NewHandler(cm)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/github.com/foo/bar/@v/v1.0.0.info")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestServeFromCache_NilRC(t *testing.T) {
	// Serve returning (nil, nil, nil) → 404.
	cm := &nilRCCacheManager{gomodTestCache: *newGomodTestCache()}
	ref := immutableRef("github.com/foo/bar", "v1.0.0.info")
	cm.seed(ref, []byte(`{"Version":"v1.0.0"}`))

	h := NewHandler(cm)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/github.com/foo/bar/@v/v1.0.0.info")
	require.NoError(t, err)
	defer resp.Body.Close()
	// nil rc → 404
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestServeFromCache_ErrCacheMiss(t *testing.T) {
	// Serve returning ErrCacheMiss → 404.
	cm := &errCacheManager{
		gomodTestCache: *newGomodTestCache(),
		serveErr:       cache.ErrCacheMiss,
	}
	ref := immutableRef("github.com/foo/bar", "v1.0.0.info")
	cm.seed(ref, []byte(`{"Version":"v1.0.0"}`))

	h := NewHandler(cm)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/github.com/foo/bar/@v/v1.0.0.info")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ── Tests: serveMutable error paths ──────────────────────────────────────────

func TestServeMutable_CacheLookupError_500(t *testing.T) {
	cm := &lookupErrCacheManager{gomodTestCache: *newGomodTestCache()}

	h := NewHandler(cm)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/github.com/foo/bar/@v/list")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

func TestServeMutable_UpstreamFetchFail_NoStale_502(t *testing.T) {
	// Upstream returns error; no stale entry → 502.
	cm := newGomodTestCache()
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failSrv.Close()

	h := NewHandler(cm,
		WithUpstream(upstream.NewClient(), []upstream.Upstream{{Name: "fail", BaseURL: failSrv.URL}}),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/github.com/foo/bar/@v/list")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

// ── Tests: serveImmutable cache lookup error ──────────────────────────────────

func TestServeImmutable_LookupError_500(t *testing.T) {
	cm := &lookupErrCacheManager{gomodTestCache: *newGomodTestCache()}

	h := NewHandler(cm)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/github.com/foo/bar/@v/v1.0.0.info")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)
}

// ── Tests: unknown @v file extension → 404 ───────────────────────────────────

func TestServeModuleFile_UnknownExt_404(t *testing.T) {
	h := NewHandler(newGomodTestCache())
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/github.com/foo/bar/@v/v1.0.0.unknown")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ── Tests: serveList/serveLatest with invalid module path → 400 ──────────────

func TestServeList_InvalidModule_400(t *testing.T) {
	h := NewHandler(newGomodTestCache())
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/!!bad!!/@v/list")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestServeLatest_InvalidModule_400(t *testing.T) {
	h := NewHandler(newGomodTestCache())
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/!!bad!!/@latest")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ── Tests: versionFromFile ────────────────────────────────────────────────────

func TestVersionFromFile(t *testing.T) {
	tests := []struct {
		file    string
		wantVer string
		wantOK  bool
	}{
		{"v1.0.0.info", "v1.0.0", true},
		{"v1.2.3-beta.1.mod", "v1.2.3-beta.1", true},
		{"v0.0.1.zip", "v0.0.1", true},
		{"list", "", false},
		{"unknown", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			ver, ok := versionFromFile(tc.file)
			assert.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				assert.Equal(t, tc.wantVer, ver)
			}
		})
	}
}

// ── Tests: fetchAndStoreImmutable upstream 404 → 404 (BUG 1) ─────────────────

// TestFetchAndStoreImmutable_Upstream404_Returns404 pins the CORRECTED behaviour
// for BUG 1. This test previously asserted 502 (StatusBadGateway) and thereby
// ENSHRINED the bug: an upstream 404 is "this module/version does not exist",
// which the GOPROXY protocol requires be surfaced as 404/410 so the go client
// can resolve module-path boundaries. Flattening it to 502 aborts that walk.
// See TestGomodImmutable_* in immutable_status_test.go for the full 404/410/502
// distinction driven through the real upstream client.
func TestFetchAndStoreImmutable_Upstream404_Returns404(t *testing.T) {
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer failSrv.Close()

	cm := newGomodTestCache()
	h := newHandlerWithUpstream(cm, failSrv.URL)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/github.com/foo/bar/@v/v1.0.0.zip")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ── Tests: large module path with escaping ────────────────────────────────────

func TestGomodInfo_BadEscaping_400(t *testing.T) {
	h := NewHandler(newGomodTestCache())
	srv := httptest.NewServer(h)
	defer srv.Close()

	// "!!" is invalid bang-encoding.
	resp, err := http.Get(srv.URL + "/github.com/!!bad!!/@v/v1.0.0.info")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ── Tests: temporary quarantine dir ──────────────────────────────────────────

func TestFetchBodyAndStore_QuarantineDir(t *testing.T) {
	tmp := t.TempDir()
	cm := newGomodTestCache()
	h := NewHandler(cm, WithQuarantineDir(tmp), WithMutableTTL(60))

	ref := listRef("github.com/foo/bar")
	body := bytes.NewReader([]byte("v1.0.0\n"))
	entry, err := h.fetchBodyAndStore(context.Background(), ref, body, artifact.UpstreamMeta{})
	require.NoError(t, err)
	assert.NotNil(t, entry)

	// The quarantine file should be removed after Store.
	fis, _ := os.ReadDir(tmp)
	assert.Empty(t, fis, "quarantine file must be cleaned up after Store")
}

// ── Tests: serve-stale-on-upstream-failure (DESIGN-REVIEW §2 H1) ─────────────
//
// The mutable tier (@v/list, @latest) must serve TTL-expired content rather than
// fail when the upstream GOPROXY is unreachable, so `go mod download` survives a
// proxy outage (PRD §G5).

func TestServeMutable_UpstreamFetchFail_StaleServed(t *testing.T) {
	cm := newGomodTestCache()
	staleList := []byte("v1.0.0\nv1.1.0\n")
	cm.seedStale(listRef("github.com/foo/bar"), staleList)

	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer failSrv.Close()

	h := NewHandler(cm,
		WithUpstream(upstream.NewClient(), []upstream.Upstream{{Name: "fail", BaseURL: failSrv.URL}}),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/github.com/foo/bar/@v/list")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode,
		"stale @v/list MUST be served when upstream is down (DESIGN-REVIEW §2 H1)")
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, staleList, body, "the stale bytes themselves must be served")
}

func TestServeMutable_NoUpstreamConfigured_StaleServed(t *testing.T) {
	cm := newGomodTestCache()
	staleList := []byte("v2.0.0\n")
	cm.seedStale(listRef("github.com/baz/qux"), staleList)

	h := NewHandler(cm) // no upstream wired
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/github.com/baz/qux/@v/list")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode,
		"stale @v/list MUST be served when no upstream is configured")
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, staleList, body)
}
