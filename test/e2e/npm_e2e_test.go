// Package e2e — hermetic end-to-end tests for the Specula npm registry handler.
//
// Every test runs entirely in-process: a tiny fake npm registry
// (httptest.Server) serves fixed packuments and tarballs; the real Specula npm
// Handler is wired with LocalDiskDriver + SQLiteStore + verify.Chain (checksum +
// TOFU) + cache.New, exactly as production.
//
// # What is tested
//
//   - TestNpmRouting          — routing layer: 404 (empty/invalid path), 405 (wrong
//     method), 404 cache-miss without upstream.
//   - TestNpmPackumentServed  — cold fetch populates CAS; bytes match fake upstream.
//   - TestNpmTarballCacheHit  — second tarball fetch is served from CAS; upstream
//     receives no additional requests.
//   - TestNpmTofuFirstLock    — first tarball fetch pins the TOFU store (StatusWarn
//     but promoted); a subsequent in-process check confirms the pin is set.
//   - TestNpmIntegrityMismatch — a second Store with different bytes for the same
//     ref triggers a TofuVerifier StatusFail and returns *cache.VerifyError.
package e2e

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
	npmhandler "github.com/ivanzzeth/specula/internal/handler/npm"
	"github.com/ivanzzeth/specula/internal/upstream"
)

// ── Fake npm registry ─────────────────────────────────────────────────────────

// npmRegistryCounters counts per-endpoint upstream hits for cache verification.
type npmRegistryCounters struct {
	packument int64
	tarball   int64
}

// newFakeNpmRegistry returns an in-process httptest.Server that serves npm
// packuments and tarballs. All served paths follow npm's URL schema:
//
//	GET /<pkg>             → packuments[pkg]
//	GET /<pkg>/-/<file>    → tarballs[file]
//
// Hit counters allow tests to assert upstream contact counts.
func newFakeNpmRegistry(
	t *testing.T,
	packuments map[string][]byte,
	tarballs map[string][]byte,
) (*httptest.Server, *npmRegistryCounters) {
	t.Helper()

	cnt := &npmRegistryCounters{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if len(p) == 0 {
			http.NotFound(w, r)
			return
		}
		rest := p[1:] // strip leading /

		// Detect tarball: rest contains "/-/"
		if pkg, file, ok := splitNpmTarball(rest); ok {
			_ = pkg
			data, found := tarballs[file]
			if !found {
				http.NotFound(w, r)
				return
			}
			atomic.AddInt64(&cnt.tarball, 1)
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(data)
			return
		}

		// Packument
		data, found := packuments[rest]
		if !found {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt64(&cnt.packument, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))

	t.Cleanup(srv.Close)
	return srv, cnt
}

// splitNpmTarball mirrors the handler's splitTarball for use in the fake
// registry without importing the handler package (to avoid import cycles).
func splitNpmTarball(rest string) (pkg, file string, ok bool) {
	const sep = "/-/"
	i := len(rest) - 1
	idx := -1
	for i >= 0 {
		if i+len(sep) <= len(rest) && rest[i:i+len(sep)] == sep {
			idx = i
			break
		}
		i--
	}
	if idx <= 0 {
		return "", "", false
	}
	p := rest[:idx]
	f := rest[idx+len(sep):]
	if p == "" || f == "" {
		return "", "", false
	}
	return p, f, true
}

// ── npm handler stack setup ───────────────────────────────────────────────────

// newNpmServer wires npmhandler.Handler over the given speculaStack pointing at
// fakeURL, and returns a running httptest.Server.
func newNpmServer(
	t *testing.T,
	s *speculaStack,
	fakeURL string,
	mutableTTL int64,
	extra ...npmhandler.Option,
) *httptest.Server {
	t.Helper()

	ups := []upstream.Upstream{{Name: "fake-npm", BaseURL: fakeURL, Priority: 0}}
	opts := []npmhandler.Option{
		npmhandler.WithMeta(s.metaStore),
		npmhandler.WithUpstream(upstream.NewClient(), ups),
		npmhandler.WithQuarantineDir(s.dir),
		npmhandler.WithMutableTTL(mutableTTL),
	}
	opts = append(opts, extra...)

	h := npmhandler.NewHandler(s.cm, opts...)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// ── Test 1 — Routing ─────────────────────────────────────────────────────────

// TestNpmRouting validates the handler's routing layer: correct HTTP status
// codes for empty/invalid paths, wrong methods, and cache-miss without upstream.
func TestNpmRouting(t *testing.T) {
	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	// No upstream — purely testing the routing layer.
	h := npmhandler.NewHandler(s.cm)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	t.Run("empty_path_404", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/")
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("post_method_405", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/react", nil)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
		assert.Equal(t, "GET, HEAD", resp.Header.Get("Allow"))
	})

	t.Run("cache_miss_no_upstream_404", func(t *testing.T) {
		status, _ := httpGet(t, srv.URL+"/react")
		assert.Equal(t, http.StatusNotFound, status)
	})

	t.Run("tarball_cache_miss_no_upstream_404", func(t *testing.T) {
		status, _ := httpGet(t, srv.URL+"/react/-/react-18.2.0.tgz")
		assert.Equal(t, http.StatusNotFound, status)
	})

	t.Run("scoped_cache_miss_no_upstream_404", func(t *testing.T) {
		status, _ := httpGet(t, srv.URL+"/@myorg/pkg")
		assert.Equal(t, http.StatusNotFound, status)
	})
}

// ── Test 2 — Cold packument fetch populates CAS ───────────────────────────────

// TestNpmPackumentServed verifies that a cold-cache packument fetch:
//   - Returns 200 with the upstream's bytes.
//   - Stores the packument in the CAS (blobStore usage is positive afterward).
func TestNpmPackumentServed(t *testing.T) {
	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	packumentBytes := []byte(`{"name":"react","dist-tags":{"latest":"18.2.0"},"versions":{}}`)
	reg, cnt := newFakeNpmRegistry(t,
		map[string][]byte{"react": packumentBytes},
		nil,
	)

	srv := newNpmServer(t, s, reg.URL, 300)

	status, body := httpGet(t, srv.URL+"/react")
	require.Equal(t, http.StatusOK, status, "packument fetch must return 200")
	assert.Equal(t, packumentBytes, body, "packument bytes must match fake upstream")
	assert.Equal(t, int64(1), atomic.LoadInt64(&cnt.packument), "upstream hit on cold fetch")

	// CAS must have been populated.
	ctx := context.Background()
	used, err := s.blobStore.UsageBytes(ctx)
	require.NoError(t, err)
	assert.Positive(t, used, "CAS must have bytes after packument fetch")

	// Second request should come from cache (mutable TTL=300s is still fresh).
	status2, body2 := httpGet(t, srv.URL+"/react")
	require.Equal(t, http.StatusOK, status2)
	assert.Equal(t, packumentBytes, body2)
	// Upstream should NOT be hit again while TTL is fresh.
	assert.Equal(t, int64(1), atomic.LoadInt64(&cnt.packument),
		"second fetch within TTL must not hit upstream")
}

// ── Test 3 — Tarball second fetch is served from CAS ─────────────────────────

// TestNpmTarballCacheHit verifies that after a cold-cache tarball fetch, the
// second request is served from the CAS and the fake upstream sees no additional
// tarball requests.
func TestNpmTarballCacheHit(t *testing.T) {
	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	tgzBytes := bytes.Repeat([]byte("FAKE_TGZ_CONTENT_"), 64)
	reg, cnt := newFakeNpmRegistry(t,
		nil,
		map[string][]byte{"react-18.2.0.tgz": tgzBytes},
	)

	srv := newNpmServer(t, s, reg.URL, 300)

	// First fetch — cold miss → upstream.
	status1, body1 := httpGet(t, srv.URL+"/react/-/react-18.2.0.tgz")
	require.Equal(t, http.StatusOK, status1, "first tarball fetch must return 200")
	assert.Equal(t, tgzBytes, body1, "first fetch bytes must match upstream")
	assert.Equal(t, int64(1), atomic.LoadInt64(&cnt.tarball), "upstream must be hit once on cold miss")

	// Second fetch — must come from CAS.
	status2, body2 := httpGet(t, srv.URL+"/react/-/react-18.2.0.tgz")
	require.Equal(t, http.StatusOK, status2, "second tarball fetch must return 200")
	assert.Equal(t, tgzBytes, body2, "second fetch bytes must match first fetch")
	assert.Equal(t, int64(1), atomic.LoadInt64(&cnt.tarball),
		"upstream must NOT be hit again for CAS-cached immutable tarball")
}

// ── Test 4 — First tarball fetch pins the TOFU store ─────────────────────────

// TestNpmTofuFirstLock verifies that when a tarball is fetched for the first
// time:
//   - The response is 200 and bytes match the upstream (StatusWarn still promotes).
//   - The TOFU store contains a pin for the tarball ref key after the fetch.
func TestNpmTofuFirstLock(t *testing.T) {
	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	tgzBytes := bytes.Repeat([]byte("NPM_TARBALL_DATA_"), 32)
	reg, _ := newFakeNpmRegistry(t,
		nil,
		map[string][]byte{"react-18.2.0.tgz": tgzBytes},
	)

	srv := newNpmServer(t, s, reg.URL, 300)

	// Cold fetch — TOFU pins on first contact (StatusWarn but still promoted).
	status, body := httpGet(t, srv.URL+"/react/-/react-18.2.0.tgz")
	require.Equal(t, http.StatusOK, status, "first tarball fetch must return 200")
	assert.Equal(t, tgzBytes, body)

	// The TOFU pin must be set. The key format used by TofuVerifier is
	// protocol + ":" + name + "@" + version.
	tofuKey := "npm:react@react-18.2.0.tgz"
	ctx := context.Background()
	pin, err := s.tofuStore.GetPin(ctx, tofuKey)
	require.NoError(t, err)
	assert.NotEmpty(t, pin, "TOFU pin must be set after first tarball fetch (key=%s)", tofuKey)
	assert.Contains(t, pin, "sha256:", "TOFU pin must be a sha256 digest")
}

// ── Test 5 — Integrity mismatch → VerifyError ────────────────────────────────

// TestNpmIntegrityMismatch verifies the TOFU-on-write rejection path:
//   - A first cache.Store pins the TOFU digest.
//   - A second cache.Store with different bytes for the SAME ref returns
//     *cache.VerifyError (StatusFail) and leaves the first blob untouched.
//
// The test bypasses the HTTP handler and calls cache.Quarantine + s.cm.Store
// directly (matching the verify-on-write unit test pattern in
// TestGomodVerifyOnWriteRejectsMismatch).
func TestNpmIntegrityMismatch(t *testing.T) {
	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	ctx := context.Background()

	const pkg = "react"
	const file = "react-18.2.0.tgz"

	ref := artifact.ArtifactRef{
		Protocol: "npm",
		Name:     pkg,
		Version:  file,
		Mutable:  false,
	}

	originalBytes := bytes.Repeat([]byte("ORIGINAL_TGZ_"), 32)
	tamperedBytes := bytes.Repeat([]byte("TAMPERED_TGZ_"), 32)

	// ── First Store: pins TOFU (StatusWarn, still promoted) ──────────────────
	art1, cleanup1, err := cache.Quarantine(ctx, tmp, bytes.NewReader(originalBytes), artifact.UpstreamMeta{})
	require.NoError(t, err)

	entry1, storeErr := s.cm.Store(ctx, ref, art1)
	if storeErr != nil {
		cleanup1()
	}
	require.NoError(t, storeErr, "first store must succeed (TOFU StatusWarn = promoted)")
	require.NotNil(t, entry1)

	// TOFU pin is now set for this ref.
	tofuKey := "npm:" + pkg + "@" + file
	pin, err := s.tofuStore.GetPin(ctx, tofuKey)
	require.NoError(t, err)
	require.NotEmpty(t, pin, "TOFU pin must be set after first store")

	// ── Second Store: tampered bytes → TOFU StatusFail → VerifyError ─────────
	art2, cleanup2, err := cache.Quarantine(ctx, tmp, bytes.NewReader(tamperedBytes), artifact.UpstreamMeta{})
	require.NoError(t, err)

	_, storeErr2 := s.cm.Store(ctx, ref, art2)
	if storeErr2 == nil {
		cleanup2()
	}

	require.Error(t, storeErr2, "second store with tampered bytes must fail")
	var ve *cache.VerifyError
	require.True(t, errors.As(storeErr2, &ve),
		"error must be *cache.VerifyError (got %T: %v)", storeErr2, storeErr2)
	assert.Equal(t, artifact.StatusFail, ve.Result.Status,
		"VerifyError must carry StatusFail")

	// The first blob must still be retrievable from the CAS.
	rc, _, err := s.cm.Serve(ctx, ref, 0, -1)
	require.NoError(t, err)
	defer rc.Close()
	served, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, originalBytes, served, "CAS must still serve the original bytes")
}
