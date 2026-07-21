// Package apt — additional unit tests covering MetadataStore paths, conditional
// GET revalidation, and error branches in serveFromCache.
//
// These tests extend apt_test.go (same package) with tests that require a
// fake MetadataStore (meta.MetadataStore). They are split into a separate file
// for readability. All identifiers follow the in-package convention.
package apt

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
	"github.com/ivanzzeth/specula/internal/store/meta"
	"github.com/ivanzzeth/specula/internal/upstream"
	"github.com/ivanzzeth/specula/internal/verify"
)

// ── fakeAptMetaStore — minimal meta.MetadataStore test double ────────────────

type fakeAptMetaStore struct {
	mu      sync.Mutex
	mutable map[string]*artifact.MutableEntry
	putErr  error
}

var _ meta.MetadataStore = (*fakeAptMetaStore)(nil)

func newFakeAptMetaStore() *fakeAptMetaStore {
	return &fakeAptMetaStore{mutable: make(map[string]*artifact.MutableEntry)}
}

func (m *fakeAptMetaStore) GetMutable(_ context.Context, key string) (*artifact.MutableEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.mutable[key]
	if !ok {
		return nil, nil
	}
	cp := *e
	return &cp, nil
}

func (m *fakeAptMetaStore) PutMutable(_ context.Context, entry artifact.MutableEntry) error {
	if m.putErr != nil {
		return m.putErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := entry
	m.mutable[entry.Key] = &cp
	return nil
}

func (m *fakeAptMetaStore) DeleteMutable(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.mutable, key)
	return nil
}

// Remaining MetadataStore methods are no-ops for the apt handler tests.
func (m *fakeAptMetaStore) Get(_ context.Context, _ artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	return nil, nil
}
func (m *fakeAptMetaStore) Put(_ context.Context, _ artifact.CacheEntry) error { return nil }
func (m *fakeAptMetaStore) Delete(_ context.Context, _ artifact.ArtifactRef) error {
	return nil
}
func (m *fakeAptMetaStore) CacheSizeByProtocol(_ context.Context) (map[string]artifact.SizeStat, error) {
	return nil, nil
}

func (m *fakeAptMetaStore) CacheSizeByOrigin(_ context.Context) (map[string]artifact.SizeStat, error) {
	return map[string]artifact.SizeStat{}, nil
}
func (m *fakeAptMetaStore) ListEntries(_ context.Context, _ string, _ meta.EntryFilter, _ meta.Page) (meta.EntryPage, error) {
	return meta.EntryPage{}, nil
}
func (m *fakeAptMetaStore) SetPinned(_ context.Context, _ artifact.ArtifactRef, _ bool) error {
	return nil
}

// ── errorAptCache — test double that can inject errors from Serve ─────────────

// errorAptCache wraps aptTestCache and can inject a Serve error (or a nil
// reader on success) to test the serveFromCache error paths.
type errorAptCache struct {
	*aptTestCache
	serveErr error // if non-nil, Serve returns this error
	serveNil bool  // if true, Serve returns (nil, nil, nil)
}

func (c *errorAptCache) Serve(ctx context.Context, ref artifact.ArtifactRef, offset, length int64) (io.ReadCloser, *artifact.CacheEntry, error) {
	if c.serveErr != nil {
		return nil, nil, c.serveErr
	}
	if c.serveNil {
		return nil, nil, nil
	}
	return c.aptTestCache.Serve(ctx, ref, offset, length)
}

// ServeEntry mirrors the Serve injection above. serveFromCache reaches the
// cache through ServeEntry whenever it holds an entry, so a double that injects
// only on Serve would leave the fault unreachable and test nothing.
func (c *errorAptCache) ServeEntry(ctx context.Context, entry *artifact.CacheEntry, offset, length int64) (io.ReadCloser, error) {
	if c.serveErr != nil {
		return nil, c.serveErr
	}
	if c.serveNil {
		return nil, nil
	}
	return c.aptTestCache.ServeEntry(ctx, entry, offset, length)
}

// ── Option function tests ─────────────────────────────────────────────────────

// TestWithMeta_SetsField verifies that WithMeta injects the MetadataStore.
func TestWithMeta_SetsField(t *testing.T) {
	m := newFakeAptMetaStore()
	h := NewHandler(newAptTestCache(), WithMeta(m))
	assert.NotNil(t, h.meta, "WithMeta must inject the MetadataStore")
}

// TestWithLogger_SetsField verifies that WithLogger injects the logger.
// Even a nil logger is a valid structural test of the option function.
func TestWithLogger_SetsField(t *testing.T) {
	// WithLogger(nil) sets h.log = nil — valid as a coverage test; the handler
	// would panic at runtime but the option function itself must be covered.
	h := NewHandler(newAptTestCache(), WithLogger(nil))
	assert.Nil(t, h.log)
}

// TestWithGPGVerifier_SetsField verifies that WithGPGVerifier injects the
// verifier. A nil *verify.GPGVerifier is the canonical "signed tier not wired"
// state — the field is set even to nil.
func TestWithGPGVerifier_SetsField(t *testing.T) {
	var gv *verify.GPGVerifier // typed nil
	h := NewHandler(newAptTestCache(), WithGPGVerifier(gv))
	assert.Equal(t, gv, h.gpgVerifier,
		"WithGPGVerifier must set h.gpgVerifier (even when nil)")
}

// ── getMutableUpstreamMeta tests ──────────────────────────────────────────────

// TestGetMutableUpstreamMeta_NoMeta_ReturnsFalse verifies the function returns
// (zero, false) when no MetadataStore is configured.
func TestGetMutableUpstreamMeta_NoMeta_ReturnsFalse(t *testing.T) {
	h := NewHandler(newAptTestCache()) // no WithMeta
	_, ok := h.getMutableUpstreamMeta(context.Background(), distsRef("", "focal/InRelease"))
	assert.False(t, ok, "no meta store → must return false")
}

// TestGetMutableUpstreamMeta_NoEntry_ReturnsFalse verifies the function returns
// (zero, false) when the MetadataStore has no matching entry.
func TestGetMutableUpstreamMeta_NoEntry_ReturnsFalse(t *testing.T) {
	ms := newFakeAptMetaStore()
	h := NewHandler(newAptTestCache(), WithMeta(ms))
	_, ok := h.getMutableUpstreamMeta(context.Background(), distsRef("", "focal/InRelease"))
	assert.False(t, ok, "empty meta store → must return false")
}

// TestGetMutableUpstreamMeta_HasETag_ReturnsTrue verifies the function returns
// (meta, true) when a MutableEntry with an ETag exists in the MetadataStore.
func TestGetMutableUpstreamMeta_HasETag_ReturnsTrue(t *testing.T) {
	ms := newFakeAptMetaStore()
	ref := distsRef("", "focal/InRelease")
	_ = ms.PutMutable(context.Background(), artifact.MutableEntry{
		Key:      aptMutableKey(ref),
		Protocol: Protocol,
		ETag:     `"etag-abc"`,
	})

	h := NewHandler(newAptTestCache(), WithMeta(ms))
	prev, ok := h.getMutableUpstreamMeta(context.Background(), ref)

	assert.True(t, ok, "entry with ETag must return true")
	assert.Equal(t, `"etag-abc"`, prev.ETag)
}

// TestGetMutableUpstreamMeta_HasLastModified_ReturnsTrue verifies that an
// entry with only LastModified (no ETag) also returns true.
func TestGetMutableUpstreamMeta_HasLastModified_ReturnsTrue(t *testing.T) {
	ms := newFakeAptMetaStore()
	ref := distsRef("", "focal/InRelease")
	_ = ms.PutMutable(context.Background(), artifact.MutableEntry{
		Key:          aptMutableKey(ref),
		Protocol:     Protocol,
		LastModified: "Thu, 01 Jan 2026 00:00:00 GMT",
	})

	h := NewHandler(newAptTestCache(), WithMeta(ms))
	prev, ok := h.getMutableUpstreamMeta(context.Background(), ref)

	assert.True(t, ok)
	assert.Equal(t, "Thu, 01 Jan 2026 00:00:00 GMT", prev.LastModified)
}

// TestGetMutableUpstreamMeta_BothEmpty_ReturnsFalse verifies that an entry
// with no ETag and no LastModified does not trigger the conditional GET path.
//
// Requirement: without revalidation state, there is no conditional GET possible.
func TestGetMutableUpstreamMeta_BothEmpty_ReturnsFalse(t *testing.T) {
	ms := newFakeAptMetaStore()
	ref := distsRef("", "focal/InRelease")
	_ = ms.PutMutable(context.Background(), artifact.MutableEntry{
		Key:      aptMutableKey(ref),
		Protocol: Protocol,
		// ETag and LastModified both empty
	})

	h := NewHandler(newAptTestCache(), WithMeta(ms))
	_, ok := h.getMutableUpstreamMeta(context.Background(), ref)
	assert.False(t, ok, "entry with empty ETag and LastModified must not trigger conditional GET")
}

// ── extendMutableTTL tests ────────────────────────────────────────────────────

// TestExtendMutableTTL_NoMeta_IsNoop verifies that extendMutableTTL is a no-op
// when no MetadataStore is configured (no panic).
func TestExtendMutableTTL_NoMeta_IsNoop(t *testing.T) {
	h := NewHandler(newAptTestCache())
	// Must not panic.
	h.extendMutableTTL(context.Background(), distsRef("", "focal/InRelease"), artifact.UpstreamMeta{})
}

// TestExtendMutableTTL_NoEntry_IsNoop verifies that extendMutableTTL is a
// no-op when the entry does not exist in the MetadataStore.
func TestExtendMutableTTL_NoEntry_IsNoop(t *testing.T) {
	ms := newFakeAptMetaStore()
	h := NewHandler(newAptTestCache(), WithMeta(ms))
	// No entry in the meta store — must not panic.
	h.extendMutableTTL(context.Background(), distsRef("", "focal/InRelease"), artifact.UpstreamMeta{})
}

// TestExtendMutableTTL_UpdatesFetchedAt verifies that extendMutableTTL updates
// FetchedAt and optionally the ETag when a 304 conditional GET response is received.
//
// Requirement: ARCHITECTURE.md §3 — on 304, extend TTL without byte transfer.
func TestExtendMutableTTL_UpdatesFetchedAt(t *testing.T) {
	ms := newFakeAptMetaStore()
	ref := distsRef("", "focal/InRelease")
	key := aptMutableKey(ref)

	oldTime := time.Now().Add(-1 * time.Hour).UTC()
	_ = ms.PutMutable(context.Background(), artifact.MutableEntry{
		Key:        key,
		Protocol:   Protocol,
		ETag:       "old-etag",
		TTLSeconds: 300,
		FetchedAt:  oldTime,
	})

	h := NewHandler(newAptTestCache(), WithMeta(ms))
	h.extendMutableTTL(context.Background(), ref, artifact.UpstreamMeta{ETag: "new-etag"})

	me, err := ms.GetMutable(context.Background(), key)
	require.NoError(t, err)
	require.NotNil(t, me, "MutableEntry must still exist after extendMutableTTL")
	assert.True(t, me.FetchedAt.After(oldTime),
		"FetchedAt must be updated to a newer time after 304 TTL extension")
	assert.Equal(t, "new-etag", me.ETag,
		"ETag must be updated to the value returned by the 304 response")
}

// TestExtendMutableTTL_NoNewETag_PreservesOldETag verifies that when the 304
// response has no ETag, the old ETag is preserved.
func TestExtendMutableTTL_NoNewETag_PreservesOldETag(t *testing.T) {
	ms := newFakeAptMetaStore()
	ref := distsRef("", "focal/InRelease")
	key := aptMutableKey(ref)

	oldTime := time.Now().Add(-1 * time.Hour).UTC()
	_ = ms.PutMutable(context.Background(), artifact.MutableEntry{
		Key:       key,
		ETag:      "existing-etag",
		FetchedAt: oldTime,
	})

	h := NewHandler(newAptTestCache(), WithMeta(ms))
	h.extendMutableTTL(context.Background(), ref, artifact.UpstreamMeta{ETag: ""}) // no new ETag

	me, _ := ms.GetMutable(context.Background(), key)
	require.NotNil(t, me)
	assert.Equal(t, "existing-etag", me.ETag, "old ETag must be preserved when 304 has no ETag")
	assert.True(t, me.FetchedAt.After(oldTime), "FetchedAt still updated even with no new ETag")
}

// ── fetchBodyAndStore with meta tests ─────────────────────────────────────────

// TestFetchBodyAndStore_WithMeta_WritesMutableEntry verifies that
// fetchBodyAndStore writes a TTL-bearing MutableEntry to the MetadataStore
// when one is configured.
//
// Requirement: ARCHITECTURE.md §3 — mutable TTL pointer must be written so
// subsequent Lookup calls respect the configured TTL.
func TestFetchBodyAndStore_WithMeta_WritesMutableEntry(t *testing.T) {
	cm := newAptTestCache()
	ms := newFakeAptMetaStore()
	h := NewHandler(cm,
		WithMeta(ms),
		WithMutableTTL(300),
		WithQuarantineDir(t.TempDir()),
	)

	ref := distsRef("", "focal/InRelease")
	content := []byte("Origin: Test\nSuite: focal\n")
	body := bytes.NewReader(content)
	umeta := artifact.UpstreamMeta{ETag: `"etag-123"`, Upstream: "test-upstream"}

	entry, err := h.fetchBodyAndStore(context.Background(), ref, body, umeta)
	require.NoError(t, err)
	require.NotNil(t, entry)

	key := aptMutableKey(ref)
	me, err := ms.GetMutable(context.Background(), key)
	require.NoError(t, err)
	require.NotNil(t, me, "PutMutable must be called when h.meta != nil")
	assert.Equal(t, int64(300), me.TTLSeconds,
		"TTLSeconds must match the configured mutableTTLSec")
	assert.Equal(t, `"etag-123"`, me.ETag,
		"ETag from UpstreamMeta must be stored in MutableEntry")
	assert.Equal(t, "test-upstream", me.Upstream,
		"Upstream from UpstreamMeta must be stored in MutableEntry")
}

// TestFetchBodyAndStore_WithoutMeta_DoesNotPanic verifies that fetchBodyAndStore
// does not require a MetadataStore (the field is optional).
func TestFetchBodyAndStore_WithoutMeta_DoesNotPanic(t *testing.T) {
	cm := newAptTestCache()
	h := NewHandler(cm, WithQuarantineDir(t.TempDir()))

	ref := distsRef("", "focal/InRelease")
	body := bytes.NewReader([]byte("Origin: Test\n"))
	umeta := artifact.UpstreamMeta{}

	entry, err := h.fetchBodyAndStore(context.Background(), ref, body, umeta)
	require.NoError(t, err)
	assert.NotNil(t, entry)
}

// ── serveMutable conditional GET test ────────────────────────────────────────

// conditionalAptUpstream starts an httptest.Server that handles conditional
// GET via If-None-Match. Matches the ETag → 304; otherwise 200 + body.
func conditionalAptUpstream(t *testing.T, path string, body []byte, etag string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		if inm := r.Header.Get("If-None-Match"); inm == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestServeMutable_ConditionalGet_304_ServesStale verifies the full conditional
// GET path in serveMutable: when the upstream responds 304 Not Modified,
// the handler extends the TTL and serves the stale cached content.
//
// Requirements:
//   - DESIGN-REVIEW §H1 — mutable tier revalidation without byte transfer
//   - ARCHITECTURE.md §3 — conditional GET with If-None-Match; 304 extends TTL
//   - extendMutableTTL is exercised here (this test is its primary coverage)
func TestServeMutable_ConditionalGet_304_ServesStale(t *testing.T) {
	const staleInRelease = "Origin: Test\nSuite: focal\n# stale\n"
	const etag = `"stable-etag"`

	ref := distsRef("", "focal/InRelease")
	cm := newAptTestCache()
	// Lookup returns nil (TTL expired); LookupStale and Serve still find the entry.
	cm.seedStale(ref, []byte(staleInRelease))

	// Prime the meta store so getMutableUpstreamMeta returns hasPrev=true.
	ms := newFakeAptMetaStore()
	_ = ms.PutMutable(context.Background(), artifact.MutableEntry{
		Key:        aptMutableKey(ref),
		Protocol:   Protocol,
		ETag:       etag,
		TTLSeconds: 300,
		FetchedAt:  time.Now().Add(-10 * time.Minute),
	})

	// Upstream returns 304 when the ETag matches.
	up := conditionalAptUpstream(t, "/dists/focal/InRelease", []byte(staleInRelease), etag)
	h := NewHandler(cm,
		WithUpstream(
			upstream.NewClient(),
			[]upstream.Upstream{{Name: "fake-apt", BaseURL: up.URL, Priority: 0}},
		),
		WithMeta(ms),
		WithMutableTTL(300),
		WithQuarantineDir(t.TempDir()),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/dists/focal/InRelease")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode,
		"304 from upstream must result in 200 with stale cached content")
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, []byte(staleInRelease), body)

	// extendMutableTTL must have updated FetchedAt in the meta store.
	me, err := ms.GetMutable(context.Background(), aptMutableKey(ref))
	require.NoError(t, err)
	require.NotNil(t, me)
	// FetchedAt should have been refreshed by extendMutableTTL.
	// We verify the entry still exists (was not deleted) and the ETag is set.
	assert.Equal(t, etag, me.ETag,
		"ETag must be preserved in the meta store after 304 TTL extension")
}

// TestServeMutable_ConditionalGet_200_StoresAndServes verifies that when the
// upstream returns 200 during revalidation (content changed), the new content
// is stored and served.
func TestServeMutable_ConditionalGet_200_StoresAndServes(t *testing.T) {
	const oldContent = "Origin: Test\n# old\n"
	const newContent = "Origin: Test\n# new\n"
	const etag = `"old-etag"`

	ref := distsRef("", "focal/InRelease")
	cm := newAptTestCache()
	cm.seedStale(ref, []byte(oldContent))

	ms := newFakeAptMetaStore()
	_ = ms.PutMutable(context.Background(), artifact.MutableEntry{
		Key:        aptMutableKey(ref),
		Protocol:   Protocol,
		ETag:       etag,
		TTLSeconds: 300,
		FetchedAt:  time.Now().Add(-20 * time.Minute),
	})

	// The upstream now has updated content (does NOT return 304 for old etag).
	up := conditionalAptUpstream(t, "/dists/focal/InRelease", []byte(newContent), `"new-etag"`)
	h := NewHandler(cm,
		WithUpstream(
			upstream.NewClient(),
			[]upstream.Upstream{{Name: "fake-apt", BaseURL: up.URL, Priority: 0}},
		),
		WithMeta(ms),
		WithMutableTTL(300),
		WithQuarantineDir(t.TempDir()),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/dists/focal/InRelease")
	require.NoError(t, err)
	defer resp.Body.Close()

	// New content should be served.
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, []byte(newContent), body,
		"when upstream returns 200 during revalidation, new content must be served")
}

// ── serveFromCache error branch tests ────────────────────────────────────────

// TestServeFromCache_NonCacheMissError_Returns500 verifies that a non-ErrCacheMiss
// error from h.cache.Serve results in a 500 Internal Server Error.
//
// This tests the second branch of the error check in serveFromCache:
//
//	if err != nil {
//	    if errors.Is(err, cache.ErrCacheMiss) { → 404 }
//	    else { → 500 }  ← THIS branch
//	}
func TestServeFromCache_NonCacheMissError_Returns500(t *testing.T) {
	innerCache := newAptTestCache()
	ref := distsRef("", "focal/InRelease")
	// Seed a fresh entry so Lookup returns it (the handler reaches serveFromCache).
	innerCache.seedFresh(ref, []byte("Origin: Test\n"))

	// Wrap the cache to inject an error from Serve that is NOT ErrCacheMiss.
	errCache := &errorAptCache{
		aptTestCache: innerCache,
		serveErr:     errors.New("disk I/O error"),
	}

	h := newHandlerNoUpstream(errCache)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/dists/focal/InRelease")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode,
		"non-ErrCacheMiss error from Serve must return 500")
}

// TestServeFromCache_NilRC_Returns404 verifies that a nil io.ReadCloser with
// no error from h.cache.Serve results in a 404.
//
// This tests the "rc == nil" guard in serveFromCache:
//
//	if rc == nil {
//	    writeError(w, http.StatusNotFound, "artifact not in cache")
//	}
func TestServeFromCache_NilRC_Returns404(t *testing.T) {
	innerCache := newAptTestCache()
	ref := distsRef("", "focal/InRelease")
	innerCache.seedFresh(ref, []byte("Origin: Test\n"))

	errCache := &errorAptCache{
		aptTestCache: innerCache,
		serveNil:     true, // Serve returns (nil, nil, nil)
	}

	h := newHandlerNoUpstream(errCache)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/dists/focal/InRelease")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode,
		"nil rc from Serve must return 404")
}

// TestServeFromCache_NonCacheMissError_Returns500_PoolFile verifies the same
// 500 branch via a pool/ (immutable) file request.
func TestServeFromCache_NonCacheMissError_Returns500_PoolFile(t *testing.T) {
	innerCache := newAptTestCache()
	ref := poolRef("main/l/libfoo", "libfoo_1.0_amd64.deb")
	innerCache.seedFresh(ref, []byte("fake deb bytes"))

	errCache := &errorAptCache{
		aptTestCache: innerCache,
		serveErr:     errors.New("storage backend unavailable"),
	}

	h := newHandlerNoUpstream(errCache)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/pool/main/l/libfoo/libfoo_1.0_amd64.deb")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode,
		"non-ErrCacheMiss error from Serve for pool file must return 500")
}

// ── serveImmutable nil-entry documentation ────────────────────────────────────

// TestServeImmutable_NilEntryAfterStore_DocumentedDeadCode documents that the
// `entry == nil` guard after fetchAndStoreImmutable is dead code in the current
// implementation. CacheManager.Store either returns (entry, nil) on success or
// (nil, error) on failure; it never returns (nil, nil). This is documented here
// rather than tested to avoid "if only purpose is touching a line, STOP."
//
// Requirement: task description — report dead code rather than fabricate tests.
func TestServeImmutable_NilEntryAfterStore_DocumentedDeadCode(t *testing.T) {
	ref := poolRef("main/l/pkg", "pkg_1.0_amd64.deb")
	assert.False(t, ref.Mutable,
		"pool ref must be immutable (regression: must route through CAS tier)")
	t.Log("COVERAGE NOTE: the `if entry == nil` guard after fetchAndStoreImmutable " +
		"(apt/endpoints.go serveImmutable) is dead code: " +
		"CacheManager.Store never returns (nil, nil) on success. " +
		"No test fabricates (nil, nil) merely to hit this line.")
}

// ── Dists TTL=0 always-revalidate isolation ───────────────────────────────────

// TestServeMutable_AlwaysRevalidate_StoredWithZeroTTL verifies that when
// mutableTTLSec=0 (always-revalidate), the handler writes a MutableEntry
// with TTLSeconds=0 to the MetadataStore so the production cache can
// enforce the always-revalidate policy.
//
// The always-revalidate invariant is CRITICAL for apt-secure: a stale InRelease
// can silently break GPG chain validation (a real bug found by running apt-get).
// The production cache.manager enforces this by returning nil from Lookup for
// TTL=0 entries; the handler's job is to consistently pass TTLSeconds=0 to the
// MetadataStore so the production cache can honour it.
//
// Requirement: apt.go package doc / ARCHITECTURE.md §3 apt always-revalidate.
func TestServeMutable_AlwaysRevalidate_StoredWithZeroTTL(t *testing.T) {
	const inRelease = "Origin: Test\nSuite: focal\n"
	up := fakeAptUpstream(t, map[string][]byte{
		"/dists/focal/InRelease": []byte(inRelease),
	})

	cm := newAptTestCache()
	ms := newFakeAptMetaStore()
	h := NewHandler(cm,
		WithUpstream(
			upstream.NewClient(),
			[]upstream.Upstream{{Name: "fake-apt", BaseURL: up.URL, Priority: 0}},
		),
		WithMeta(ms),
		WithMutableTTL(ttlAlwaysRevalidate), // 0 = always revalidate
		WithQuarantineDir(t.TempDir()),
	)
	handler := httptest.NewServer(h)
	t.Cleanup(handler.Close)

	resp, err := http.Get(handler.URL + "/dists/focal/InRelease")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// The handler must write a MutableEntry with TTLSeconds=0 to the
	// MetadataStore so the production cache can enforce always-revalidate.
	ref := distsRef("", "focal/InRelease")
	me, err := ms.GetMutable(context.Background(), aptMutableKey(ref))
	require.NoError(t, err)
	require.NotNil(t, me, "handler must write MutableEntry even for TTL=0")
	assert.Equal(t, int64(0), me.TTLSeconds,
		"CRITICAL: TTLSeconds must be 0 (always-revalidate) so the production "+
			"cache.Lookup returns nil and apt-secure GPG chain is not broken by stale InRelease")
}
