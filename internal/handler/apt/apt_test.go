// Package apt — unit tests for the APT handler.
//
// All tests are in-package (package apt) so that routing helpers, ArtifactRef
// constructors, and content-type helpers can be called directly without export.
//
// # Requirements under test
//
//   - PRD §G2 / DESIGN-REVIEW §1.1: apt is the "offline gold standard" for the
//     signed tier (keyring → InRelease → Packages → .deb SHA256 chain).
//   - DESIGN-REVIEW §3 two-tier invariant:
//   - /dists/* MUST be the mutable tier (ArtifactRef.Mutable=true)
//   - /pool/*.deb MUST be the immutable CAS tier (ArtifactRef.Mutable=false)
//   - ARCHITECTURE.md §3 mutable TTL sentinel: InRelease has its own Valid-Until;
//     the handler default MUST be mutableTTLSec=0 (always-revalidate) to prevent
//     apt-secure's GPG chain from validating a stale, possibly-replaced index.
//   - ARCHITECTURE.md §4 verify-on-write / quarantine: VerifyError → 502 (never
//     promoted to CAS, never served).
//   - ARCHITECTURE.md §3 serve-stale-on-upstream-failure: stale dists/ content
//     is served when the upstream is unreachable.
//
// # Fidelity requirement for aptTestCache (previously a live production bug)
//
// serveFromCache used to discard the entry it was handed and re-resolve the ref
// through cache.Serve, which re-runs Lookup (no allowStale) and therefore
// returned ErrCacheMiss → 404 for exactly the stale entries the serve-stale path
// had just chosen to serve. The bug survived because this file's test double
// implemented Serve to fall back to staleEntries — something the production
// manager cannot do — so the stale tests passed against broken production code.
//
// aptTestCache.Serve therefore now mirrors manager.Serve exactly: fresh entries
// only, no stale fallback. Stale bytes are reachable solely via ServeEntry
// (cache.EntryServer), which is how the handler now serves an entry it holds.
// Keep it that way: a fake that serves stale from Serve makes these tests green
// against a 404-ing production build.
package apt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
	"github.com/ivanzzeth/specula/internal/upstream"
)

// ── aptTestCache — test double for cache.CacheManager ────────────────────────
//
// Implements cache.CacheManager and the staler interface (LookupStale).
// Keyed by "protocol:name:version".
// Serves stale entries from a separate map when fresh Lookup returns nil.
// Store reads the quarantine file and removes it, matching production behaviour.

type aptTestCache struct {
	mu           sync.Mutex
	entries      map[string]*artifact.CacheEntry // cacheKey → fresh entry
	staleEntries map[string]*artifact.CacheEntry // cacheKey → stale entry
	blobs        map[string][]byte               // digest → bytes
	storeErr     error                           // injected error returned by Store
}

var _ cache.CacheManager = (*aptTestCache)(nil)

// staler satisfied — the handler uses a type-assertion to opt-in.
var _ staler = (*aptTestCache)(nil)

func newAptTestCache() *aptTestCache {
	return &aptTestCache{
		entries:      make(map[string]*artifact.CacheEntry),
		staleEntries: make(map[string]*artifact.CacheEntry),
		blobs:        make(map[string][]byte),
	}
}

func (c *aptTestCache) cacheKey(ref artifact.ArtifactRef) string {
	return ref.Protocol + ":" + ref.Name + ":" + ref.Version
}

func (c *aptTestCache) Lookup(_ context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[c.cacheKey(ref)], nil
}

// LookupStale returns a stale entry when present, otherwise falls through to
// the fresh entries map. Satisfies the staler interface used by serveMutable.
func (c *aptTestCache) LookupStale(_ context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := c.cacheKey(ref)
	if e, ok := c.staleEntries[k]; ok {
		return e, nil
	}
	return c.entries[k], nil
}

// Store reads the quarantine file, stores bytes keyed by digest, and records a
// CacheEntry. Removes art.Path on success (matching production behaviour).
func (c *aptTestCache) Store(_ context.Context, ref artifact.ArtifactRef, art *artifact.Artifact) (*artifact.CacheEntry, error) {
	if c.storeErr != nil {
		_ = os.Remove(art.Path)
		return nil, c.storeErr
	}
	data, err := os.ReadFile(art.Path)
	if err != nil {
		return nil, fmt.Errorf("aptTestCache.Store: read quarantine %s: %w", art.Path, err)
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
	c.entries[c.cacheKey(ref)] = entry
	return entry, nil
}

// Serve mirrors production manager.Serve: it re-runs the FRESH Lookup and
// serves only what that returns. It deliberately does NOT fall back to
// staleEntries — the real manager cannot do so, because manager.Serve calls
// Lookup (no allowStale), which returns nil for a stale mutable entry. A fake
// that served stale here would report a passing serve-stale path that 404s in
// production. Stale bytes are reachable only via ServeEntry.
func (c *aptTestCache) Serve(_ context.Context, ref artifact.ArtifactRef, offset, length int64) (io.ReadCloser, *artifact.CacheEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[c.cacheKey(ref)]
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

// seedFresh pre-populates the cache with a fresh entry (Lookup returns it).
func (c *aptTestCache) seedFresh(ref artifact.ArtifactRef, data []byte) {
	digest := aptsha256sum(data)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blobs[digest] = data
	c.entries[c.cacheKey(ref)] = &artifact.CacheEntry{
		Ref:      ref,
		Digest:   digest,
		Size:     int64(len(data)),
		Protocol: ref.Protocol,
	}
}

// seedStale pre-populates only the stale map (Lookup returns nil; LookupStale
// and Serve still return the data).
func (c *aptTestCache) seedStale(ref artifact.ArtifactRef, data []byte) {
	digest := aptsha256sum(data)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blobs[digest] = data
	c.staleEntries[c.cacheKey(ref)] = &artifact.CacheEntry{
		Ref:      ref,
		Digest:   digest,
		Size:     int64(len(data)),
		Protocol: ref.Protocol,
	}
}

// aptsha256sum returns "sha256:<hex>" for data.
func aptsha256sum(data []byte) string {
	h := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(h[:])
}

// ── fakeAptUpstream — minimal APT upstream httptest.Server ───────────────────

// fakeAptUpstream creates an upstream that serves from a flat map of
// path → body. Unknown paths return 404.
func fakeAptUpstream(t *testing.T, routes map[string][]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// failingUpstream returns an httptest.Server that replies 500 to all requests.
func failingUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream error", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ── Handler constructors ──────────────────────────────────────────────────────

func newHandlerNoUpstream(cm cache.CacheManager, opts ...Option) *Handler {
	return NewHandler(cm, opts...)
}

func newHandlerWithFakeUpstream(cm cache.CacheManager, upstreamURL string, opts ...Option) *Handler {
	return NewHandler(cm, append([]Option{
		WithUpstream(
			upstream.NewClient(),
			[]upstream.Upstream{{Name: "fake-apt", BaseURL: upstreamURL, Priority: 0}},
		),
	}, opts...)...)
}

// ── Pure function tests ───────────────────────────────────────────────────────

// TestNewHandler_DefaultMutableTTL_AlwaysRevalidate asserts that the default
// mutableTTLSec is 0 (always-revalidate).
//
// Requirement: DESIGN-REVIEW §3 / ARCHITECTURE.md §3 — "apt defaults to 0
// (always revalidate). InRelease has its own Valid-Until field; a stale index
// breaks apt-secure's GPG chain — a real bug found by running real apt-get."
func TestNewHandler_DefaultMutableTTL_AlwaysRevalidate(t *testing.T) {
	h := NewHandler(newAptTestCache())
	assert.Equal(t, ttlAlwaysRevalidate, h.mutableTTLSec,
		"APT handler default mutableTTLSec MUST be 0 (always-revalidate) to prevent "+
			"stale InRelease from silently breaking the apt-secure GPG chain")
}

// TestDistsRef_IsMarkedMutable asserts that dists/ refs carry Mutable=true.
//
// Requirement: DESIGN-REVIEW §3 two-tier invariant — dists/InRelease is mutable.
func TestDistsRef_IsMarkedMutable(t *testing.T) {
	ref := distsRef("ubuntu", "focal/InRelease")
	assert.Equal(t, Protocol, ref.Protocol)
	assert.Equal(t, "ubuntu", ref.Name)
	assert.Equal(t, "focal/InRelease", ref.Version)
	assert.True(t, ref.Mutable,
		"dists/ ref MUST be Mutable=true (short-TTL mutable tier)")
}

// TestPoolRef_IsMarkedImmutable asserts that pool/ refs carry Mutable=false.
//
// Requirement: DESIGN-REVIEW §3 two-tier invariant — pool/*.deb is immutable CAS.
func TestPoolRef_IsMarkedImmutable(t *testing.T) {
	ref := poolRef("main/p", "pkg_1.0_amd64.deb")
	assert.Equal(t, Protocol, ref.Protocol)
	assert.Equal(t, "main/p", ref.Name)
	assert.Equal(t, "pkg_1.0_amd64.deb", ref.Version)
	assert.False(t, ref.Mutable,
		"pool/ ref MUST be Mutable=false (permanent immutable CAS tier)")
}

// TestCut exercises the cut() routing helper.
func TestCut(t *testing.T) {
	tests := []struct {
		desc      string
		p         string
		seg       string
		wantRepo  string
		wantAfter string
		wantOK    bool
	}{
		{"dists_at_root", "/dists/focal/InRelease", "/dists/", "", "focal/InRelease", true},
		{"dists_with_repo", "/ubuntu/dists/focal/InRelease", "/dists/", "ubuntu", "focal/InRelease", true},
		{"pool_at_root", "/pool/main/p/pkg.deb", "/pool/", "", "main/p/pkg.deb", true},
		{"pool_with_repo", "/ubuntu/pool/main/p/pkg.deb", "/pool/", "ubuntu", "main/p/pkg.deb", true},
		{"no_match", "/something/else", "/dists/", "", "", false},
		{"empty_after_seg", "/dists/", "/dists/", "", "", false},
		{"by_hash_path", "/dists/focal/by-hash/SHA256/abc123", "/dists/", "", "focal/by-hash/SHA256/abc123", true},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			repo, after, ok := cut(tc.p, tc.seg)
			assert.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				assert.Equal(t, tc.wantRepo, repo)
				assert.Equal(t, tc.wantAfter, after)
			}
		})
	}
}

// TestSplitDir exercises the splitDir() path-splitting helper.
func TestSplitDir(t *testing.T) {
	tests := []struct {
		desc     string
		path     string
		wantDir  string
		wantFile string
		wantOK   bool
	}{
		{"simple", "main/p/pkg.deb", "main/p", "pkg.deb", true},
		{"deep", "main/l/libfoo/libfoo_1.0_amd64.deb", "main/l/libfoo", "libfoo_1.0_amd64.deb", true},
		{"single_dir", "a/b.deb", "a", "b.deb", true},
		{"leading_slash_stripped", "/main/p/pkg.deb", "main/p", "pkg.deb", true},
		{"no_slash", "pkg.deb", "", "", false},
		{"trailing_slash", "main/p/", "", "", false},
		{"empty", "", "", "", false},
		{"only_slash", "/", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			dir, file, ok := splitDir(tc.path)
			assert.Equal(t, tc.wantOK, ok, "ok mismatch for %q", tc.path)
			if tc.wantOK {
				assert.Equal(t, tc.wantDir, dir)
				assert.Equal(t, tc.wantFile, file)
			}
		})
	}
}

// TestContentTypeForDistsPath exercises content-type detection for dists/ paths.
func TestContentTypeForDistsPath(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"focal/InRelease", "text/plain; charset=utf-8"},
		{"focal/Release", "text/plain; charset=utf-8"},
		{"focal/Release.gpg", "application/pgp-signature"},
		{"focal/main/binary-amd64/Packages.gz", "application/x-gzip"},
		{"focal/main/binary-amd64/Packages.xz", "application/x-xz"},
		{"focal/main/binary-amd64/Packages.bz2", "application/x-bzip2"},
		// By-hash path: content is the sha-named file, same type as Packages
		{"focal/main/binary-amd64/by-hash/SHA256/abcdef", "text/plain; charset=utf-8"},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			assert.Equal(t, tc.want, contentTypeForDistsPath(tc.path))
		})
	}
}

// TestContentTypeForPool exercises content-type detection for pool/ files.
func TestContentTypeForPool(t *testing.T) {
	tests := []struct {
		file string
		want string
	}{
		{"libfoo_1.0_amd64.deb", "application/vnd.debian.binary-package"},
		{"libfoo-udeb_1.0_amd64.udeb", "application/vnd.debian.binary-package"},
		{"libfoo.dsc", "text/plain; charset=utf-8"},
		{"libfoo_1.0.orig.tar.gz", "application/octet-stream"},
		{"unknown.bin", "application/octet-stream"},
	}
	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			assert.Equal(t, tc.want, contentTypeForPool(tc.file))
		})
	}
}

// TestAptMutableKey exercises the mutable-tier cache key builder.
func TestAptMutableKey(t *testing.T) {
	ref := distsRef("ubuntu", "focal/InRelease")
	got := aptMutableKey(ref)
	assert.Equal(t, "apt:ubuntu:focal/InRelease", got,
		"mutable key must be 'protocol:name:version'")
}

// ── HTTP routing & method enforcement tests ───────────────────────────────────

// TestServeHTTP_MethodNotAllowed verifies that only GET and HEAD are accepted.
func TestServeHTTP_MethodNotAllowed(t *testing.T) {
	h := newHandlerNoUpstream(newAptTestCache())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	for _, method := range []string{
		http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch,
	} {
		t.Run(method, func(t *testing.T) {
			req, _ := http.NewRequest(method, srv.URL+"/dists/focal/InRelease", nil)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
			assert.Equal(t, "GET, HEAD", resp.Header.Get("Allow"))
		})
	}
}

// TestServeHTTP_PathTraversal_Returns404 verifies that paths containing ".."
// are rejected to prevent directory traversal attacks.
func TestServeHTTP_PathTraversal_Returns404(t *testing.T) {
	h := newHandlerNoUpstream(newAptTestCache())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	paths := []string{
		"/dists/../etc/passwd",
		"/pool/../pool/evil.deb",
		"/dists/focal/../InRelease",
	}
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			resp, err := http.Get(srv.URL + p)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusNotFound, resp.StatusCode,
				"path traversal (%q) must be rejected with 404", p)
		})
	}
}

// TestServeHTTP_UnknownPath_Returns404 verifies that paths not matching
// /dists/ or /pool/ result in 404.
func TestServeHTTP_UnknownPath_Returns404(t *testing.T) {
	h := newHandlerNoUpstream(newAptTestCache())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	for _, p := range []string{"/", "/apt/", "/something/else", "/v2/", "/pypi/"} {
		t.Run(p, func(t *testing.T) {
			resp, err := http.Get(srv.URL + p)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusNotFound, resp.StatusCode)
		})
	}
}

// TestServeHTTP_EmptyDistsPath_Returns404 verifies that /dists/ with no
// suite produces 404.
func TestServeHTTP_EmptyDistsPath_Returns404(t *testing.T) {
	h := newHandlerNoUpstream(newAptTestCache())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	// The path /dists/ has an empty distsPath after the segment.
	resp, err := http.Get(srv.URL + "/dists/")
	require.NoError(t, err)
	defer resp.Body.Close()
	// cut() returns ok=false for empty after string → 404
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// TestServeHTTP_Pool_BadPathNoFile_Returns404 verifies that a pool/ path with
// no file component (only a directory) returns 404.
func TestServeHTTP_Pool_BadPathNoFile_Returns404(t *testing.T) {
	h := newHandlerNoUpstream(newAptTestCache())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/pool/main/")
	require.NoError(t, err)
	defer resp.Body.Close()
	// splitDir("main/") → ok=false (trailing slash, no file)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ── Cache-hit tests ───────────────────────────────────────────────────────────

// TestServeHTTP_Dists_CacheHit verifies that a fresh dists/ entry in the cache
// is served without contacting any upstream.
func TestServeHTTP_Dists_CacheHit(t *testing.T) {
	const inReleaseBody = "Origin: Test\nSuite: focal\n"
	ref := distsRef("", "focal/InRelease")
	cm := newAptTestCache()
	cm.seedFresh(ref, []byte(inReleaseBody))

	h := newHandlerNoUpstream(cm) // no upstream; cache hit must not need one
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/dists/focal/InRelease")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, []byte(inReleaseBody), body)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/plain")
}

// TestServeHTTP_Dists_ContentType_Compressed verifies content-type for .gz.
func TestServeHTTP_Dists_ContentType_Compressed(t *testing.T) {
	ref := distsRef("", "focal/main/binary-amd64/Packages.gz")
	cm := newAptTestCache()
	cm.seedFresh(ref, []byte{0x1f, 0x8b, 0x08}) // gzip magic bytes

	h := newHandlerNoUpstream(cm)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/dists/focal/main/binary-amd64/Packages.gz")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/x-gzip", resp.Header.Get("Content-Type"))
}

// TestServeHTTP_Pool_CacheHit verifies that a cached pool/ artifact is served
// without upstream contact.
func TestServeHTTP_Pool_CacheHit(t *testing.T) {
	debContent := []byte("fake deb bytes")
	ref := poolRef("main/l/libfoo", "libfoo_1.0_amd64.deb")
	cm := newAptTestCache()
	cm.seedFresh(ref, debContent)

	h := newHandlerNoUpstream(cm)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/pool/main/l/libfoo/libfoo_1.0_amd64.deb")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, debContent, body)
	assert.Equal(t, "application/vnd.debian.binary-package", resp.Header.Get("Content-Type"))
}

// TestServeHTTP_Pool_CacheHit_UDeb verifies content-type for .udeb files.
func TestServeHTTP_Pool_CacheHit_UDeb(t *testing.T) {
	ref := poolRef("main/u/udeb-pkg", "udeb-pkg_1.0_amd64.udeb")
	cm := newAptTestCache()
	cm.seedFresh(ref, []byte("udeb content"))

	h := newHandlerNoUpstream(cm)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/pool/main/u/udeb-pkg/udeb-pkg_1.0_amd64.udeb")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/vnd.debian.binary-package", resp.Header.Get("Content-Type"))
}

// ── Cache-miss + no-upstream tests ───────────────────────────────────────────

// TestServeHTTP_Dists_CacheMiss_NoUpstream_Returns404 asserts 404 when no
// upstream is configured and the cache is empty.
func TestServeHTTP_Dists_CacheMiss_NoUpstream_Returns404(t *testing.T) {
	h := newHandlerNoUpstream(newAptTestCache())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/dists/focal/InRelease")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// TestServeHTTP_Pool_CacheMiss_NoUpstream_Returns404 asserts 404 when no
// upstream is configured and the CAS has no matching blob.
func TestServeHTTP_Pool_CacheMiss_NoUpstream_Returns404(t *testing.T) {
	h := newHandlerNoUpstream(newAptTestCache())
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/pool/main/l/libfoo/libfoo_1.0_amd64.deb")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ── Cache-miss + upstream fetch tests ────────────────────────────────────────

// TestServeHTTP_Dists_CacheMiss_FetchFromUpstream verifies that on a dists/
// cache miss the handler fetches from the upstream, serves the response, and
// stores the content for subsequent lookups.
func TestServeHTTP_Dists_CacheMiss_FetchFromUpstream(t *testing.T) {
	const inRelease = "Origin: Test\nSuite: focal\nValid-Until: ...\n"
	up := fakeAptUpstream(t, map[string][]byte{
		"/dists/focal/InRelease": []byte(inRelease),
	})

	cm := newAptTestCache()
	h := newHandlerWithFakeUpstream(cm, up.URL,
		WithMutableTTL(300),
		WithQuarantineDir(t.TempDir()),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	// First request: cache miss → upstream fetch.
	resp, err := http.Get(srv.URL + "/dists/focal/InRelease")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, []byte(inRelease), body,
		"response body must match upstream content")
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/plain")
}

// TestServeHTTP_Pool_CacheMiss_FetchFromUpstream verifies that on a pool/
// cache miss the handler fetches, quarantines, stores, and serves the .deb.
func TestServeHTTP_Pool_CacheMiss_FetchFromUpstream(t *testing.T) {
	debContent := []byte("fake debian package bytes\x00\x01\x02")
	up := fakeAptUpstream(t, map[string][]byte{
		"/pool/main/l/libfoo/libfoo_1.0_amd64.deb": debContent,
	})

	cm := newAptTestCache()
	h := newHandlerWithFakeUpstream(cm, up.URL,
		WithQuarantineDir(t.TempDir()),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	// First request: cache miss → fetch → store → serve.
	resp, err := http.Get(srv.URL + "/pool/main/l/libfoo/libfoo_1.0_amd64.deb")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, debContent, body,
		"first fetch must serve the upstream .deb bytes")
	assert.Equal(t, "application/vnd.debian.binary-package",
		resp.Header.Get("Content-Type"))

	// Second request: CAS hit — upstream must NOT be re-contacted.
	// (The upstream only serves one copy; a second request to the upstream
	// would not fail, but we verify the entry is stored in the cache.)
	ref := poolRef("main/l/libfoo", "libfoo_1.0_amd64.deb")
	cm.mu.Lock()
	entry := cm.entries[cm.cacheKey(ref)]
	cm.mu.Unlock()
	assert.NotNil(t, entry, "pool artifact must be stored in CAS after first fetch")
}

// TestServeHTTP_Pool_Upstream404_Returns404 asserts that an upstream 404 for a
// pool file results in a 404 from the handler (not 502).
func TestServeHTTP_Pool_Upstream404_Returns404(t *testing.T) {
	up := fakeAptUpstream(t, map[string][]byte{}) // empty → all 404

	cm := newAptTestCache()
	h := newHandlerWithFakeUpstream(cm, up.URL, WithQuarantineDir(t.TempDir()))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/pool/main/l/libfoo/libfoo_1.0_amd64.deb")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// TestServeHTTP_Pool_UpstreamError_Returns502 asserts that a transient upstream
// error (500) for a pool file results in 502 from the handler.
func TestServeHTTP_Pool_UpstreamError_Returns502(t *testing.T) {
	up := failingUpstream(t)

	cm := newAptTestCache()
	h := newHandlerWithFakeUpstream(cm, up.URL, WithQuarantineDir(t.TempDir()))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/pool/main/l/libfoo/libfoo_1.0_amd64.deb")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

// TestServeHTTP_Dists_UpstreamError_Returns502 asserts that a transient
// upstream error for a dists/ file results in 502 (no stale available).
func TestServeHTTP_Dists_UpstreamError_Returns502(t *testing.T) {
	up := failingUpstream(t)

	cm := newAptTestCache()
	h := newHandlerWithFakeUpstream(cm, up.URL, WithQuarantineDir(t.TempDir()))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/dists/focal/InRelease")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

// TestServeHTTP_VerifyError_Returns502_PoolFile asserts that a VerifyError from
// the cache (verify-on-write failure) results in 502 and does NOT serve the
// artifact. This is the critical "fail-closed" behaviour (DESIGN-REVIEW §C2).
func TestServeHTTP_VerifyError_Returns502_PoolFile(t *testing.T) {
	debContent := []byte("tampered deb bytes")
	up := fakeAptUpstream(t, map[string][]byte{
		"/pool/main/t/tamper/tampered_1.0_amd64.deb": debContent,
	})

	cm := newAptTestCache()
	// Inject a VerifyError: the cache's Store returns failure.
	cm.storeErr = &cache.VerifyError{
		Ref: poolRef("main/t/tamper", "tampered_1.0_amd64.deb"),
		Result: artifact.Result{
			Status:  artifact.StatusFail,
			Tier:    artifact.TierChecksum,
			Message: "digest mismatch",
		},
	}

	h := newHandlerWithFakeUpstream(cm, up.URL, WithQuarantineDir(t.TempDir()))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/pool/main/t/tamper/tampered_1.0_amd64.deb")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusBadGateway, resp.StatusCode,
		"verify-on-write FAIL must return 502, never serve the artifact (fix C2)")
}

// TestServeHTTP_Dists_StaleServed_WhenUpstreamFails verifies that the
// stale-serve path returns cached dists/ content when the upstream is
// unreachable (DESIGN-REVIEW §2 H1, ARCHITECTURE.md §3).
//
// This asserts real behaviour: aptTestCache.Serve is freshness-gated exactly
// like manager.Serve, so the stale bytes can only arrive via ServeEntry.
func TestServeHTTP_Dists_StaleServed_WhenUpstreamFails(t *testing.T) {
	const staleInRelease = "Origin: Test\nSuite: focal\n# stale content\n"
	ref := distsRef("", "focal/InRelease")

	cm := newAptTestCache()
	// Seed the stale map: Lookup returns nil, but LookupStale and Serve find it.
	cm.seedStale(ref, []byte(staleInRelease))

	up := failingUpstream(t) // upstream is down
	h := newHandlerWithFakeUpstream(cm, up.URL,
		WithMutableTTL(0), // always-revalidate
		WithQuarantineDir(t.TempDir()),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/dists/focal/InRelease")
	require.NoError(t, err)
	defer resp.Body.Close()

	// The handler SHOULD serve the stale content (designed behaviour).
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"stale dists/ content MUST be served when upstream is down (DESIGN-REVIEW §H1)")
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, []byte(staleInRelease), body)
}

// ── HEAD request tests ────────────────────────────────────────────────────────

// TestServeHTTP_HEAD_NoBody verifies that HEAD requests return 200 with no body.
func TestServeHTTP_HEAD_NoBody(t *testing.T) {
	tests := []struct {
		desc string
		path string
		ref  artifact.ArtifactRef
		data []byte
	}{
		{
			"dists_inrelease",
			"/dists/focal/InRelease",
			distsRef("", "focal/InRelease"),
			[]byte("Origin: Test\n"),
		},
		{
			"pool_deb",
			"/pool/main/l/libfoo/libfoo_1.0_amd64.deb",
			poolRef("main/l/libfoo", "libfoo_1.0_amd64.deb"),
			[]byte("fake deb bytes"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			cm := newAptTestCache()
			cm.seedFresh(tc.ref, tc.data)

			h := newHandlerNoUpstream(cm)
			srv := httptest.NewServer(h)
			t.Cleanup(srv.Close)

			req, _ := http.NewRequest(http.MethodHead, srv.URL+tc.path, nil)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()

			require.Equal(t, http.StatusOK, resp.StatusCode)
			body, _ := io.ReadAll(resp.Body)
			assert.Empty(t, body, "HEAD response MUST have no body")
		})
	}
}

// ── Path prefix tests ─────────────────────────────────────────────────────────

// TestServeHTTP_PathPrefix_Stripped verifies that WithPathPrefix correctly
// strips a mount prefix before routing.
func TestServeHTTP_PathPrefix_Stripped(t *testing.T) {
	inRelease := []byte("Origin: Test\n")
	ref := distsRef("", "focal/InRelease")
	cm := newAptTestCache()
	cm.seedFresh(ref, inRelease)

	h := newHandlerNoUpstream(cm, WithPathPrefix("/apt"))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/apt/dists/focal/InRelease")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, inRelease, body)
}

// TestServeHTTP_PathPrefix_NoPrefix_UnmodifiedRouting verifies that a handler
// without a path prefix routes normally.
func TestServeHTTP_PathPrefix_NoPrefix_UnmodifiedRouting(t *testing.T) {
	inRelease := []byte("Origin: Test\n")
	ref := distsRef("", "focal/InRelease")
	cm := newAptTestCache()
	cm.seedFresh(ref, inRelease)

	h := newHandlerNoUpstream(cm) // no WithPathPrefix
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/dists/focal/InRelease")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// ── ArtifactRef routing invariants ───────────────────────────────────────────

// TestRouting_Dists_InRelease_BuildsCorrectRef verifies that the ArtifactRef
// built for dists/focal/InRelease has the expected fields.
//
// Requirement: DESIGN-REVIEW §3 table — apt mutable: InRelease/Release/Packages.
func TestRouting_Dists_InRelease_BuildsCorrectRef(t *testing.T) {
	ref := distsRef("ubuntu", "focal/InRelease")
	assert.Equal(t, "apt", ref.Protocol)
	assert.Equal(t, "ubuntu", ref.Name) // repo prefix scopes the cache key
	assert.Equal(t, "focal/InRelease", ref.Version)
	assert.True(t, ref.Mutable)
}

// TestRouting_Pool_Deb_BuildsCorrectRef verifies that the ArtifactRef built
// for pool/main/l/libfoo/libfoo_1.0_amd64.deb has the expected fields.
//
// Requirement: DESIGN-REVIEW §3 table — apt immutable: pool/*.deb.
func TestRouting_Pool_Deb_BuildsCorrectRef(t *testing.T) {
	ref := poolRef("main/l/libfoo", "libfoo_1.0_amd64.deb")
	assert.Equal(t, "apt", ref.Protocol)
	assert.Equal(t, "main/l/libfoo", ref.Name)
	assert.Equal(t, "libfoo_1.0_amd64.deb", ref.Version)
	assert.False(t, ref.Mutable)
}

// ── WithMutableTTL option ─────────────────────────────────────────────────────

// TestWithMutableTTL_OverridesDefault asserts WithMutableTTL can override the
// default always-revalidate sentinel.
func TestWithMutableTTL_OverridesDefault(t *testing.T) {
	h := NewHandler(newAptTestCache(), WithMutableTTL(300))
	assert.Equal(t, int64(300), h.mutableTTLSec,
		"WithMutableTTL must override the default 0 sentinel")
}

// TestWithMutableTTL_NeverRevalidate_Sentinel asserts that -1 (never revalidate)
// can be configured for testing purposes.
func TestWithMutableTTL_NeverRevalidate_Sentinel(t *testing.T) {
	h := NewHandler(newAptTestCache(), WithMutableTTL(ttlNeverRevalidate))
	assert.Equal(t, ttlNeverRevalidate, h.mutableTTLSec)
}

// ── isNotFound helper ─────────────────────────────────────────────────────────

// TestIsNotFound exercises the upstream-404 detection helper.
func TestIsNotFound(t *testing.T) {
	assert.True(t, isNotFound(errors.New("upstream foo: HTTP 404")))
	assert.False(t, isNotFound(errors.New("upstream foo: HTTP 500")))
	assert.False(t, isNotFound(nil))
	assert.False(t, isNotFound(errors.New("connection refused")))
}

// ServeEntry mirrors manager.ServeEntry: it serves the bytes of the entry the
// caller already holds, with no lookup and therefore no freshness gate. This is
// the ONLY way stale bytes can reach the response — Serve cannot produce them.
func (c *aptTestCache) ServeEntry(_ context.Context, entry *artifact.CacheEntry, offset, length int64) (io.ReadCloser, error) {
	if entry == nil {
		return nil, cache.ErrCacheMiss
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	data, ok := c.blobs[entry.Digest]
	if !ok {
		return nil, cache.ErrCacheMiss
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
	return io.NopCloser(bytes.NewReader(data[start:end])), nil
}

// entryServer satisfied — the handler opts in via a type assertion.
var _ entryServer = (*aptTestCache)(nil)
