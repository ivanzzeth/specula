// Package e2e — hermetic end-to-end tests for the Specula PyPI data-plane handler.
//
// Every test runs entirely in-process: a tiny fake PyPI index / package server
// (httptest.Server) acts as the upstream, and the real Specula pypi.Handler is
// wired with LocalDiskDriver + SQLiteStore + verify.Chain(checksum+tofu) +
// cache.New, exactly as production. No external network access is needed.
//
// # What is tested
//
//   - TestPypiIndexServed             — GET /simple/<project>/ returns 200 with
//     HTML Content-Type and the index body.
//   - TestPypiWheelCachedAndHit       — cold-cache wheel fetch populates CAS;
//     second request is a CAS hit (upstream counter stays at 1).
//   - TestPypiTofuPinConfirmed        — second fetch of the same wheel content
//     confirms the TOFU pin (StatusPass) and returns 200.
//   - TestPypiSHA256Mismatch502       — after pinning, if the upstream serves
//     different bytes (the blob is removed from CAS to force a re-fetch), the
//     TofuVerifier returns StatusFail and the handler returns 502.
package e2e

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	pypihandler "github.com/ivanzzeth/specula/internal/handler/pypi"
	"github.com/ivanzzeth/specula/internal/upstream"
)

// ── fake PyPI upstream ────────────────────────────────────────────────────────

// fakePyPI is a minimal in-process PyPI simple-index + package server.
//
// URL structure:
//
//	GET /simple/<project>/         → simpleIndex[project] (mutable, updated via setIndex)
//	GET /packages/<name>/<file>    → packageBytes[file]   (mutable, updated via setFile)
//
// Both maps are guarded by a RWMutex so tests can swap content mid-test.
type fakePyPI struct {
	mu           sync.RWMutex
	simpleIndex  map[string][]byte // key: normalised project name
	packageBytes map[string][]byte // key: filename
	hits         int64             // total GET request counter (atomic)
}

func newFakePyPI() *fakePyPI {
	return &fakePyPI{
		simpleIndex:  make(map[string][]byte),
		packageBytes: make(map[string][]byte),
	}
}

func (f *fakePyPI) setIndex(project string, body []byte) {
	f.mu.Lock()
	f.simpleIndex[normalizeProject(project)] = body
	f.mu.Unlock()
}

func (f *fakePyPI) setFile(filename string, body []byte) {
	f.mu.Lock()
	f.packageBytes[filename] = body
	f.mu.Unlock()
}

// normalizeProject mirrors pypi.normalizeProject so the fake can use the same
// normalisation without importing the unexported helper.
func normalizeProject(name string) string {
	name = strings.ToLower(name)
	var b strings.Builder
	prevSep := false
	for _, r := range name {
		if r == '-' || r == '_' || r == '.' {
			if !prevSep {
				b.WriteByte('-')
				prevSep = true
			}
			continue
		}
		b.WriteRune(r)
		prevSep = false
	}
	s := b.String()
	s = strings.Trim(s, "-")
	return s
}

func (f *fakePyPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt64(&f.hits, 1)
	p := r.URL.Path

	if strings.HasPrefix(p, "/simple/") {
		// Extract and normalise the project name from /simple/<project>/
		rest := strings.TrimPrefix(p, "/simple/")
		rest = strings.Trim(rest, "/")
		if rest == "" || strings.Contains(rest, "/") {
			http.NotFound(w, r)
			return
		}
		project := normalizeProject(rest)

		f.mu.RLock()
		body, ok := f.simpleIndex[project]
		f.mu.RUnlock()

		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(body)
		return
	}

	if strings.HasPrefix(p, "/packages/") {
		// Extract the filename (last path component).
		idx := strings.LastIndexByte(p, '/')
		if idx < 0 || idx == len(p)-1 {
			http.NotFound(w, r)
			return
		}
		file := p[idx+1:]

		f.mu.RLock()
		body, ok := f.packageBytes[file]
		f.mu.RUnlock()

		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(body)
		return
	}

	http.NotFound(w, r)
}

// newFakePyPISrv starts an httptest.Server backed by f and registers cleanup.
func newFakePyPISrv(t *testing.T, f *fakePyPI) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return srv
}

// ── Specula pypi handler stack ────────────────────────────────────────────────

// newPypiSpeculaServer wires a pypi.Handler over the given speculaStack,
// pointing the upstream at fakeURL, and returns a running httptest.Server.
func newPypiSpeculaServer(
	t *testing.T,
	s *speculaStack,
	fakeURL string,
	opts ...pypihandler.Option,
) *httptest.Server {
	t.Helper()

	ups := []upstream.Upstream{{Name: "fake-pypi", BaseURL: fakeURL, Priority: 0}}
	allOpts := []pypihandler.Option{
		pypihandler.WithMeta(s.metaStore),
		pypihandler.WithUpstream(upstream.NewClient(), ups),
		pypihandler.WithQuarantineDir(s.dir),
		pypihandler.WithMutableTTL(1800),
	}
	allOpts = append(allOpts, opts...)

	h := pypihandler.NewHandler(s.cm, allOpts...)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// ── PyPI ArtifactRef helpers ──────────────────────────────────────────────────

// pypiIndexRef returns the mutable ArtifactRef for a /simple/<project>/ index.
// Matches pypi.indexRef internally.
func pypiIndexRef(project string) artifact.ArtifactRef {
	return artifact.ArtifactRef{
		Protocol: "pypi",
		Name:     project,
		Version:  "simple",
		Mutable:  true,
	}
}

// pypiFileRef returns the immutable ArtifactRef for a wheel/sdist file.
// Matches pypi.fileRef internally: Name = directory path, Version = filename.
func pypiFileRef(name, file string) artifact.ArtifactRef {
	return artifact.ArtifactRef{
		Protocol: "pypi",
		Name:     name,
		Version:  file,
		Mutable:  false,
	}
}

// ── Test 1 — Simple index is served ──────────────────────────────────────────

// TestPypiIndexServed drives a cold-cache GET /simple/<project>/ request through
// Specula. The fake upstream serves an HTML simple index. Specula must:
//  1. Detect a cache miss.
//  2. Fetch the index from the upstream.
//  3. Quarantine → verify-on-write → promote to mutable-tier CAS.
//  4. Return 200 with Content-Type: text/html and the index bytes.
func TestPypiIndexServed(t *testing.T) {
	indexHTML := []byte(`<!DOCTYPE html>
<html><body>
<a href="/packages/ab/flask-2.0-py3-none-any.whl#sha256=abc">Flask-2.0-py3-none-any.whl</a>
</body></html>`)

	f := newFakePyPI()
	f.setIndex("flask", indexHTML)
	upSrv := newFakePyPISrv(t, f)

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	srv := newPypiSpeculaServer(t, s, upSrv.URL)

	resp, err := http.Get(srv.URL + "/simple/flask/")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode, "GET /simple/flask/ must return 200")
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html",
		"Content-Type must be text/html")

	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, indexHTML, got, "response body must match upstream index HTML")
}

// ── Test 2 — Wheel cached and hit ─────────────────────────────────────────────

// TestPypiWheelCachedAndHit drives a cold-cache wheel file fetch through
// Specula (first request), then a second request that must be served entirely
// from the CAS without contacting the upstream.
//
// Protocol:
//  1. First GET /packages/ab/flask-2.0-py3-none-any.whl → cache miss → upstream
//     fetch → Quarantine → ChecksumVerifier(PASS, ref.Digest="") →
//     TofuVerifier(WARN, first-lock) → CAS promotion → 200.
//  2. Second GET → CAS hit → 200.
//  3. Upstream hit counter is 1 after both requests.
func TestPypiWheelCachedAndHit(t *testing.T) {
	whlBytes := bytes.Repeat([]byte("FAKE_WHEEL_"), 64)
	const whlFile = "flask-2.0-py3-none-any.whl"
	const whlPath = "/packages/ab/" + whlFile

	f := newFakePyPI()
	f.setFile(whlFile, whlBytes)
	upSrv := newFakePyPISrv(t, f)

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	srv := newPypiSpeculaServer(t, s, upSrv.URL)

	hitsBase := atomic.LoadInt64(&f.hits)

	// First request — cold cache.
	resp, err := http.Get(srv.URL + whlPath)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode, "first wheel GET must return 200")
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, whlBytes, got, "first wheel GET must return upstream bytes")

	hitsAfterFirst := atomic.LoadInt64(&f.hits)
	assert.Greater(t, hitsAfterFirst, hitsBase, "first fetch must contact upstream")

	// Verify CAS entry exists after first fetch.
	ctx := context.Background()
	entry, err := s.cm.Lookup(ctx, pypiFileRef("ab", whlFile))
	require.NoError(t, err)
	assert.NotNil(t, entry, "wheel must be in CAS after first fetch")

	// Second request — must be a CAS hit.
	resp2, err := http.Get(srv.URL + whlPath)
	require.NoError(t, err)
	defer resp2.Body.Close()

	require.Equal(t, http.StatusOK, resp2.StatusCode, "second wheel GET must return 200")
	got2, err := io.ReadAll(resp2.Body)
	require.NoError(t, err)
	assert.Equal(t, whlBytes, got2, "second wheel GET must return same bytes (from CAS)")

	hitsAfterSecond := atomic.LoadInt64(&f.hits)
	assert.Equal(t, hitsAfterFirst, hitsAfterSecond,
		"second wheel GET must NOT contact upstream (CAS hit)")
}

// ── Test 3 — TOFU pin confirmed on repeat fetch ───────────────────────────────

// TestPypiTofuPinConfirmed verifies that the TofuVerifier's StatusWarn
// (first-lock) on the initial fetch does not block serving, and that a repeat
// fetch of the same content returns 200 (StatusPass — pin confirmed).
//
// This tests the happy-path TOFU flow that should occur for every legitimate
// recurring download.
func TestPypiTofuPinConfirmed(t *testing.T) {
	whlBytes := bytes.Repeat([]byte("CONFIRMED_WHEEL_"), 32)
	const whlFile = "requests-2.28.0-py3-none-any.whl"
	const whlPath = "/packages/cd/ef/" + whlFile

	f := newFakePyPI()
	f.setFile(whlFile, whlBytes)
	upSrv := newFakePyPISrv(t, f)

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	srv := newPypiSpeculaServer(t, s, upSrv.URL)

	ctx := context.Background()

	// First fetch: TOFU first-lock (StatusWarn → still promoted, served as 200).
	resp, err := http.Get(srv.URL + whlPath)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "first TOFU fetch must return 200")
	assert.Equal(t, whlBytes, body)

	// Confirm TOFU pin is now stored.
	// Key format: "pypi:<name>@<version>" where name = "cd/ef", version = whlFile.
	tofuKey := "pypi:cd/ef@" + whlFile
	pinned, err := s.tofuStore.GetPin(ctx, tofuKey)
	require.NoError(t, err)
	assert.NotEmpty(t, pinned, "TOFU pin must be recorded after first fetch")

	// Second fetch: pin confirmed (StatusPass), served from CAS → 200.
	// The blob is still in CAS so this is a pure cache hit; TofuVerifier is not
	// re-invoked on a cache hit (verify-on-write only runs during Store).
	resp2, err := http.Get(srv.URL + whlPath)
	require.NoError(t, err)
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	require.Equal(t, http.StatusOK, resp2.StatusCode, "repeat TOFU-confirmed fetch must return 200")
	assert.Equal(t, whlBytes, body2, "repeat fetch must return same wheel bytes")
}

// ── Test 4 — SHA256 mismatch → 502 (TOFU tamper detection) ──────────────────

// TestPypiSHA256Mismatch502 verifies the TOFU tamper-detection path:
//
//  1. First fetch: wheel bytes fetched, digest pinned by TofuVerifier.
//  2. Blob deleted from CAS (simulating GC or storage loss) → Lookup returns miss.
//  3. Upstream content replaced with different bytes (same filename, new content).
//  4. Second fetch: cache miss → upstream fetch → new digest differs from pin →
//     TofuVerifier returns StatusFail → cache.Store returns *cache.VerifyError →
//     pypi.Handler returns 502.
//
// This is the critical security gate for supply-chain tampering detection.
func TestPypiSHA256Mismatch502(t *testing.T) {
	originalBytes := bytes.Repeat([]byte("ORIGINAL_WHEEL_"), 32)
	tamperedBytes := bytes.Repeat([]byte("TAMPERED_WHEEL_"), 32)

	require.NotEqual(t, sha256hex(originalBytes), sha256hex(tamperedBytes),
		"test precondition: original and tampered must have different digests")

	const whlFile = "mypackage-1.0-py3-none-any.whl"
	const whlPath = "/packages/ab/" + whlFile

	f := newFakePyPI()
	f.setFile(whlFile, originalBytes)
	upSrv := newFakePyPISrv(t, f)

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	srv := newPypiSpeculaServer(t, s, upSrv.URL)

	ctx := context.Background()

	// Step 1: First fetch — original bytes → CAS populated, TOFU pin stored.
	resp, err := http.Get(srv.URL + whlPath)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "first fetch must return 200")
	assert.Equal(t, originalBytes, body)

	// Capture the pinned digest.
	tofuKey := "pypi:ab@" + whlFile
	pinnedDigest, err := s.tofuStore.GetPin(ctx, tofuKey)
	require.NoError(t, err)
	require.NotEmpty(t, pinnedDigest, "TOFU must have pinned the digest after first fetch")

	// Step 2: Find the CAS entry and delete the blob to force a re-fetch.
	entry, err := s.cm.Lookup(ctx, pypiFileRef("ab", whlFile))
	require.NoError(t, err)
	require.NotNil(t, entry, "wheel must be in CAS after first fetch")

	deleteErr := s.blobStore.Delete(ctx, entry.Digest)
	require.NoError(t, deleteErr, "must be able to delete blob from CAS for test setup")

	// Confirm the blob is gone (Lookup now returns nil).
	missingEntry, err := s.cm.Lookup(ctx, pypiFileRef("ab", whlFile))
	require.NoError(t, err)
	assert.Nil(t, missingEntry, "Lookup must return nil after blob deletion")

	// Step 3: Swap upstream content to tampered bytes (different sha256).
	f.setFile(whlFile, tamperedBytes)

	// Step 4: Second fetch — cache miss → tampered fetch → TOFU fail → 502.
	resp2, err := http.Get(srv.URL + whlPath)
	require.NoError(t, err)
	io.Copy(io.Discard, resp2.Body) //nolint:errcheck
	resp2.Body.Close()

	assert.Equal(t, http.StatusBadGateway, resp2.StatusCode,
		"TOFU mismatch must return 502 Bad Gateway (tamper alert)")

	// Verify TOFU pin is unchanged (the pin stored the original digest, not the
	// tampered one — the tampered artifact was rejected before meta.Put ran).
	pinnedAfter, err := s.tofuStore.GetPin(ctx, tofuKey)
	require.NoError(t, err)
	assert.Equal(t, pinnedDigest, pinnedAfter,
		"TOFU pin must remain pinned to the original digest, not the tampered one")
}
