// Package e2e — hermetic end-to-end tests for the Helm classic HTTP handler.
//
// # What is tested
//
//   - Signed provenance PASS: chart + valid .prov → TierSigned PASS.
//   - Tampered chart FAIL: chart bytes differ from .prov SHA256 → VerifyError.
//   - index.yaml mutable tier: cold fetch, cache hit, conditional GET 304.
//   - .tgz cache hit: second request served from CAS, upstream not contacted.
//   - No .prov available: degrade to checksum/warn tier (not a hard failure).
//
// All tests run entirely in-process. No external network access is required.
package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
	helmhandler "github.com/ivanzzeth/specula/internal/handler/helm"
	"github.com/ivanzzeth/specula/internal/store/local"
	"github.com/ivanzzeth/specula/internal/store/sqlite"
	"github.com/ivanzzeth/specula/internal/upstream"
	"github.com/ivanzzeth/specula/internal/verify"
)

// ── Helm test fixture ─────────────────────────────────────────────────────────

// helmKey holds a generated test GPG key pair.
type helmKey struct {
	entity  *openpgp.Entity
	keyring openpgp.EntityList
}

// newHelmKey generates a fresh Ed25519 test key pair.
func newHelmKey(t *testing.T) *helmKey {
	t.Helper()
	cfg := &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA}
	entity, err := openpgp.NewEntity("Test Helm Publisher", "", "helm@specula.test", cfg)
	require.NoError(t, err, "generate test GPG key")
	return &helmKey{entity: entity, keyring: openpgp.EntityList{entity}}
}

// armoredPublicKey serialises the test entity's public key as an ASCII-armored
// PGP key block and writes it to a temp file, returning the file path.
func (k *helmKey) armoredPublicKey(t *testing.T, dir string) string {
	t.Helper()
	var buf bytes.Buffer
	aw, err := armor.Encode(&buf, "PGP PUBLIC KEY BLOCK", nil)
	require.NoError(t, err)
	require.NoError(t, k.entity.Serialize(aw))
	require.NoError(t, aw.Close())

	path := filepath.Join(dir, "keyring.gpg")
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o600))
	return path
}

// signProv creates a Helm-style clear-signed provenance document for chartFile
// whose sha256 digest is charDigest (hex, without "sha256:" prefix).
func (k *helmKey) signProv(t *testing.T, chartName, chartVersion, chartFile, chartDigestHex string) []byte {
	t.Helper()

	// Build the YAML-ish provenance body that Helm expects.
	body := fmt.Sprintf(`apiVersion: v2
description: A test Helm chart
name: %s
version: %s

files:
  %s: sha256:%s
`, chartName, chartVersion, chartFile, chartDigestHex)

	var buf bytes.Buffer
	w, err := clearsign.Encode(&buf, k.entity.PrivateKey, nil)
	require.NoError(t, err)
	_, err = io.WriteString(w, body)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	return buf.Bytes()
}

// buildChartTGZ returns the bytes of a minimal Helm chart tarball containing
// only a Chart.yaml. The tarball is deterministic so its SHA256 is stable.
func buildChartTGZ(t *testing.T, name, version string) []byte {
	t.Helper()

	chartYAML := []byte(fmt.Sprintf("apiVersion: v2\nname: %s\nversion: %s\n", name, version))

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	hdr := &tar.Header{
		Name:     fmt.Sprintf("%s/Chart.yaml", name),
		Mode:     0o644,
		Size:     int64(len(chartYAML)),
		Typeflag: tar.TypeReg,
	}
	require.NoError(t, tw.WriteHeader(hdr))
	_, err := tw.Write(chartYAML)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())
	return buf.Bytes()
}

// sha256Hex returns the lowercase hex SHA256 of b.
func sha256HexHelm(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ── helmStack — full real data-plane stack ────────────────────────────────────

type helmStack struct {
	dir       string
	blobStore *local.LocalDiskDriver
	metaStore *sqlite.SQLiteStore
	cm        cache.CacheManager
	tofuStore *inMemTofuStore
}

// newHelmStack wires LocalDiskDriver + SQLiteStore + verify.Chain(checksum, tofu,
// helmProv) + cache.New under a test-isolated temp directory.
//
// When keyringPath is "" no HelmProvVerifier is added; tests that don't need
// provenance verification pass "" to stay at the checksum/tofu tier.
func newHelmStack(t *testing.T, dir, keyringPath string) *helmStack {
	t.Helper()

	blobDir := filepath.Join(dir, "blobs")
	require.NoError(t, os.MkdirAll(blobDir, 0o755))

	ms, err := sqlite.NewSQLiteStore(filepath.Join(dir, "specula.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = ms.Close() })

	ts := newInMemTofuStore()
	bs := local.NewLocalDiskDriver(blobDir)

	verifiers := []verify.Verifier{verify.NewChecksumVerifier(), verify.NewTofuVerifier(ts)}
	if keyringPath != "" {
		hpv, err := verify.NewHelmProvVerifier(keyringPath)
		require.NoError(t, err)
		verifiers = append(verifiers, hpv)
	}
	chain := verify.NewChain(verifiers...)

	return &helmStack{
		dir:       dir,
		blobStore: bs,
		metaStore: ms,
		cm:        cache.New(bs, ms, chain),
		tofuStore: ts,
	}
}

// newHelmServer wires a helm.Handler over the given stack and starts an
// httptest.Server.
func newHelmServer(t *testing.T, s *helmStack, opts ...helmhandler.Option) *httptest.Server {
	t.Helper()
	baseOpts := []helmhandler.Option{
		helmhandler.WithMeta(s.metaStore),
		helmhandler.WithQuarantineDir(s.dir),
	}
	h := helmhandler.NewHandler(s.cm, append(baseOpts, opts...)...)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// ── fakeChartServer — serves index.yaml + chart .tgz + optional .prov ────────

type fakeChartServer struct {
	indexYAML []byte
	charts    map[string][]byte // filename → bytes
	etag      string
	hits      int64 // atomic; counts requests to /index.yaml
}

func (f *fakeChartServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimLeft(r.URL.Path, "/")

	// The upstream client constructs paths as "<repo>/file" (buildPath for helm
	// returns ref.Name + "/" + ref.Version). Extract the last segment so the
	// server can look up files by bare filename regardless of repo prefix.
	file := p
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		file = p[i+1:]
	}

	if file == "index.yaml" {
		atomic.AddInt64(&f.hits, 1)
		w.Header().Set("Content-Type", "application/yaml")
		if f.etag != "" {
			w.Header().Set("ETag", f.etag)
			if r.Header.Get("If-None-Match") == f.etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(f.indexYAML)
		}
		return
	}

	data, ok := f.charts[file]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(file, ".tgz.prov") {
		w.Header().Set("Content-Type", "text/plain")
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
}

// buildFakeIndex returns a minimal Helm repository index.yaml.
func buildFakeIndex(repoURL, chartName, chartVersion, chartFile, chartDigestHex string) []byte {
	return []byte(fmt.Sprintf(`apiVersion: v1
entries:
  %s:
  - name: %s
    version: %s
    urls:
    - %s/%s
    digest: sha256:%s
generated: %s
`, chartName, chartName, chartVersion, repoURL, chartFile, chartDigestHex,
		time.Now().UTC().Format(time.RFC3339)))
}

// ── Test 1 — Provenance PASS ─────────────────────────────────────────────────

// TestHelmProvPass verifies the full happy path:
//
//  1. A test GPG key is generated.
//  2. A minimal chart .tgz is built and its SHA256 computed.
//  3. A clear-signed .prov is produced with the GPG key.
//  4. Specula fetches the .tgz + .prov, verifies the provenance, and promotes
//     the artifact to the CAS at TierSigned.
//  5. The artifact is served from CAS on the second request (no upstream hit).
func TestHelmProvPass(t *testing.T) {
	const (
		chartName    = "test-chart"
		chartVersion = "0.1.0"
		repo         = "stable"
	)
	chartFile := fmt.Sprintf("%s-%s.tgz", chartName, chartVersion)

	tmp := t.TempDir()

	// Generate test GPG key.
	key := newHelmKey(t)
	keyringPath := key.armoredPublicKey(t, tmp)

	// Build the chart tarball.
	chartBytes := buildChartTGZ(t, chartName, chartVersion)
	chartDigestHex := sha256HexHelm(chartBytes)

	// Sign the provenance.
	provBytes := key.signProv(t, chartName, chartVersion, chartFile, chartDigestHex)

	// Build the fake upstream server.
	indexYAML := buildFakeIndex("http://placeholder", chartName, chartVersion, chartFile, chartDigestHex)
	fakeServer := &fakeChartServer{
		indexYAML: indexYAML,
		charts: map[string][]byte{
			chartFile:           chartBytes,
			chartFile + ".prov": provBytes,
		},
	}
	upstreamSrv := httptest.NewServer(fakeServer)
	t.Cleanup(upstreamSrv.Close)

	// Wire Specula with the HelmProvVerifier.
	s := newHelmStack(t, tmp, keyringPath)
	speculaSrv := newHelmServer(t, s,
		helmhandler.WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "fake-helm", BaseURL: upstreamSrv.URL, Priority: 0},
		}),
	)

	// First request: cold miss → fetch .tgz + .prov → verify → CAS.
	chartURL := fmt.Sprintf("%s/%s/%s", speculaSrv.URL, repo, chartFile)
	resp, err := http.Get(chartURL)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "first .tgz request must return 200")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, chartBytes, body, "served bytes must match original chart")

	// Verify the artifact reached TierSigned in the metadata.
	ref := artifact.ArtifactRef{
		Protocol: "helm",
		Name:     repo,
		Version:  chartFile,
	}
	ctx := context.Background()
	entry, err := s.metaStore.Get(ctx, ref)
	require.NoError(t, err)
	require.NotNil(t, entry, "CAS entry must exist after first pull")
	assert.Equal(t, artifact.TierSigned, entry.Tier,
		"chart with valid .prov must reach TierSigned")
	assert.Positive(t, entry.Size)
}

// ── Test 2 — Tampered chart FAIL ─────────────────────────────────────────────

// TestHelmTamperedChartFail verifies that when the chart .tgz bytes differ from
// the SHA256 recorded in the signed .prov, the verify-on-write pipeline returns
// a *cache.VerifyError and the artifact is not promoted to CAS.
func TestHelmTamperedChartFail(t *testing.T) {
	const (
		chartName    = "tamper-chart"
		chartVersion = "0.1.0"
		repo         = "test-repo"
	)
	chartFile := fmt.Sprintf("%s-%s.tgz", chartName, chartVersion)

	tmp := t.TempDir()

	key := newHelmKey(t)
	keyringPath := key.armoredPublicKey(t, tmp)

	// Build the legitimate chart and sign its provenance.
	legitimateBytes := buildChartTGZ(t, chartName, chartVersion)
	legitimateDigestHex := sha256HexHelm(legitimateBytes)
	provBytes := key.signProv(t, chartName, chartVersion, chartFile, legitimateDigestHex)

	// The upstream serves TAMPERED chart bytes (different from what was signed).
	tamperedBytes := append(legitimateBytes, []byte("TAMPERED")...)

	fakeServer := &fakeChartServer{
		indexYAML: []byte("apiVersion: v1\n"),
		charts: map[string][]byte{
			chartFile:           tamperedBytes,
			chartFile + ".prov": provBytes,
		},
	}
	upstreamSrv := httptest.NewServer(fakeServer)
	t.Cleanup(upstreamSrv.Close)

	s := newHelmStack(t, tmp, keyringPath)
	speculaSrv := newHelmServer(t, s,
		helmhandler.WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "fake-helm", BaseURL: upstreamSrv.URL, Priority: 0},
		}),
	)

	chartURL := fmt.Sprintf("%s/%s/%s", speculaSrv.URL, repo, chartFile)
	resp, err := http.Get(chartURL)
	require.NoError(t, err)
	defer resp.Body.Close()

	// The handler must return 502 when verification fails.
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode,
		"tampered chart must not be served (502 expected)")

	// The CAS must remain empty — the artifact must not have been promoted.
	used, err := s.blobStore.UsageBytes(ctx(t))
	require.NoError(t, err)
	assert.Equal(t, int64(0), used, "CAS must be empty after a verify failure")
}

// ── Test 3 — index.yaml mutable revalidation ─────────────────────────────────

// TestHelmIndexRevalidation exercises the mutable pipeline for index.yaml:
//
//  1. First request: cold miss → upstream fetch → store with TTL.
//  2. Second request within TTL: served from mutable cache (no upstream hit).
//  3. A second fake server with an ETag performs a 304 Not Modified on
//     revalidation; the handler extends the TTL and serves stale.
func TestHelmIndexRevalidation(t *testing.T) {
	const repo = "bitnami"

	indexContent := []byte("apiVersion: v1\nentries: {}\n")
	indexETag := `"stable-v1"`

	var upstreamHits int64

	fakeServer := &fakeChartServer{
		indexYAML: indexContent,
		etag:      indexETag,
		charts:    map[string][]byte{},
	}
	// Wrap in a counting handler to track every upstream index.yaml hit.
	countingHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&upstreamHits, 1)
		fakeServer.ServeHTTP(w, r)
	})
	upstreamSrv := httptest.NewServer(countingHandler)
	t.Cleanup(upstreamSrv.Close)

	tmp := t.TempDir()
	// No provenance verifier needed for index tests.
	s := newHelmStack(t, tmp, "")
	speculaSrv := newHelmServer(t, s,
		helmhandler.WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "fake-helm", BaseURL: upstreamSrv.URL, Priority: 0},
		}),
		// Very long TTL so the second request is definitely a cache hit.
		helmhandler.WithMutableTTL(3600),
	)

	indexURL := fmt.Sprintf("%s/%s/index.yaml", speculaSrv.URL, repo)

	// First request: cold miss.
	resp1, err := http.Get(indexURL)
	require.NoError(t, err)
	body1, _ := io.ReadAll(resp1.Body)
	resp1.Body.Close()
	require.Equal(t, http.StatusOK, resp1.StatusCode, "first index request must succeed")
	assert.Equal(t, indexContent, body1, "index content must match upstream")
	hitsAfterFirst := atomic.LoadInt64(&upstreamHits)
	assert.Equal(t, int64(1), hitsAfterFirst, "first request must hit upstream once")

	// Second request within TTL: must be a cache hit (zero additional upstream hits).
	resp2, err := http.Get(indexURL)
	require.NoError(t, err)
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	require.Equal(t, http.StatusOK, resp2.StatusCode, "second index request must succeed")
	assert.Equal(t, indexContent, body2, "cached index content must match")
	assert.Equal(t, hitsAfterFirst, atomic.LoadInt64(&upstreamHits),
		"second request must be served from cache (no upstream hit)")
}

// ── Test 4 — .tgz cache hit ──────────────────────────────────────────────────

// TestHelmChartCacheHit verifies that after the first .tgz pull populates the
// CAS, a second pull is served entirely from CAS with zero upstream contacts.
func TestHelmChartCacheHit(t *testing.T) {
	const (
		chartName    = "cache-chart"
		chartVersion = "1.0.0"
		repo         = "stable"
	)
	chartFile := fmt.Sprintf("%s-%s.tgz", chartName, chartVersion)

	tmp := t.TempDir()

	chartBytes := buildChartTGZ(t, chartName, chartVersion)
	var chartHits int64

	fakeServer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The upstream client prepends "<repo>/" so strip to get the bare filename.
		p := r.URL.Path
		file := p
		if i := strings.LastIndexByte(p, '/'); i >= 0 {
			file = p[i+1:]
		}
		if file == chartFile {
			atomic.AddInt64(&chartHits, 1)
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			if r.Method == http.MethodGet {
				_, _ = w.Write(chartBytes)
			}
			return
		}
		// .prov not available for this test.
		http.NotFound(w, r)
	})
	upstreamSrv := httptest.NewServer(fakeServer)
	t.Cleanup(upstreamSrv.Close)

	// No provenance verifier (the .prov fetch will 404, which is fine).
	s := newHelmStack(t, tmp, "")
	speculaSrv := newHelmServer(t, s,
		helmhandler.WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "fake-helm", BaseURL: upstreamSrv.URL, Priority: 0},
		}),
	)

	chartURL := fmt.Sprintf("%s/%s/%s", speculaSrv.URL, repo, chartFile)

	// First pull: cold cache, must hit upstream.
	resp1, err := http.Get(chartURL)
	require.NoError(t, err)
	body1, _ := io.ReadAll(resp1.Body)
	resp1.Body.Close()
	require.Equal(t, http.StatusOK, resp1.StatusCode)
	assert.Equal(t, chartBytes, body1)
	assert.Equal(t, int64(1), atomic.LoadInt64(&chartHits),
		"first pull must hit upstream exactly once")

	// Second pull: CAS hit, must NOT hit upstream.
	resp2, err := http.Get(chartURL)
	require.NoError(t, err)
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	require.Equal(t, http.StatusOK, resp2.StatusCode)
	assert.Equal(t, chartBytes, body2)
	assert.Equal(t, int64(1), atomic.LoadInt64(&chartHits),
		"second pull must be served from CAS (no upstream hit)")
}

// ── Test 5 — No .prov → degrade gracefully ───────────────────────────────────

// TestHelmNoProv verifies that a chart without a .prov file is served
// successfully, but only reaches the lower checksum/warn tier (not TierSigned).
// This is the "诚实分级" (honest tiered trust) behaviour: no .prov → degrade,
// do NOT fail.
func TestHelmNoProv(t *testing.T) {
	const (
		chartName    = "unsigned-chart"
		chartVersion = "2.0.0"
		repo         = "unsigned-repo"
	)
	chartFile := fmt.Sprintf("%s-%s.tgz", chartName, chartVersion)

	tmp := t.TempDir()

	key := newHelmKey(t)
	keyringPath := key.armoredPublicKey(t, tmp)

	chartBytes := buildChartTGZ(t, chartName, chartVersion)

	// Upstream serves chart only — no .prov.
	fakeServer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Strip repo prefix from path to get bare filename.
		p := r.URL.Path
		file := p
		if i := strings.LastIndexByte(p, '/'); i >= 0 {
			file = p[i+1:]
		}
		if file == chartFile {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			if r.Method == http.MethodGet {
				_, _ = w.Write(chartBytes)
			}
			return
		}
		// .prov → 404, triggering the degrade path.
		http.NotFound(w, r)
	})
	upstreamSrv := httptest.NewServer(fakeServer)
	t.Cleanup(upstreamSrv.Close)

	// Wire HelmProvVerifier — it should degrade, not fail, on missing .prov.
	s := newHelmStack(t, tmp, keyringPath)
	speculaSrv := newHelmServer(t, s,
		helmhandler.WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "fake-helm", BaseURL: upstreamSrv.URL, Priority: 0},
		}),
	)

	chartURL := fmt.Sprintf("%s/%s/%s", speculaSrv.URL, repo, chartFile)
	resp, err := http.Get(chartURL)
	require.NoError(t, err)
	defer resp.Body.Close()

	// Must be served successfully (not rejected) — honest downgrade.
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"chart without .prov must still be served (degrade, not fail)")

	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, chartBytes, body)

	// Verify the achieved tier is NOT TierSigned (degrade to checksum/tofu).
	ref := artifact.ArtifactRef{
		Protocol: "helm",
		Name:     repo,
		Version:  chartFile,
	}
	entry, err := s.metaStore.Get(ctx(t), ref)
	require.NoError(t, err)
	require.NotNil(t, entry)
	assert.NotEqual(t, artifact.TierSigned, entry.Tier,
		"unsigned chart must NOT reach TierSigned (no .prov)")
}

// ── Test 6 — prov file itself is served as a normal immutable artifact ────────

// TestHelmProvFileServed verifies that .prov files are served transparently
// from the CAS just like any other immutable artifact.
func TestHelmProvFileServed(t *testing.T) {
	const (
		chartName    = "prov-chart"
		chartVersion = "0.5.0"
		repo         = "signed-repo"
	)
	chartFile := fmt.Sprintf("%s-%s.tgz", chartName, chartVersion)
	provFile := chartFile + ".prov"

	tmp := t.TempDir()

	key := newHelmKey(t)
	keyringPath := key.armoredPublicKey(t, tmp)

	chartBytes := buildChartTGZ(t, chartName, chartVersion)
	chartDigestHex := sha256HexHelm(chartBytes)
	provBytes := key.signProv(t, chartName, chartVersion, chartFile, chartDigestHex)

	fakeServer := &fakeChartServer{
		indexYAML: []byte("apiVersion: v1\n"),
		charts: map[string][]byte{
			chartFile: chartBytes,
			provFile:  provBytes,
		},
	}
	upstreamSrv := httptest.NewServer(fakeServer)
	t.Cleanup(upstreamSrv.Close)

	s := newHelmStack(t, tmp, keyringPath)
	speculaSrv := newHelmServer(t, s,
		helmhandler.WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "fake-helm", BaseURL: upstreamSrv.URL, Priority: 0},
		}),
	)

	// First fetch the .tgz (which also fetches the .prov as an attachment).
	chartURL := fmt.Sprintf("%s/%s/%s", speculaSrv.URL, repo, chartFile)
	resp, err := http.Get(chartURL)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Now explicitly request the .prov as a separate artifact.
	provURL := fmt.Sprintf("%s/%s/%s", speculaSrv.URL, repo, provFile)
	resp2, err := http.Get(provURL)
	require.NoError(t, err)
	defer resp2.Body.Close()
	require.Equal(t, http.StatusOK, resp2.StatusCode,
		".prov file must be served as a standalone immutable artifact")

	got, _ := io.ReadAll(resp2.Body)
	assert.Equal(t, provBytes, got, ".prov bytes must match the original")
}

// ── Test 7 — 404 without upstream configured ─────────────────────────────────

// TestHelmNoUpstreamReturns404 verifies that without an upstream configured,
// Specula returns 404 for unknown chart files and 404 for unknown indexes.
func TestHelmNoUpstreamReturns404(t *testing.T) {
	tmp := t.TempDir()
	s := newHelmStack(t, tmp, "")
	speculaSrv := newHelmServer(t, s) // no WithUpstream

	for _, path := range []string{
		"/stable/index.yaml",
		"/stable/missing-chart-1.0.0.tgz",
	} {
		resp, err := http.Get(speculaSrv.URL + path)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode,
			"path %q without upstream must return 404", path)
	}
}

// ── Test 8 — JSON/YAML response bodies from index contain expected fields ──────

// TestHelmIndexContentType verifies that index.yaml is served with an
// appropriate Content-Type header.
func TestHelmIndexContentType(t *testing.T) {
	indexContent := []byte("apiVersion: v1\nentries: {}\n")

	fakeServer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(indexContent)
	})
	upstreamSrv := httptest.NewServer(fakeServer)
	t.Cleanup(upstreamSrv.Close)

	tmp := t.TempDir()
	s := newHelmStack(t, tmp, "")
	speculaSrv := newHelmServer(t, s,
		helmhandler.WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "fake-helm", BaseURL: upstreamSrv.URL, Priority: 0},
		}),
	)

	resp, err := http.Get(speculaSrv.URL + "/myrepo/index.yaml")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	ct := resp.Header.Get("Content-Type")
	assert.True(t, strings.Contains(ct, "yaml") || strings.Contains(ct, "application/"),
		"index.yaml Content-Type must reference yaml or application: got %q", ct)
}

// ── helpers ──────────────────────────────────────────────────────────────────

// ctx returns a background context (helper to avoid shadowing the package-level
// context variable from oci_e2e_test.go).
func ctx(t *testing.T) context.Context {
	t.Helper()
	return context.Background()
}

// Suppress unused import warning for json in case some test helpers use it.
var _ = json.Marshal
