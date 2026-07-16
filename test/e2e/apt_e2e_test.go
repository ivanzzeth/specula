//go:build integration

// Package e2e — hermetic end-to-end tests for the Specula APT handler.
//
// Every test runs entirely in-process: a minimal httptest.Server acts as the fake
// APT upstream, and the real Specula apt.Handler is wired with LocalDiskDriver +
// SQLiteStore + verify.Chain (ChecksumVerifier + TofuVerifier + GPGVerifier) +
// cache.New, exactly as production. No external network access is needed.
//
// # Test fixtures
//
// A test GPG key is generated once per test (openpgp.NewEntity) and used to
// sign an InRelease clear-signed message. The Packages index is built from a
// small in-memory fake .deb, and the SHA256s are embedded in both InRelease and
// Packages so the full chain passes end-to-end.
//
// # What is tested
//
//   - TestAptRouting               — routing layer: 405 (wrong method), 404 (bad path).
//   - TestAptSignedChainPass       — full chain: InRelease → Packages → .deb; all pass
//     TierSigned when the GPGVerifier is wired into the Chain.
//   - TestAptTamperedDebFAIL       — a .deb whose actual bytes differ from the SHA256
//     listed in Packages causes a verify-on-write failure; the CAS stays clean.
//   - TestAptDistsCachingMutable   — dists/ metadata is mutable: a second fetch within
//     the TTL is served from cache (upstream not re-contacted); after the TTL expires
//     the upstream is contacted again.
//   - TestAptPoolCachingImmutable  — pool/*.deb is immutable: after the first fetch
//     populates the CAS the upstream receives no additional requests.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
	apthandler "github.com/ivanzzeth/specula/internal/handler/apt"
	"github.com/ivanzzeth/specula/internal/store/local"
	"github.com/ivanzzeth/specula/internal/store/sqlite"
	"github.com/ivanzzeth/specula/internal/upstream"
	"github.com/ivanzzeth/specula/internal/verify"
)

// ── APT test fixtures ─────────────────────────────────────────────────────────

const (
	aptSuite     = "noble"
	aptComponent = "main"
	aptArch      = "amd64"
	aptPkgName   = "libfake"
	aptPkgVer    = "1.0.0"
	aptPoolDir   = "main/l/libfake"
)

// aptPkgFilename is the canonical .deb filename.
var aptPkgFilename = fmt.Sprintf("%s_%s_%s.deb", aptPkgName, aptPkgVer, aptArch)

// aptFixture holds all in-memory test data for a fake APT repository.
type aptFixture struct {
	entity      *openpgp.Entity // test signing key
	keyringPath string          // path to public keyring file
	debContent  []byte          // fake .deb bytes
	debSHA256   string          // hex SHA256 of debContent
	packages    []byte          // Packages index content
	pkgsSHA256  string          // hex SHA256 of packages
	inRelease   []byte          // clear-signed InRelease
}

// newAptFixture generates an in-memory APT repository fixture with a fresh test
// GPG key. The key is written to a temp keyring file so NewGPGVerifier can load
// it. The InRelease is signed with the private key and pins the SHA256 of the
// Packages index; Packages pins the SHA256 of the fake .deb.
func newAptFixture(t *testing.T) *aptFixture {
	t.Helper()

	// 1. Generate a test GPG key.
	entity, err := openpgp.NewEntity("Test Repo", "Specula e2e", "repo@test.example", nil)
	require.NoError(t, err, "generate test GPG key")

	// 2. Write the public key to a temp keyring file (armored).
	keyringPath := filepath.Join(t.TempDir(), "test-keyring.asc")
	{
		f, err := os.Create(keyringPath)
		require.NoError(t, err, "create keyring file")
		armorWriter, err := armor.Encode(f, "PGP PUBLIC KEY BLOCK", nil)
		require.NoError(t, err, "armor encode")
		require.NoError(t, entity.Serialize(armorWriter), "serialize public key")
		require.NoError(t, armorWriter.Close(), "close armor writer")
		require.NoError(t, f.Close(), "close keyring file")
	}

	// 3. Create fake .deb content.
	debContent := []byte("fake debian package bytes for libfake_1.0.0_amd64.deb")
	debHex := sha256Hex(debContent)

	// 4. Build a Packages index that lists the fake .deb.
	poolPath := fmt.Sprintf("pool/%s/%s", aptPoolDir, aptPkgFilename)
	packages := buildPackagesIndex(aptPkgName, aptPkgVer, aptArch, poolPath, debContent)
	pkgsHex := sha256Hex(packages)

	// 5. Build and GPG-sign the InRelease content.
	//    The relative path in InRelease is from "dists/<suite>/" → "main/binary-amd64/Packages"
	pkgsRelPath := fmt.Sprintf("%s/binary-%s/Packages", aptComponent, aptArch)
	inReleaseBody := buildInReleaseBody(aptSuite, pkgsRelPath, pkgsHex, int64(len(packages)))
	inRelease := signInRelease(t, entity, inReleaseBody)

	return &aptFixture{
		entity:      entity,
		keyringPath: keyringPath,
		debContent:  debContent,
		debSHA256:   debHex,
		packages:    packages,
		pkgsSHA256:  pkgsHex,
		inRelease:   inRelease,
	}
}

// buildPackagesIndex creates a minimal Packages stanza listing the given .deb.
func buildPackagesIndex(name, version, arch, filename string, content []byte) []byte {
	h := sha256.Sum256(content)
	sha256hex := hex.EncodeToString(h[:])
	s := fmt.Sprintf(`Package: %s
Version: %s
Architecture: %s
Installed-Size: %d
Filename: %s
Size: %d
SHA256: %s

`, name, version, arch, len(content)/1024+1, filename, len(content), sha256hex)
	return []byte(s)
}

// buildInReleaseBody constructs the plaintext body of an InRelease file
// (before GPG signing) with the given SHA256 for a single Packages file.
func buildInReleaseBody(suite, pkgsRelPath, pkgsHex string, pkgsSize int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Origin: Specula-Test\n")
	fmt.Fprintf(&b, "Label: Specula-Test\n")
	fmt.Fprintf(&b, "Suite: %s\n", suite)
	fmt.Fprintf(&b, "Codename: %s\n", suite)
	fmt.Fprintf(&b, "Components: main\n")
	fmt.Fprintf(&b, "Architectures: amd64\n")
	fmt.Fprintf(&b, "SHA256:\n")
	fmt.Fprintf(&b, " %s %d %s\n", pkgsHex, pkgsSize, pkgsRelPath)
	return b.String()
}

// signInRelease produces a PGP clear-signed InRelease message using the given
// entity's private key.
func signInRelease(t *testing.T, entity *openpgp.Entity, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := clearsign.Encode(&buf, entity.PrivateKey, nil)
	require.NoError(t, err, "clearsign.Encode")
	_, err = io.WriteString(w, body)
	require.NoError(t, err, "write InRelease body")
	require.NoError(t, w.Close(), "close clearsign writer")
	return buf.Bytes()
}

// sha256Hex returns the hex-encoded SHA256 of data.
// (Distinct from sha256hex in oci_e2e_test.go to avoid naming collision;
// sha256Hex (capital H) is defined only in this file for clarity.)
func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// ── Fake APT upstream ─────────────────────────────────────────────────────────

// aptCounters holds per-path atomic hit counters for the fake APT upstream.
type aptCounters struct {
	inRelease int64
	packages  int64
	deb       int64
}

// newFakeAptUpstream creates a fake APT upstream server serving the fixture's
// artifacts and returns the server and request counters.
//
// The server's mux is configured for a minimal single-suite, single-component
// repository layout. Paths:
//
//	/dists/noble/InRelease                          → signed release index
//	/dists/noble/main/binary-amd64/Packages         → packages index
//	/pool/main/l/libfake/libfake_1.0.0_amd64.deb   → fake package
func newFakeAptUpstream(t *testing.T, fix *aptFixture) (*httptest.Server, *aptCounters) {
	t.Helper()
	cnt := &aptCounters{}
	mux := http.NewServeMux()

	mux.HandleFunc("/dists/"+aptSuite+"/InRelease", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&cnt.inRelease, 1)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(fix.inRelease)
	})

	pkgsPath := fmt.Sprintf("/dists/%s/%s/binary-%s/Packages", aptSuite, aptComponent, aptArch)
	mux.HandleFunc(pkgsPath, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&cnt.packages, 1)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(fix.packages)
	})

	debPath := fmt.Sprintf("/pool/%s/%s", aptPoolDir, aptPkgFilename)
	mux.HandleFunc(debPath, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&cnt.deb, 1)
		w.Header().Set("Content-Type", "application/vnd.debian.binary-package")
		_, _ = w.Write(fix.debContent)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, cnt
}

// ── APT test stack ────────────────────────────────────────────────────────────

// aptStack is like speculaStack but includes a GPGVerifier in the chain so that
// the apt signed-chain tests can verify TierSigned behaviour end-to-end.
type aptStack struct {
	dir         string
	blobStore   *local.LocalDiskDriver
	metaStore   *sqlite.SQLiteStore
	cm          cache.CacheManager
	gpgVerifier *verify.GPGVerifier
}

// newAptStack wires LocalDiskDriver + SQLiteStore +
// verify.Chain(Checksum, Tofu, GPG) + cache.New for the apt e2e tests.
func newAptStack(t *testing.T, dir string, keyringPath string) *aptStack {
	t.Helper()

	blobDir := filepath.Join(dir, "blobs")
	require.NoError(t, os.MkdirAll(blobDir, 0o755))

	ms, err := sqlite.NewSQLiteStore(filepath.Join(dir, "specula.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = ms.Close() })

	ts := newInMemTofuStore()
	bs := local.NewLocalDiskDriver(blobDir)

	gpgV, err := verify.NewGPGVerifier(keyringPath)
	require.NoError(t, err, "NewGPGVerifier with test keyring")

	chain := verify.NewChain(
		verify.NewChecksumVerifier(),
		verify.NewTofuVerifier(ts),
		gpgV,
	)

	return &aptStack{
		dir:         dir,
		blobStore:   bs,
		metaStore:   ms,
		cm:          cache.New(bs, ms, chain),
		gpgVerifier: gpgV,
	}
}

// newAptServer wires an apt.Handler over the given aptStack and upstream URL,
// and returns a running httptest.Server.
func newAptServer(t *testing.T, s *aptStack, upstreamURL string, mutableTTL int64, extra ...apthandler.Option) *httptest.Server {
	t.Helper()

	ups := []upstream.Upstream{{Name: "fake-apt", BaseURL: upstreamURL, Priority: 0}}
	opts := []apthandler.Option{
		apthandler.WithMeta(s.metaStore),
		apthandler.WithUpstream(upstream.NewClient(), ups),
		apthandler.WithQuarantineDir(s.dir),
		apthandler.WithMutableTTL(mutableTTL),
		apthandler.WithGPGVerifier(s.gpgVerifier),
	}
	opts = append(opts, extra...)

	h := apthandler.NewHandler(s.cm, opts...)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// ── Helper GET ────────────────────────────────────────────────────────────────

// aptGet performs an HTTP GET and returns (status, body).
func aptGet(t *testing.T, url string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(url) //nolint:noctx
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, body
}

// ── Test 1 — Routing ─────────────────────────────────────────────────────────

// TestAptRouting verifies the handler's routing and validation layer without
// any upstream.
func TestAptRouting(t *testing.T) {
	tmp := t.TempDir()

	// Use a plain speculaStack (no GPGVerifier needed for routing tests).
	s := newSpeculaStack(t, tmp)
	h := apthandler.NewHandler(s.cm)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	t.Run("post_method_405", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/dists/noble/InRelease", nil)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode, "POST must return 405")
	})

	t.Run("unknown_path_404", func(t *testing.T) {
		status, _ := aptGet(t, srv.URL+"/not/an/apt/path")
		assert.Equal(t, http.StatusNotFound, status, "unknown path must return 404")
	})

	t.Run("no_upstream_dists_404", func(t *testing.T) {
		// No upstream → cache miss → 404.
		status, _ := aptGet(t, srv.URL+"/dists/noble/InRelease")
		assert.Equal(t, http.StatusNotFound, status, "dists/ without upstream must return 404")
	})

	t.Run("no_upstream_pool_404", func(t *testing.T) {
		// No upstream → cache miss → 404.
		status, _ := aptGet(t, srv.URL+"/pool/main/l/libfake/libfake_1.0.0_amd64.deb")
		assert.Equal(t, http.StatusNotFound, status, "pool/ without upstream must return 404")
	})

	t.Run("path_traversal_404", func(t *testing.T) {
		// Paths containing ".." are rejected by the routing guard.
		status, _ := aptGet(t, srv.URL+"/dists/../etc/passwd")
		assert.Equal(t, http.StatusNotFound, status, "path traversal must return 404")
	})
}

// ── Test 2 — Full signed chain PASS ───────────────────────────────────────────

// TestAptSignedChainPass verifies the full end-to-end GPG chain:
//
//  1. GET /dists/<suite>/InRelease → GPGVerifier verifies the clear-signed
//     message, caches the Packages SHA256 sums.
//  2. GET /dists/<suite>/main/binary-amd64/Packages → GPGVerifier verifies
//     the SHA256 against InRelease; caches .deb SHA256 sums.
//  3. GET /pool/main/l/libfake/libfake_1.0.0_amd64.deb → GPGVerifier verifies
//     the SHA256 against the Packages index; returns TierSigned.
//
// All three fetches must return 200 with the expected bytes.
func TestAptSignedChainPass(t *testing.T) {
	tmp := t.TempDir()
	fix := newAptFixture(t)
	fakeUp, cnt := newFakeAptUpstream(t, fix)

	s := newAptStack(t, tmp, fix.keyringPath)
	srv := newAptServer(t, s, fakeUp.URL, 300)

	// Step 1: fetch InRelease.
	inReleasePath := fmt.Sprintf("/dists/%s/InRelease", aptSuite)
	status, body := aptGet(t, srv.URL+inReleasePath)
	require.Equal(t, http.StatusOK, status, "InRelease must return 200")
	assert.Equal(t, fix.inRelease, body, "InRelease bytes must match upstream")
	assert.EqualValues(t, 1, atomic.LoadInt64(&cnt.inRelease), "InRelease upstream hit once")

	// Step 2: fetch Packages.
	pkgsPath := fmt.Sprintf("/dists/%s/%s/binary-%s/Packages", aptSuite, aptComponent, aptArch)
	status, body = aptGet(t, srv.URL+pkgsPath)
	require.Equal(t, http.StatusOK, status, "Packages must return 200")
	assert.Equal(t, fix.packages, body, "Packages bytes must match upstream")
	assert.EqualValues(t, 1, atomic.LoadInt64(&cnt.packages), "Packages upstream hit once")

	// Step 3: fetch the pool .deb (full chain — must pass).
	debPath := fmt.Sprintf("/pool/%s/%s", aptPoolDir, aptPkgFilename)
	status, body = aptGet(t, srv.URL+debPath)
	require.Equal(t, http.StatusOK, status, ".deb must return 200 (full chain PASS)")
	assert.Equal(t, fix.debContent, body, ".deb bytes must match upstream")
	assert.EqualValues(t, 1, atomic.LoadInt64(&cnt.deb), ".deb upstream hit once")

	// CAS must contain the .deb blob.
	ctx := context.Background()
	used, err := s.blobStore.UsageBytes(ctx)
	require.NoError(t, err)
	assert.Positive(t, used, "CAS must have bytes after .deb fetch")

	// Per-protocol stats must show apt objects.
	stats, err := s.metaStore.CacheSizeByProtocol(ctx)
	require.NoError(t, err)
	aptStat, ok := stats["apt"]
	require.True(t, ok, "apt stats must exist after fetches")
	assert.Positive(t, aptStat.Objects, "apt object count must be positive")
}

// ── Test 3 — Tampered .deb → FAIL ────────────────────────────────────────────

// TestAptTamperedDebFAIL verifies that a .deb whose content has been tampered
// (different bytes from what the Packages index lists) causes a verify-on-write
// failure: the handler returns 502 and the CAS stays clean.
//
// The test first fetches InRelease and Packages normally (correct content), then
// replaces the upstream .deb handler with tampered bytes. The third request (pool
// .deb) must fail at the GPGVerifier (SHA256 mismatch against Packages).
func TestAptTamperedDebFAIL(t *testing.T) {
	tmp := t.TempDir()
	fix := newAptFixture(t)

	// Switch between real and tampered .deb by index.
	var serveTampered atomic.Bool // false = real, true = tampered
	tamperedContent := []byte("THIS IS TAMPERED CONTENT — SHA256 differs from Packages listing")

	mux := http.NewServeMux()
	mux.HandleFunc("/dists/"+aptSuite+"/InRelease", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(fix.inRelease)
	})
	pkgsPath := fmt.Sprintf("/dists/%s/%s/binary-%s/Packages", aptSuite, aptComponent, aptArch)
	mux.HandleFunc(pkgsPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(fix.packages)
	})
	debPath := fmt.Sprintf("/pool/%s/%s", aptPoolDir, aptPkgFilename)
	mux.HandleFunc(debPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.debian.binary-package")
		if serveTampered.Load() {
			_, _ = w.Write(tamperedContent)
		} else {
			_, _ = w.Write(fix.debContent)
		}
	})
	fakeUp := httptest.NewServer(mux)
	t.Cleanup(fakeUp.Close)

	s := newAptStack(t, tmp, fix.keyringPath)
	srv := newAptServer(t, s, fakeUp.URL, 300)

	// Step 1+2: fetch InRelease and Packages normally (must succeed).
	inReleasePath := fmt.Sprintf("/dists/%s/InRelease", aptSuite)
	status, _ := aptGet(t, srv.URL+inReleasePath)
	require.Equal(t, http.StatusOK, status, "InRelease step must succeed")

	status, _ = aptGet(t, srv.URL+pkgsPath)
	require.Equal(t, http.StatusOK, status, "Packages step must succeed")

	// Switch the upstream to tampered content.
	serveTampered.Store(true)

	// Step 3: fetch the tampered .deb — must fail with 502 (verify-on-write).
	debHandlerPath := fmt.Sprintf("/pool/%s/%s", aptPoolDir, aptPkgFilename)
	status, body := aptGet(t, srv.URL+debHandlerPath)
	assert.Equal(t, http.StatusBadGateway, status,
		"tampered .deb must return 502 (verify-on-write chain failure), got body: %s", body)

	// CAS must NOT contain the tampered blob.
	ctx := context.Background()
	tamperedDigest := "sha256:" + sha256Hex(tamperedContent)
	existsTampered, err := s.blobStore.Exists(ctx, tamperedDigest)
	require.NoError(t, err)
	assert.False(t, existsTampered, "CAS must not contain the tampered .deb blob")
}

// ── Test 4 — dists/ mutable caching ──────────────────────────────────────────

// TestAptDistsCachingMutable verifies the two-tier caching model for dists/ metadata:
//
//   - InRelease and Packages are MUTABLE: a second request within the TTL is served
//     from the mutable cache without contacting the upstream.
//   - After TTL expiry (forced by backdating fetched_at, not by sleeping), the
//     upstream is re-contacted.
func TestAptDistsCachingMutable(t *testing.T) {
	// A long TTL so the "within TTL" assertions can never expire spuriously
	// under CPU contention; expiry is then forced deterministically by
	// backdating fetched_at (see ageMutableEntries) rather than by sleeping.
	const mutableTTL = int64(3600)

	tmp := t.TempDir()
	fix := newAptFixture(t)

	// Serve InRelease without the signed chain (no GPGVerifier needed for this
	// caching test — we just use the base speculaStack with ChecksumVerifier only).
	fakeUp, cnt := newFakeAptUpstream(t, fix)

	// Use the apt-specific stack (includes GPGVerifier) since the handler still
	// needs InRelease to parse correctly.
	s := newAptStack(t, tmp, fix.keyringPath)
	srv := newAptServer(t, s, fakeUp.URL, mutableTTL)

	inReleasePath := fmt.Sprintf("/dists/%s/InRelease", aptSuite)

	// Round 1: cold cache — upstream must be contacted.
	status, body := aptGet(t, srv.URL+inReleasePath)
	require.Equal(t, http.StatusOK, status, "round1 InRelease must return 200")
	assert.Equal(t, fix.inRelease, body, "round1 InRelease bytes must match")
	hits1 := atomic.LoadInt64(&cnt.inRelease)
	assert.EqualValues(t, 1, hits1, "round1: upstream hit exactly once")

	// Round 2: immediately re-request (within TTL) — must serve from mutable cache.
	status, body = aptGet(t, srv.URL+inReleasePath)
	require.Equal(t, http.StatusOK, status, "round2 InRelease must return 200 (cache hit)")
	assert.Equal(t, fix.inRelease, body, "round2 bytes must match")
	hits2 := atomic.LoadInt64(&cnt.inRelease)
	assert.Equal(t, hits1, hits2, "round2 (within TTL): upstream must NOT be re-contacted")

	// Force TTL expiry deterministically (no sleep, no wall-clock race).
	ageMutableEntries(t, s.metaStore, 2*time.Hour)

	// Round 3: post-TTL — upstream must be re-contacted.
	status, body = aptGet(t, srv.URL+inReleasePath)
	require.Equal(t, http.StatusOK, status, "round3 InRelease must return 200 (re-fetched)")
	assert.Equal(t, fix.inRelease, body, "round3 bytes must still match")
	hits3 := atomic.LoadInt64(&cnt.inRelease)
	assert.Greater(t, hits3, hits2,
		"round3 (TTL expired): upstream must be re-contacted; was %d, now %d", hits2, hits3)
}

// ── Test 5 — pool/ immutable caching ─────────────────────────────────────────

// TestAptPoolCachingImmutable verifies that pool/*.deb files are permanently
// cached in the CAS: after the first fetch populates the CAS, subsequent
// requests are served without contacting the upstream.
func TestAptPoolCachingImmutable(t *testing.T) {
	tmp := t.TempDir()
	fix := newAptFixture(t)
	fakeUp, cnt := newFakeAptUpstream(t, fix)

	s := newAptStack(t, tmp, fix.keyringPath)
	srv := newAptServer(t, s, fakeUp.URL, 300)

	// Prerequisite: fetch InRelease and Packages so the GPGVerifier can verify
	// the .deb SHA256 against the chain.
	inReleasePath := fmt.Sprintf("/dists/%s/InRelease", aptSuite)
	status, _ := aptGet(t, srv.URL+inReleasePath)
	require.Equal(t, http.StatusOK, status, "InRelease prerequisite must succeed")

	pkgsPath := fmt.Sprintf("/dists/%s/%s/binary-%s/Packages", aptSuite, aptComponent, aptArch)
	status, _ = aptGet(t, srv.URL+pkgsPath)
	require.Equal(t, http.StatusOK, status, "Packages prerequisite must succeed")

	debPath := fmt.Sprintf("/pool/%s/%s", aptPoolDir, aptPkgFilename)

	// Round 1: cold CAS — upstream must be contacted.
	status, body := aptGet(t, srv.URL+debPath)
	require.Equal(t, http.StatusOK, status, "round1 .deb must return 200")
	assert.Equal(t, fix.debContent, body, "round1 .deb bytes must match")
	debHits1 := atomic.LoadInt64(&cnt.deb)
	assert.EqualValues(t, 1, debHits1, "round1: .deb upstream hit exactly once")

	// Round 2: warm CAS — upstream must NOT be contacted.
	status, body = aptGet(t, srv.URL+debPath)
	require.Equal(t, http.StatusOK, status, "round2 .deb must return 200 (CAS hit)")
	assert.Equal(t, fix.debContent, body, "round2 .deb bytes from CAS must match")
	debHits2 := atomic.LoadInt64(&cnt.deb)
	assert.Equal(t, debHits1, debHits2, "round2: .deb upstream must NOT be re-contacted (immutable CAS)")
}

// ── Test 6 — verify-on-write unit (direct cache.Store) ───────────────────────

// TestAptVerifyOnWriteDirect confirms that the verify-on-write path uses the
// GPGVerifier's chain state: storing a pool artifact before the Packages index
// has been verified causes a fail-closed StatusFail / VerifyError.
//
// This test does NOT go through the HTTP handler — it calls cache.Store directly
// to isolate the verification contract.
func TestAptVerifyOnWriteDirect(t *testing.T) {
	tmp := t.TempDir()
	fix := newAptFixture(t)

	// Create the stack (GPGVerifier included in chain but no chain state populated).
	s := newAptStack(t, tmp, fix.keyringPath)
	ctx := context.Background()

	// Quarantine the real .deb bytes.
	art, cleanup, err := cache.Quarantine(ctx, tmp, bytes.NewReader(fix.debContent), artifact.UpstreamMeta{})
	require.NoError(t, err)
	defer cleanup()

	// Build a pool ref for the .deb.
	ref := artifact.ArtifactRef{
		Protocol: "apt",
		Name:     aptPoolDir,
		Version:  aptPkgFilename,
		Mutable:  false,
	}

	// Store with NO InRelease/Packages chain state — GPGVerifier must fail-closed.
	_, storeErr := s.cm.Store(ctx, ref, art)
	require.Error(t, storeErr, "Store must fail when pool SHA256 not in chain state")

	ve, isVerify := cache.AsVerifyError(storeErr)
	require.True(t, isVerify,
		"error must be *cache.VerifyError, got %T: %v", storeErr, storeErr)
	assert.Equal(t, artifact.StatusFail, ve.Result.Status,
		"verify status must be FAIL when chain state is absent")

	// Quarantine file must have been removed.
	_, statErr := os.Stat(art.Path)
	assert.True(t, os.IsNotExist(statErr),
		"quarantine file must be removed after verify failure")

	// CAS must be clean.
	exists, err := s.blobStore.Exists(ctx, art.Digest)
	require.NoError(t, err)
	assert.False(t, exists, "CAS must not contain blob after verify-on-write failure")
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// scanPackages is a simple sanity-check parser used only in tests — it confirms
// that the buildPackagesIndex output is valid enough for parsePackagesSHA256s.
func scanPackages(data []byte) map[string]string {
	result := make(map[string]string)
	var filename, sha256sum string
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if filename != "" && sha256sum != "" {
				result[filename] = sha256sum
			}
			filename, sha256sum = "", ""
			continue
		}
		if strings.HasPrefix(line, "Filename: ") {
			filename = strings.TrimPrefix(line, "Filename: ")
		}
		if strings.HasPrefix(line, "SHA256: ") {
			sha256sum = strings.TrimPrefix(line, "SHA256: ")
		}
	}
	if filename != "" && sha256sum != "" {
		result[filename] = sha256sum
	}
	return result
}
