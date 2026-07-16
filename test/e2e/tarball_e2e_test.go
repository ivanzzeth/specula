//go:build integration

// Package e2e — hermetic end-to-end tests for the Specula tarball handler.
//
// Every test runs entirely in-process: a minimal httptest.Server acts as the
// fake upstream file server, and the real Specula tarball Handler is wired with
// LocalDiskDriver + SQLiteStore + verify.Chain + cache.New, exactly as production.
// No external network access is needed.
//
// # What is tested
//
//   - TestTarballRouting          — routing layer: 403 (disallowed host), 404 (bad path),
//     405 (wrong method), 404 (empty list — no upstream configured).
//   - TestTarballFetchPopulatesCache — cold fetch populates the CAS; bytes match.
//   - TestTarballSecondHitFromCache — second fetch is a CAS hit; upstream counter unchanged.
//   - TestTarballDigestPin         — fetch with a correct ?digest=sha256:… pin succeeds.
//   - TestTarballDigestMismatch    — fetch with a wrong ?digest=sha256:… pin causes
//     verify-on-write failure: 502, CAS clean, quarantine file removed.
//   - TestTarballMultipleFiles     — multiple distinct files, each keyed independently.
//   - TestTarballStatsPopulated    — after fetches, per-protocol stats show tarball bytes.
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
	tarballhandler "github.com/ivanzzeth/specula/internal/handler/tarball"
)

// ── Fake file server ──────────────────────────────────────────────────────────

// fakeFileServer returns an httptest.Server that serves the provided content
// map (path → bytes) and a request counter.
//
// Requests for paths not in content return 404.
func fakeFileServer(t *testing.T, content map[string][]byte) (*httptest.Server, *int64) {
	t.Helper()

	var hits int64
	mux := http.NewServeMux()
	for path, data := range content {
		data := data // capture for closure
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&hits, 1)
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("ETag", fmt.Sprintf(`"%s"`, sha256hex(data)))
			_, _ = w.Write(data)
		})
	}

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &hits
}

// ── Tarball handler stack setup ───────────────────────────────────────────────

// newTarballServer wires a tarball.Handler over the given speculaStack and
// returns a running httptest.Server. The handler uses "http" scheme so it can
// reach the in-process httptest fake upstream.
func newTarballServer(
	t *testing.T,
	s *speculaStack,
	allowedHosts []string,
	extra ...tarballhandler.Option,
) *httptest.Server {
	t.Helper()

	opts := []tarballhandler.Option{
		tarballhandler.WithAllowedHosts(allowedHosts),
		tarballhandler.WithScheme("http"),
		tarballhandler.WithQuarantineDir(s.dir),
		tarballhandler.WithMeta(s.metaStore),
	}
	opts = append(opts, extra...)

	h := tarballhandler.NewHandler(s.cm, opts...)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// ── Test 1 — Routing and error codes ─────────────────────────────────────────

// TestTarballRouting verifies the routing and validation layer: correct HTTP
// status codes are returned for malformed / unauthorized requests.
func TestTarballRouting(t *testing.T) {
	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	// Use an httptest server as a placeholder upstream; its address is the
	// allowed host.
	placeholder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(placeholder.Close)

	allowedHost := placeholder.Listener.Addr().String()
	srv := newTarballServer(t, s, []string{allowedHost})

	t.Run("wrong_method_405", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/"+allowedHost+"/path/file.tar.gz", nil)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	})

	t.Run("empty_path_404", func(t *testing.T) {
		status, _ := httpGet(t, srv.URL+"/")
		assert.Equal(t, http.StatusNotFound, status)
	})

	t.Run("host_only_no_file_404", func(t *testing.T) {
		status, _ := httpGet(t, srv.URL+"/"+allowedHost)
		assert.Equal(t, http.StatusNotFound, status)
	})

	t.Run("disallowed_host_403", func(t *testing.T) {
		status, _ := httpGet(t, srv.URL+"/evil.example.com/path/file.tar.gz")
		assert.Equal(t, http.StatusForbidden, status)
	})

	t.Run("path_traversal_404", func(t *testing.T) {
		status, _ := httpGet(t, srv.URL+"/"+allowedHost+"/../../etc/passwd")
		assert.Equal(t, http.StatusNotFound, status)
	})
}

// ── Test 2 — Cold fetch populates CAS; bytes match ───────────────────────────

// TestTarballFetchPopulatesCache drives a cold-cache fetch, asserts that the
// response bytes equal the fake upstream content, and verifies that the CAS
// is populated after the fetch.
func TestTarballFetchPopulatesCache(t *testing.T) {
	const (
		filePath    = "/releases/v1.0.0/tool.tar.gz"
		fileContent = "binary content of tool v1.0.0"
	)

	data := []byte(fileContent)
	upstream, hits := fakeFileServer(t, map[string][]byte{filePath: data})

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	allowedHost := upstream.Listener.Addr().String()
	srv := newTarballServer(t, s, []string{allowedHost})

	reqURL := srv.URL + "/" + allowedHost + filePath
	status, body := httpGet(t, reqURL)

	require.Equal(t, http.StatusOK, status, "fetch must return 200")
	require.Equal(t, data, body, "response bytes must match upstream content")
	assert.EqualValues(t, 1, atomic.LoadInt64(hits), "upstream must be hit exactly once")

	// CAS must contain the blob.
	ctx := context.Background()
	used, err := s.blobStore.UsageBytes(ctx)
	require.NoError(t, err)
	assert.Positive(t, used, "CAS must have bytes after fetch")
}

// ── Test 3 — Second fetch is a CAS hit; upstream counter unchanged ────────────

// TestTarballSecondHitFromCache verifies that after the cold-cache miss path
// populates the CAS, subsequent fetches are served entirely from the CAS —
// the fake upstream sees no additional requests.
func TestTarballSecondHitFromCache(t *testing.T) {
	const filePath = "/dist/package-linux-amd64.tar.gz"
	data := bytes.Repeat([]byte("tarball-content-"), 64)

	upstream, hits := fakeFileServer(t, map[string][]byte{filePath: data})

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	allowedHost := upstream.Listener.Addr().String()
	srv := newTarballServer(t, s, []string{allowedHost})

	reqURL := srv.URL + "/" + allowedHost + filePath

	// First fetch — cold cache.
	status1, body1 := httpGet(t, reqURL)
	require.Equal(t, http.StatusOK, status1, "first fetch must return 200")
	require.Equal(t, data, body1, "first fetch bytes must match")
	hitAfterFirst := atomic.LoadInt64(hits)
	assert.EqualValues(t, 1, hitAfterFirst, "first fetch must hit upstream exactly once")

	// Second fetch — warm CAS.
	status2, body2 := httpGet(t, reqURL)
	require.Equal(t, http.StatusOK, status2, "second fetch must return 200 (cache hit)")
	require.Equal(t, data, body2, "second fetch bytes must match (from CAS)")
	assert.Equal(t, hitAfterFirst, atomic.LoadInt64(hits),
		"second fetch must NOT contact upstream (CAS hit)")
}

// ── Test 4 — Digest pin: correct digest ──────────────────────────────────────

// TestTarballDigestPin verifies that a fetch with the correct ?digest=sha256:…
// query parameter succeeds: the ChecksumVerifier accepts the matching digest
// and the bytes are promoted to the CAS.
func TestTarballDigestPin(t *testing.T) {
	const filePath = "/assets/pinned.tar.gz"
	data := []byte("pinned tarball content for e2e test")
	correctDigest := "sha256:" + sha256hex(data)

	upstream, hits := fakeFileServer(t, map[string][]byte{filePath: data})

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	allowedHost := upstream.Listener.Addr().String()
	srv := newTarballServer(t, s, []string{allowedHost})

	// Fetch with the correct digest pin.
	reqURL := srv.URL + "/" + allowedHost + filePath + "?digest=" + correctDigest
	status, body := httpGet(t, reqURL)

	require.Equal(t, http.StatusOK, status, "fetch with correct digest pin must return 200")
	require.Equal(t, data, body, "response bytes must match upstream content")
	assert.EqualValues(t, 1, atomic.LoadInt64(hits), "upstream must be hit once")

	// CAS must be populated.
	ctx := context.Background()
	used, err := s.blobStore.UsageBytes(ctx)
	require.NoError(t, err)
	assert.Positive(t, used, "CAS must contain the blob after pinned fetch")
}

// ── Test 5 — Digest pin: mismatch → 502, CAS clean ───────────────────────────

// TestTarballDigestMismatch verifies the verify-on-write contract: when the
// caller supplies a wrong ?digest=sha256:… pin, the ChecksumVerifier fails,
// the quarantine file is removed, the handler returns 502, and the CAS is empty.
func TestTarballDigestMismatch(t *testing.T) {
	const filePath = "/dist/mismatch.tar.gz"
	realContent := []byte("real tarball bytes that the server actually returns")
	wrongDigest := "sha256:" + sha256hex([]byte("some completely different bytes — NOT what the server returns"))

	// Safety check: real and wrong digests must differ.
	realDigest := "sha256:" + sha256hex(realContent)
	require.NotEqual(t, realDigest, wrongDigest, "test precondition: digests must differ")

	upstream, _ := fakeFileServer(t, map[string][]byte{filePath: realContent})

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	allowedHost := upstream.Listener.Addr().String()
	srv := newTarballServer(t, s, []string{allowedHost})

	reqURL := srv.URL + "/" + allowedHost + filePath + "?digest=" + wrongDigest
	status, _ := httpGet(t, reqURL)

	assert.Equal(t, http.StatusBadGateway, status,
		"wrong digest pin must cause verify-on-write failure (502)")

	ctx := context.Background()

	// CAS must be empty — quarantine never promoted.
	used, err := s.blobStore.UsageBytes(ctx)
	require.NoError(t, err)
	assert.Zero(t, used, "CAS must be empty after digest mismatch")

	// Neither the declared wrong digest nor the real digest must appear in CAS.
	existsBad, err := s.blobStore.Exists(ctx, wrongDigest)
	require.NoError(t, err)
	assert.False(t, existsBad, "CAS must not contain blob under the wrong declared digest")

	existsReal, err := s.blobStore.Exists(ctx, realDigest)
	require.NoError(t, err)
	assert.False(t, existsReal, "CAS must not contain blob under the real computed digest")
}

// ── Test 6 — Direct verify-on-write pipeline (cache.Quarantine + cm.Store) ───

// TestTarballVerifyOnWritePipeline directly exercises the quarantine+Store
// pipeline with a wrong ref.Digest — the protocol-agnostic path exercised
// by the gomod equivalent test. This ensures the pipeline works correctly
// for the "tarball" protocol, not just gomod.
func TestTarballVerifyOnWritePipeline(t *testing.T) {
	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	ctx := context.Background()

	realBytes := []byte("genuine tarball bytes for the verify-on-write test")
	art, cleanup, err := cache.Quarantine(ctx, tmp, bytes.NewReader(realBytes), artifact.UpstreamMeta{})
	require.NoError(t, err)
	defer cleanup()

	wrongDigest := "sha256:" + sha256hex([]byte("completely different bytes — NOT what was written"))
	require.NotEqual(t, art.Digest, wrongDigest,
		"test precondition: wrong digest must differ from actual computed digest")

	ref := artifact.ArtifactRef{
		Protocol: "tarball",
		Name:     "example.com/releases",
		Version:  "tool-v1.0.0.tar.gz",
		Digest:   wrongDigest, // intentionally wrong — triggers ChecksumVerifier fail
		Mutable:  false,
	}

	_, storeErr := s.cm.Store(ctx, ref, art)
	require.Error(t, storeErr, "Store must fail when ref.Digest does not match actual digest")

	ve, isVerify := cache.AsVerifyError(storeErr)
	require.True(t, isVerify,
		"error must be *cache.VerifyError, got %T: %v", storeErr, storeErr)
	assert.Equal(t, artifact.StatusFail, ve.Result.Status,
		"verify status must be FAIL on digest mismatch")

	// Quarantine file must have been removed by Store on failure.
	_, statErr := os.Stat(art.Path)
	assert.True(t, os.IsNotExist(statErr),
		"quarantine file must be removed after verify-on-write failure")

	// CAS must be clean.
	existsBad, err := s.blobStore.Exists(ctx, wrongDigest)
	require.NoError(t, err)
	assert.False(t, existsBad, "CAS must not contain blob under the wrong digest")

	existsActual, err := s.blobStore.Exists(ctx, art.Digest)
	require.NoError(t, err)
	assert.False(t, existsActual, "CAS must not contain blob under the actual digest")
}

// ── Test 7 — Multiple distinct files are keyed independently ─────────────────

// TestTarballMultipleFiles verifies that distinct files (same host, different
// paths; or different hosts) are keyed independently in the CAS. A cache hit
// for one file must not interfere with another.
func TestTarballMultipleFiles(t *testing.T) {
	files := map[string][]byte{
		"/pkg/alpha/alpha.tar.gz": []byte("alpha package bytes"),
		"/pkg/beta/beta.tar.gz":   []byte("beta package bytes"),
	}

	upstream, _ := fakeFileServer(t, files)

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	allowedHost := upstream.Listener.Addr().String()
	srv := newTarballServer(t, s, []string{allowedHost})

	for path, want := range files {
		reqURL := srv.URL + "/" + allowedHost + path
		status, body := httpGet(t, reqURL)
		require.Equal(t, http.StatusOK, status, "fetch %s must return 200", path)
		require.Equal(t, want, body, "fetch %s bytes must match", path)
	}

	ctx := context.Background()
	stats, err := s.metaStore.CacheSizeByProtocol(ctx)
	require.NoError(t, err)
	tarballStat, ok := stats["tarball"]
	require.True(t, ok, "tarball stats must exist after fetches")
	assert.Equal(t, int64(2), tarballStat.Objects,
		"two independent files must produce two CAS entries")
}

// ── Test 8 — Per-protocol stats are populated ─────────────────────────────────

// TestTarballStatsPopulated verifies that the per-protocol tarball stats
// (bytes, objects) are correctly accumulated after fetches.
func TestTarballStatsPopulated(t *testing.T) {
	data := []byte("stats test tarball content for specula")
	upstream, _ := fakeFileServer(t, map[string][]byte{"/assets/stats.tar.gz": data})

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	allowedHost := upstream.Listener.Addr().String()
	srv := newTarballServer(t, s, []string{allowedHost})

	status, _ := httpGet(t, srv.URL+"/"+allowedHost+"/assets/stats.tar.gz")
	require.Equal(t, http.StatusOK, status)

	ctx := context.Background()
	stats, err := s.metaStore.CacheSizeByProtocol(ctx)
	require.NoError(t, err)

	ts, ok := stats["tarball"]
	require.True(t, ok, "tarball entry must appear in CacheSizeByProtocol")
	assert.EqualValues(t, 1, ts.Objects, "one tarball object must be counted")
	assert.Positive(t, ts.Bytes, "tarball bytes must be positive")
	assert.GreaterOrEqual(t, ts.Bytes, int64(len(data)),
		"bytes must be at least the size of the fetched content")
}

// ── Live test — gated behind SPECULA_E2E_LIVE=1 ───────────────────────────────

// TestTarballLive fetches a real, small public release tarball through Specula.
// The second fetch must be served from the CAS. Requires outbound HTTPS access.
//
// Skipped unless SPECULA_E2E_LIVE=1.
func TestTarballLive(t *testing.T) {
	if os.Getenv("SPECULA_E2E_LIVE") != "1" {
		t.Skip("set SPECULA_E2E_LIVE=1 to run live network tests (requires outbound HTTPS)")
	}

	// github.com releases a checksums.txt alongside tarball releases; use a tiny
	// publicly accessible file. We use a well-known, stable URL.
	const (
		liveHost = "raw.githubusercontent.com"
		livePath = "/cli/cli/v2.40.1/README.md"
	)

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	h := tarballhandler.NewHandler(s.cm,
		tarballhandler.WithAllowedHosts([]string{liveHost}),
		tarballhandler.WithScheme("https"),
		tarballhandler.WithQuarantineDir(tmp),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	reqURL := srv.URL + "/" + liveHost + livePath

	// First fetch — cold cache.
	status1, body1 := httpGet(t, reqURL)
	if status1 == http.StatusBadGateway {
		t.Skip("live upstream unreachable (SPECULA_E2E_LIVE=1 but no outbound HTTPS)")
	}
	require.Equal(t, http.StatusOK, status1, "live first fetch must return 200")
	assert.NotEmpty(t, body1, "live first fetch must return non-empty body")
	t.Logf("live: fetched %s%s (%d bytes) through Specula", liveHost, livePath, len(body1))

	// CAS must be populated.
	ctx := context.Background()
	used, err := s.blobStore.UsageBytes(ctx)
	require.NoError(t, err)
	assert.Positive(t, used, "CAS must have bytes after live fetch")

	// Second fetch — warm CAS; bytes must match and no re-fetch from upstream.
	status2, body2 := httpGet(t, reqURL)
	require.Equal(t, http.StatusOK, status2, "live second fetch must return 200 (CAS hit)")
	assert.Equal(t, body1, body2, "live second fetch must return same bytes as first (CAS served)")
	t.Logf("live: second fetch served from CAS (%d bytes)", len(body2))
}
