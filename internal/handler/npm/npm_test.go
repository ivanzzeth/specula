package npm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
	"github.com/ivanzzeth/specula/internal/upstream"
)

// ── npmTestCache — test double for cache.CacheManager ────────────────────────
//
// Entries are keyed by "protocol:name:version", matching the real SQLite
// MetadataStore primary key. Store reads the quarantine file and removes it,
// matching production behaviour. Serve applies offset/length windowing.

type npmTestCache struct {
	mu      sync.Mutex
	entries map[string]*artifact.CacheEntry // cacheKey → CacheEntry
	blobs   map[string][]byte               // digest → bytes
}

var _ cache.CacheManager = (*npmTestCache)(nil)

func newNpmTestCache() *npmTestCache {
	return &npmTestCache{
		entries: make(map[string]*artifact.CacheEntry),
		blobs:   make(map[string][]byte),
	}
}

func (c *npmTestCache) entryKey(ref artifact.ArtifactRef) string {
	return ref.Protocol + ":" + ref.Name + ":" + ref.Version
}

func (c *npmTestCache) Lookup(_ context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[c.entryKey(ref)], nil
}

// Store reads the quarantine file, records bytes keyed by digest, and writes
// a CacheEntry. Removes art.Path (matching production CacheManager behaviour).
func (c *npmTestCache) Store(_ context.Context, ref artifact.ArtifactRef, art *artifact.Artifact) (*artifact.CacheEntry, error) {
	data, err := os.ReadFile(art.Path)
	if err != nil {
		return nil, fmt.Errorf("npmTestCache.Store: read %s: %w", art.Path, err)
	}
	_ = os.Remove(art.Path)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.blobs[art.Digest] = data
	entry := &artifact.CacheEntry{
		Ref:      ref,
		Digest:   art.Digest,
		Size:     art.Size,
		Protocol: ref.Protocol,
		Upstream: art.Meta.Upstream,
	}
	c.entries[c.entryKey(ref)] = entry
	return entry, nil
}

// Serve returns the blob bytes with offset/length windowing.
func (c *npmTestCache) Serve(_ context.Context, ref artifact.ArtifactRef, offset, length int64) (io.ReadCloser, *artifact.CacheEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[c.entryKey(ref)]
	if !ok {
		return nil, nil, cache.ErrCacheMiss
	}
	data, ok := c.blobs[entry.Digest]
	if !ok {
		return nil, nil, cache.ErrCacheMiss
	}

	total := int64(len(data))
	start := offset
	if start < 0 {
		start = 0
	}
	if start > total {
		start = total
	}
	end := total
	if length >= 0 && start+length < end {
		end = start + length
	}
	return io.NopCloser(bytes.NewReader(data[start:end])), entry, nil
}

// seed pre-populates the cache with content for cache-hit tests.
func (c *npmTestCache) seed(ref artifact.ArtifactRef, data []byte) string {
	digest := npmsha256sum(data)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blobs[digest] = data
	c.entries[c.entryKey(ref)] = &artifact.CacheEntry{
		Ref:      ref,
		Digest:   digest,
		Size:     int64(len(data)),
		Protocol: ref.Protocol,
	}
	return digest
}

// npmsha256sum returns "sha256:<hex>" for data.
func npmsha256sum(data []byte) string {
	h := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(h[:])
}

// ── fakeNpmRegistry — minimal npm registry httptest.Server ───────────────────
//
// Routes:
//
//	GET /<pkg>                  → packuments[pkg]          (application/json)
//	GET /<pkg>/-/<file>         → tarballs[file]           (application/octet-stream)
//
// Scoped packages work identically to unscoped ones — Go's HTTP server decodes
// %2F to / transparently so the path reaching the handler is @scope/pkg.
//
// An atomic counter per endpoint is exposed for cache-hit verification.
func fakeNpmRegistry(t *testing.T, packuments map[string][]byte, tarballs map[string][]byte) (*httptest.Server, *int64, *int64) {
	t.Helper()

	var packumentHits, tarballHits int64
	mux := http.NewServeMux()

	// Serve any path that ends without "/-/": treat rest as package name.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		rest := p[1:] // strip leading /

		// Tarball: /<pkg>/-/<file>
		if pkg, file, ok := splitTarball(rest); ok {
			_ = pkg
			data, found := tarballs[file]
			if !found {
				http.NotFound(w, r)
				return
			}
			_ = &tarballHits
			tarballHits++
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(data)
			return
		}

		// Packument: /<pkg>
		pkg := decodeScopedName(rest)
		data, found := packuments[pkg]
		if !found {
			http.NotFound(w, r)
			return
		}
		packumentHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))

	_ = mux
	t.Cleanup(srv.Close)
	return srv, &packumentHits, &tarballHits
}

// ── Helper: handler with fake upstream ───────────────────────────────────────

func newNpmHandlerWithUpstream(cm cache.CacheManager, upstreamURL string, opts ...Option) *Handler {
	return NewHandler(cm, append([]Option{
		WithUpstream(
			upstream.NewClient(),
			[]upstream.Upstream{{Name: "fake-npm", BaseURL: upstreamURL, Priority: 0}},
		),
		WithMutableTTL(300),
	}, opts...)...)
}

// ── Pure function tests ───────────────────────────────────────────────────────

func TestValidPackageName(t *testing.T) {
	tests := []struct {
		name string
		pkg  string
		want bool
	}{
		{"empty", "", false},
		{"dotdot", "../evil", false},
		{"unscoped_simple", "react", true},
		{"unscoped_with_dash", "my-pkg", true},
		{"scoped_ok", "@myorg/mypkg", true},
		{"scoped_extra_slash", "@myorg/pkg/sub", false}, // 2 slashes
		{"scoped_no_slash", "@scope", false},            // no slash after @
		{"unscoped_with_slash", "pkg/sub", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, validPackageName(tc.pkg))
		})
	}
}

func TestSplitTarball(t *testing.T) {
	tests := []struct {
		rest     string
		wantPkg  string
		wantFile string
		wantOK   bool
	}{
		{"react/-/react-18.2.0.tgz", "react", "react-18.2.0.tgz", true},
		{"@myorg/pkg/-/pkg-1.0.0.tgz", "@myorg/pkg", "pkg-1.0.0.tgz", true},
		{"react", "", "", false},                          // no /-/
		{"react/-/", "", "", false},                       // empty file
		{"/-/react-18.2.0.tgz", "", "", false},            // empty pkg
		{"react/-/foo/bar.tgz", "", "", false},            // slash in file
		{"react/-/a/-/b.tgz", "react/-/a", "b.tgz", true}, // last /-/ wins
	}
	for _, tc := range tests {
		t.Run(tc.rest, func(t *testing.T) {
			pkg, file, ok := splitTarball(tc.rest)
			assert.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				assert.Equal(t, tc.wantPkg, pkg)
				assert.Equal(t, tc.wantFile, file)
			}
		})
	}
}

func TestDecodeScopedName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"react", "react"},
		{"@myorg/pkg", "@myorg/pkg"},
		{"@myorg%2Fpkg", "@myorg/pkg"},
		{"@myorg%2fpkg", "@myorg/pkg"}, // lowercase %2f
		{"/react/", "react"},           // trimmed
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			assert.Equal(t, tc.want, decodeScopedName(tc.input))
		})
	}
}

func TestPackumentAndTarballRef(t *testing.T) {
	ref := packumentRef("react")
	assert.Equal(t, Protocol, ref.Protocol)
	assert.Equal(t, "react", ref.Name)
	assert.Equal(t, packumentVersion, ref.Version)
	assert.True(t, ref.Mutable)

	tref := tarballRef("react", "react-18.2.0.tgz")
	assert.Equal(t, Protocol, tref.Protocol)
	assert.Equal(t, "react", tref.Name)
	assert.Equal(t, "react-18.2.0.tgz", tref.Version)
	assert.False(t, tref.Mutable)

	scopedRef := packumentRef("@myorg/pkg")
	assert.Equal(t, "@myorg/pkg", scopedRef.Name)
	assert.True(t, scopedRef.Mutable)
}

// ── Routing / error code tests ────────────────────────────────────────────────

func TestMethodNotAllowed(t *testing.T) {
	h := NewHandler(newNpmTestCache())
	srv := httptest.NewServer(h)
	defer srv.Close()

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			req, _ := http.NewRequest(method, srv.URL+"/react", nil)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
			assert.Equal(t, "GET, HEAD", resp.Header.Get("Allow"))
		})
	}
}

func TestEmptyPath_NotFound(t *testing.T) {
	h := NewHandler(newNpmTestCache())
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestInvalidPackageName_NotFound(t *testing.T) {
	h := NewHandler(newNpmTestCache())
	srv := httptest.NewServer(h)
	defer srv.Close()

	// "../evil" normalises to nothing useful — validPackageName rejects it.
	resp, err := http.Get(srv.URL + "/..%2Fevil")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ── Packument cache-hit tests ─────────────────────────────────────────────────

func TestPackumentCacheHit_Unscoped(t *testing.T) {
	packument := []byte(`{"name":"react","dist-tags":{"latest":"18.2.0"},"versions":{}}`)
	ref := packumentRef("react")
	cm := newNpmTestCache()
	cm.seed(ref, packument)

	h := NewHandler(cm) // no upstream needed for cache hit
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/react")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, contentTypePackument, resp.Header.Get("Content-Type"))
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, packument, got)
}

func TestPackumentCacheHit_Scoped(t *testing.T) {
	pkg := "@myorg/utils"
	packument := []byte(`{"name":"@myorg/utils","dist-tags":{"latest":"1.0.0"},"versions":{}}`)
	ref := packumentRef(pkg)
	cm := newNpmTestCache()
	cm.seed(ref, packument)

	h := NewHandler(cm)
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Use the slash-separated scoped form; Go's HTTP decodes %2F → / transparently.
	resp, err := http.Get(srv.URL + "/@myorg/utils")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, packument, got)
}

// TestPackumentCacheHit_PercentEncoded verifies that a %2F-encoded scoped path
// reaches the handler as the decoded "@scope/pkg" form and hits the cache.
func TestPackumentCacheHit_PercentEncoded(t *testing.T) {
	pkg := "@corp/sdk"
	packument := []byte(`{"name":"@corp/sdk","dist-tags":{"latest":"2.0.0"},"versions":{}}`)
	ref := packumentRef(pkg)
	cm := newNpmTestCache()
	cm.seed(ref, packument)

	h := NewHandler(cm)
	srv := httptest.NewServer(h)
	defer srv.Close()

	// %2F in the URL: Go's HTTP decodes it to / before routing.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/@corp%2Fsdk", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, packument, got)
}

// ── Tarball cache-hit test ────────────────────────────────────────────────────

func TestTarballCacheHit(t *testing.T) {
	tgzData := bytes.Repeat([]byte("FAKE_TGZ"), 128)
	ref := tarballRef("react", "react-18.2.0.tgz")
	cm := newNpmTestCache()
	cm.seed(ref, tgzData)

	h := NewHandler(cm)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/react/-/react-18.2.0.tgz")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, contentTypeTarball, resp.Header.Get("Content-Type"))
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, tgzData, got)
}

// ── Cache-miss → fetch from upstream tests ────────────────────────────────────

func TestPackumentCacheMiss_FetchFromUpstream(t *testing.T) {
	packument := []byte(`{"name":"lodash","dist-tags":{"latest":"4.17.21"},"versions":{}}`)
	upstream, _, _ := fakeNpmRegistry(t, map[string][]byte{"lodash": packument}, nil)

	cm := newNpmTestCache()
	h := newNpmHandlerWithUpstream(cm, upstream.URL)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/lodash")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, packument, got)

	// Verify the entry is now cached.
	entry, err := cm.Lookup(context.Background(), packumentRef("lodash"))
	require.NoError(t, err)
	assert.NotNil(t, entry, "packument must be cached after upstream fetch")
}

func TestTarballCacheMiss_FetchFromUpstream(t *testing.T) {
	tgzData := bytes.Repeat([]byte("TGZ_BYTES"), 64)
	reg, _, _ := fakeNpmRegistry(t, nil, map[string][]byte{"react-18.2.0.tgz": tgzData})

	cm := newNpmTestCache()
	h := newNpmHandlerWithUpstream(cm, reg.URL)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/react/-/react-18.2.0.tgz")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, tgzData, got)

	// Verify the tarball is now cached.
	entry, err := cm.Lookup(context.Background(), tarballRef("react", "react-18.2.0.tgz"))
	require.NoError(t, err)
	assert.NotNil(t, entry, "tarball must be cached after upstream fetch")
}

// ── 404 without upstream ──────────────────────────────────────────────────────

func TestNotFound_NoUpstream(t *testing.T) {
	h := NewHandler(newNpmTestCache()) // empty cache, no upstream
	srv := httptest.NewServer(h)
	defer srv.Close()

	for _, path := range []string{
		"/react",
		"/@myorg/pkg",
		"/react/-/react-18.2.0.tgz",
	} {
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(srv.URL + path)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusNotFound, resp.StatusCode)
		})
	}
}

// ── HEAD method ───────────────────────────────────────────────────────────────

func TestHEAD_Packument(t *testing.T) {
	packument := []byte(`{"name":"react","dist-tags":{"latest":"18.2.0"},"versions":{}}`)
	cm := newNpmTestCache()
	cm.seed(packumentRef("react"), packument)

	h := NewHandler(cm)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodHead, srv.URL+"/react", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, contentTypePackument, resp.Header.Get("Content-Type"))
	// HEAD must return no body.
	body, _ := io.ReadAll(resp.Body)
	assert.Empty(t, body)
}

// ── PathPrefix option ─────────────────────────────────────────────────────────

func TestPathPrefix(t *testing.T) {
	packument := []byte(`{"name":"react","dist-tags":{"latest":"18.2.0"},"versions":{}}`)
	cm := newNpmTestCache()
	cm.seed(packumentRef("react"), packument)

	h := NewHandler(cm, WithPathPrefix("/npm"))
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Must succeed at prefixed path.
	resp, err := http.Get(srv.URL + "/npm/react")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, packument, got)
}

// ── Dependency-confusion guard ────────────────────────────────────────────────

func TestIsPrivatePkg(t *testing.T) {
	h := &Handler{
		privateScopes:   []string{"@corp", "@internal"},
		privateUnscoped: []string{"internal-lib", "secret-pkg"},
	}

	tests := []struct {
		pkg  string
		want bool
	}{
		{"react", false},
		{"lodash", false},
		{"internal-lib", true},
		{"secret-pkg", true},
		{"other-pkg", false},
		{"@corp/sdk", true},
		{"@internal/utils", true},
		{"@public/pkg", false},
		// Partial matches must not trigger.
		{"internal-lib-extra", false},
	}
	for _, tc := range tests {
		t.Run(tc.pkg, func(t *testing.T) {
			assert.Equal(t, tc.want, h.isPrivatePkg(tc.pkg))
		})
	}
}

func TestDepConfusion_PrivateScopeNoPrivateUpstream_FailClosed(t *testing.T) {
	// @corp/sdk is private, but no private upstream is configured.
	// With failClosed=true (default), this must return 502.
	cm := newNpmTestCache()
	h := NewHandler(cm,
		WithPrivateScopes([]string{"@corp"}),
		// Note: WithPrivateUpstream NOT called.
		WithFailClosed(true),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/@corp/sdk")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

func TestDepConfusion_PublicPackage_PrivateScopeConfigured(t *testing.T) {
	// "react" is NOT in any private scope → served from public upstream.
	packument := []byte(`{"name":"react","dist-tags":{"latest":"18.2.0"},"versions":{}}`)
	reg, _, _ := fakeNpmRegistry(t, map[string][]byte{"react": packument}, nil)

	cm := newNpmTestCache()
	h := newNpmHandlerWithUpstream(cm, reg.URL,
		WithPrivateScopes([]string{"@corp"}),
		WithFailClosed(true),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/react")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, packument, got)
}

func TestDepConfusion_PrivateUnscoped_ServedFromPrivateUpstream(t *testing.T) {
	// "internal-lib" is private → only private upstream is queried.
	// The private upstream serves it; no public upstream should be used.
	internalPackument := []byte(`{"name":"internal-lib","dist-tags":{"latest":"1.0.0"},"versions":{}}`)
	privateReg, _, _ := fakeNpmRegistry(t, map[string][]byte{"internal-lib": internalPackument}, nil)

	cm := newNpmTestCache()
	h := NewHandler(cm,
		WithUpstream(
			upstream.NewClient(),
			// public mirror — should NOT be used for internal-lib
			[]upstream.Upstream{{Name: "public", BaseURL: "http://127.0.0.1:0", Priority: 0}},
		),
		WithPrivateUnscoped([]string{"internal-lib"}),
		WithPrivateUpstream(upstream.Upstream{Name: "private", BaseURL: privateReg.URL, Priority: 0}),
		WithMutableTTL(300),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/internal-lib")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, internalPackument, got)
}

// ── tests: wired guard + additional dep-confusion cases ──────────────────────

// TestDepConfusion_GuardWired verifies that NewHandler wires the
// DependencyConfusionGuard when scopes or unscoped names are configured.
func TestDepConfusion_GuardWired(t *testing.T) {
	h := NewHandler(newNpmTestCache(),
		WithPrivateScopes([]string{"@corp"}),
		WithPrivateUnscoped([]string{"internal-lib"}),
	)
	assert.NotNil(t, h.guard, "guard must be non-nil when private config is present")

	hNoPrivate := NewHandler(newNpmTestCache())
	assert.Nil(t, hNoPrivate.guard, "guard must be nil when no private config is set")
}

// TestDepConfusion_ScopeServedFromPrivateOnly verifies that a scoped private
// package is routed only to the private upstream and the public mirror receives
// no requests, even when it holds a "higher" version.
func TestDepConfusion_ScopeServedFromPrivateOnly(t *testing.T) {
	const pkg = "@corp/sdk"
	privatePackument := []byte(`{"name":"@corp/sdk","dist-tags":{"latest":"1.0.0"},"versions":{}}`)
	publicPackument := []byte(`{"name":"@corp/sdk","dist-tags":{"latest":"9.9.9"},"versions":{}}`)

	privateReg, _, _ := fakeNpmRegistry(t, map[string][]byte{pkg: privatePackument}, nil)

	var publicHits int64
	pubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&publicHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(publicPackument)
	}))
	defer pubSrv.Close()

	cm := newNpmTestCache()
	h := NewHandler(cm,
		WithPrivateScopes([]string{"@corp"}),
		WithPrivateUpstream(upstream.Upstream{Name: "private", BaseURL: privateReg.URL}),
		WithUpstream(upstream.NewClient(), []upstream.Upstream{{Name: "public", BaseURL: pubSrv.URL}}),
		WithMutableTTL(300),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/@corp/sdk")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, privatePackument, got, "response must be from private upstream")
	assert.Zero(t, atomic.LoadInt64(&publicHits), "public mirror must receive zero requests for @corp scoped pkg")
}

// TestDepConfusion_PrivateDown_FailClosed verifies that a private upstream that
// is unreachable results in 5xx and the public mirror is never consulted.
func TestDepConfusion_PrivateDown_FailClosed(t *testing.T) {
	const pkg = "acme-internal"
	publicPackument := []byte(`{"name":"acme-internal","dist-tags":{"latest":"2.0.0"},"versions":{}}`)

	// Public upstream is healthy and would serve the package.
	pubReg, _, _ := fakeNpmRegistry(t, map[string][]byte{pkg: publicPackument}, nil)

	cm := newNpmTestCache()
	h := NewHandler(cm,
		WithPrivateUnscoped([]string{pkg}),
		WithPrivateUpstream(upstream.Upstream{
			Name:    "private",
			BaseURL: "http://127.0.0.1:0", // nothing listening → refused
		}),
		WithUpstream(upstream.NewClient(), []upstream.Upstream{{Name: "public", BaseURL: pubReg.URL}}),
		WithFailClosed(true),
		WithMutableTTL(0),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/" + pkg)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.NotEqual(t, http.StatusOK, resp.StatusCode,
		"private-upstream-down must not return 200 from public (dep-confusion protection)")
	assert.GreaterOrEqual(t, resp.StatusCode, 500,
		"response must be a 5xx error when private upstream is down and failClosed=true")
}

// TestDepConfusion_NoPublicFallthrough_WithoutPrivateUpstream verifies the fix
// for the former bug where failClosed=false caused a public fallback for private
// names with no private upstream configured.
func TestDepConfusion_NoPublicFallthrough_WithoutPrivateUpstream(t *testing.T) {
	const pkg = "corp-secret"
	publicPackument := []byte(`{"name":"corp-secret","dist-tags":{"latest":"1.0.0"},"versions":{}}`)

	pubReg, _, _ := fakeNpmRegistry(t, map[string][]byte{pkg: publicPackument}, nil)

	cm := newNpmTestCache()
	h := NewHandler(cm,
		WithPrivateUnscoped([]string{pkg}),
		// No private upstream configured — previously caused public fallthrough.
		WithUpstream(upstream.NewClient(), []upstream.Upstream{{Name: "public", BaseURL: pubReg.URL}}),
		WithFailClosed(false), // the old buggy path required failClosed=false
		WithMutableTTL(0),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/" + pkg)
	require.NoError(t, err)
	defer resp.Body.Close()

	// selectUpstreams returns an error → must fail, NEVER return public 200.
	assert.NotEqual(t, http.StatusOK, resp.StatusCode,
		"private name without private upstream must NEVER fall through to public")
}

// TestDepConfusion_UnscopedGlob verifies that unscoped glob patterns in the
// private denylist are matched by the guard's IsPrivate logic.
func TestDepConfusion_UnscopedGlob(t *testing.T) {
	h := NewHandler(newNpmTestCache(),
		WithPrivateUnscoped([]string{"acme-*", "internal-lib"}),
	)
	require.NotNil(t, h.guard)

	tests := []struct {
		pkg  string
		want bool
	}{
		{"acme-core", true},
		{"acme-utils", true},
		{"acme-", true},        // path.Match("acme-*","acme-"): * matches empty string
		{"internal-lib", true}, // exact match
		{"react", false},
		{"@corp/pkg", false}, // scoped without a configured scope
	}
	for _, tc := range tests {
		t.Run(tc.pkg, func(t *testing.T) {
			got := h.isPrivatePkg(tc.pkg)
			assert.Equal(t, tc.want, got)
		})
	}
}
