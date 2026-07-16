//go:build integration

// Package e2e contains hermetic end-to-end tests for the Specula OCI
// data-plane handler. Every hermetic test runs entirely in-process using an
// httptest.Server as the fake upstream; no external network access is required.
//
// # What is tested
//
//   - Version probe (/v2/) — GET and HEAD.
//   - Cache-hit serve: manifest + config blob served from pre-seeded CAS.
//   - Cold-cache miss: manifest and blob fetched from a gcr in-memory registry,
//     promoted through the verify-on-write pipeline, then served from CAS.
//   - Second pull is a cache hit: upstream receives zero manifest/blob GETs.
//   - verify-on-write rejection: digest mismatch returns *cache.VerifyError
//     and leaves the CAS clean.
//   - Multi-arch image index: index manifest fetched, child manifest fetched
//     by digest, config blob fetched — three independent miss→CAS hops.
//   - Fake upstream fixture: standalone push/pull round-trip through the gcr
//     in-memory registry (validates the upstream fixture itself).
//   - Seed-from-gcr: image data extracted from the in-memory registry and
//     manually seeded into Specula's CAS, then pulled through Specula.
//
// # Live test
//
// TestLive is always skipped unless SPECULA_E2E_LIVE=1. It pulls a small
// public image through docker.m.daocloud.io (CN mirror). Bearer-token auth
// (G7) and OCI Accept negotiation (G4) are both implemented so the live pull
// should succeed once network access is available.
package e2e

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
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
	ocihandler "github.com/ivanzzeth/specula/internal/handler/oci"
	"github.com/ivanzzeth/specula/internal/store/local"
	"github.com/ivanzzeth/specula/internal/store/sqlite"
	"github.com/ivanzzeth/specula/internal/upstream"
	"github.com/ivanzzeth/specula/internal/verify"
)

// ────────────────────────────────────────────────────────────────────────────
// inMemTofuStore — in-memory TofuStore for tests
// ────────────────────────────────────────────────────────────────────────────

// inMemTofuStore implements verify.TofuStore using a mutex-protected map.
type inMemTofuStore struct {
	mu   sync.Mutex
	pins map[string]string
}

func newInMemTofuStore() *inMemTofuStore {
	return &inMemTofuStore{pins: make(map[string]string)}
}

func (s *inMemTofuStore) GetPin(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pins[key], nil
}

func (s *inMemTofuStore) SetPin(_ context.Context, key, digest string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pins[key] = digest
	return nil
}

// compile-time assertion
var _ verify.TofuStore = (*inMemTofuStore)(nil)

// ────────────────────────────────────────────────────────────────────────────
// speculaStack — the full real data-plane stack
// ────────────────────────────────────────────────────────────────────────────

type speculaStack struct {
	dir       string // root temp dir for all state
	blobStore *local.LocalDiskDriver
	metaStore *sqlite.SQLiteStore
	cm        cache.CacheManager
	tofuStore *inMemTofuStore
}

// newSpeculaStack wires LocalDiskDriver + SQLiteStore + verify.Chain(checksum, tofu)
// + cache.New under a test-isolated temp directory.
func newSpeculaStack(t *testing.T, dir string) *speculaStack {
	t.Helper()

	blobDir := filepath.Join(dir, "blobs")
	require.NoError(t, os.MkdirAll(blobDir, 0o755))

	ms, err := sqlite.NewSQLiteStore(filepath.Join(dir, "specula.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = ms.Close() })

	ts := newInMemTofuStore()
	bs := local.NewLocalDiskDriver(blobDir)
	chain := verify.NewChain(verify.NewChecksumVerifier(), verify.NewTofuVerifier(ts))

	return &speculaStack{
		dir:       dir,
		blobStore: bs,
		metaStore: ms,
		cm:        cache.New(bs, ms, chain),
		tofuStore: ts,
	}
}

// seedBlob quarantines data and promotes it to the real CAS keyed by
// (protocol="oci", name=imageName, version=digest). The blob handler (post G3
// fix) looks up entries using version=digest, so this is the correct key.
func (s *speculaStack) seedBlob(t *testing.T, imageName string, data []byte, umeta artifact.UpstreamMeta) {
	t.Helper()
	ctx := context.Background()

	art, cleanup, err := cache.Quarantine(ctx, s.dir, bytes.NewReader(data), umeta)
	require.NoError(t, err)

	// The computed digest is the lookup key (G3 fix: version=digest).
	ref := artifact.ArtifactRef{
		Protocol: "oci",
		Name:     imageName,
		Version:  art.Digest,
		Digest:   art.Digest,
	}

	_, storeErr := s.cm.Store(ctx, ref, art)
	if storeErr != nil {
		cleanup()
	}
	require.NoError(t, storeErr, "seedBlob: Store failed for %s", imageName)
}

// seedManifest quarantines manifest bytes and stores them keyed by digestStr
// (which must equal sha256(data)). The manifest handler looks up entries with
// version=manifestDigest.
func (s *speculaStack) seedManifest(t *testing.T, imageName string, data []byte, digestStr string, umeta artifact.UpstreamMeta) {
	t.Helper()
	ctx := context.Background()

	art, cleanup, err := cache.Quarantine(ctx, s.dir, bytes.NewReader(data), umeta)
	require.NoError(t, err)

	require.Equal(t, digestStr, art.Digest,
		"seedManifest: digestStr must equal sha256 of manifest bytes (got %s, want %s)",
		art.Digest, digestStr)

	ref := artifact.ArtifactRef{
		Protocol: "oci",
		Name:     imageName,
		Version:  digestStr,
		Digest:   digestStr,
	}

	_, storeErr := s.cm.Store(ctx, ref, art)
	if storeErr != nil {
		cleanup()
	}
	require.NoError(t, storeErr, "seedManifest: Store failed for %s@%s", imageName, digestStr)
}

// seedTag writes a mutable tag→digest pointer with TTL=-1 (never revalidate).
func (s *speculaStack) seedTag(t *testing.T, imageName, tag, digestStr string) {
	t.Helper()
	err := s.metaStore.PutMutable(context.Background(), artifact.MutableEntry{
		Key:        "oci:" + imageName + ":" + tag,
		Protocol:   "oci",
		Digest:     digestStr,
		TTLSeconds: -1,
		FetchedAt:  time.Now().UTC(),
	})
	require.NoError(t, err, "seedTag: PutMutable failed")
}

// ────────────────────────────────────────────────────────────────────────────
// countingMux — counts /manifests/ and /blobs/ upstream hits
// ────────────────────────────────────────────────────────────────────────────

type countingMux struct {
	inner     http.Handler
	manifests int64 // atomic
	blobs     int64 // atomic
}

func (c *countingMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Only count read requests (GET/HEAD), not push operations (PUT/POST/PATCH).
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		p := r.URL.Path
		switch {
		case strings.Contains(p, "/manifests/"):
			atomic.AddInt64(&c.manifests, 1)
		case strings.Contains(p, "/blobs/"):
			atomic.AddInt64(&c.blobs, 1)
		}
	}
	c.inner.ServeHTTP(w, r)
}

// ────────────────────────────────────────────────────────────────────────────
// Shared helpers
// ────────────────────────────────────────────────────────────────────────────

func sha256hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// buildTestImage returns raw manifest bytes, raw config bytes, and the manifest
// digest for a tiny single-layer random OCI image.
func buildTestImage(t *testing.T) (manifestBytes, configBytes []byte, digest v1.Hash) {
	t.Helper()
	img, err := random.Image(64, 1)
	require.NoError(t, err)

	manifestBytes, err = img.RawManifest()
	require.NoError(t, err)
	configBytes, err = img.RawConfigFile()
	require.NoError(t, err)
	digest, err = img.Digest()
	require.NoError(t, err)
	return
}

// newSpeculaServer wires an oci.Handler over the given stack, starts an
// httptest.Server, and returns it together with the bare host:port string.
func newSpeculaServer(t *testing.T, s *speculaStack, opts ...ocihandler.Option) (*httptest.Server, string) {
	t.Helper()
	allOpts := append([]ocihandler.Option{ocihandler.WithMeta(s.metaStore)}, opts...)
	h := ocihandler.NewHandler(s.cm, allOpts...)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, strings.TrimPrefix(srv.URL, "http://")
}

// pullImage performs remote.Image() against Specula and returns the v1.Image.
func pullImage(t *testing.T, regHost, imageName, tag string) v1.Image {
	t.Helper()
	ref, err := name.ParseReference(
		fmt.Sprintf("%s/%s:%s", regHost, imageName, tag),
		name.Insecure,
	)
	require.NoError(t, err)
	img, err := remote.Image(ref, remote.WithTransport(http.DefaultTransport))
	require.NoError(t, err)
	return img
}

// ────────────────────────────────────────────────────────────────────────────
// fakeRegistryWithImage — builds and pushes a random image to a gcr in-memory
// registry. Returns the registry httptest.Server and the pushed image.
// ────────────────────────────────────────────────────────────────────────────

type fakeRegistry struct {
	srv  *httptest.Server
	host string // bare host:port
	img  v1.Image
	dig  v1.Hash
}

// newFakeRegistry creates an in-memory gcr registry, pushes a random image to
// it at repo:tag, and returns the populated fakeRegistry.
func newFakeRegistry(t *testing.T, repo, tag string) *fakeRegistry {
	t.Helper()
	srv := httptest.NewServer(ggcrregistry.New())
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")

	img, err := random.Image(64, 1)
	require.NoError(t, err)

	ref, err := name.ParseReference(
		fmt.Sprintf("%s/%s:%s", host, repo, tag),
		name.Insecure,
	)
	require.NoError(t, err)
	require.NoError(t, remote.Write(ref, img, remote.WithTransport(http.DefaultTransport)))

	dig, err := img.Digest()
	require.NoError(t, err)

	return &fakeRegistry{srv: srv, host: host, img: img, dig: dig}
}

// asUpstream returns an upstream.Upstream pointing at the fake registry.
func (f *fakeRegistry) asUpstream(name string) upstream.Upstream {
	return upstream.Upstream{Name: name, BaseURL: f.srv.URL, Priority: 0}
}

// ────────────────────────────────────────────────────────────────────────────
// Test 1 — Version probe
// ────────────────────────────────────────────────────────────────────────────

func TestVersionProbe(t *testing.T) {
	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	_, regHost := newSpeculaServer(t, s)

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		method := method
		t.Run(method, func(t *testing.T) {
			req, _ := http.NewRequest(method, "http://"+regHost+"/v2/", nil)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Equal(t, "registry/2.0",
				resp.Header.Get("Docker-Distribution-Api-Version"))
		})
	}
}

// ────────────────────────────────────────────────────────────────────────────
// Test 2 — Cache-hit: manifest + config served from pre-seeded CAS
// ────────────────────────────────────────────────────────────────────────────

// TestCacheHitImagePull pre-seeds the real CAS via cache.Quarantine + cm.Store
// (the same path that fetchAndStoreManifest / fetchAndStoreBlob follow on a
// cold-cache miss), then drives remote.Image() through Specula and asserts the
// round-trip digest is correct. Also verifies that CAS usage bytes and OCI
// metadata stats are positive after the promote pipeline ran.
func TestCacheHitImagePull(t *testing.T) {
	manifestBytes, configBytes, wantDigest := buildTestImage(t)
	digestStr := wantDigest.String()

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	const imageName = "library/test"
	umeta := artifact.UpstreamMeta{Upstream: "fake"}

	// Seed the config blob keyed by its digest (post G3-fix: version=digest).
	s.seedBlob(t, imageName, configBytes, umeta)
	// Seed the manifest keyed by its digest.
	s.seedManifest(t, imageName, manifestBytes, digestStr, umeta)
	// Seed the mutable tag pointer so resolveManifestDigest resolves without upstream.
	s.seedTag(t, imageName, "latest", digestStr)

	_, regHost := newSpeculaServer(t, s)
	img := pullImage(t, regHost, imageName, "latest")

	// Manifest digest round-trips.
	gotDigest, err := img.Digest()
	require.NoError(t, err)
	assert.Equal(t, wantDigest, gotDigest)

	// ConfigFile() triggers a blob GET to Specula — verifies blob serve path.
	_, err = img.ConfigFile()
	require.NoError(t, err, "config file must be parseable (blob GET to Specula)")

	// CAS must have content (verify-on-write pipeline ran during seeding).
	used, err := s.blobStore.UsageBytes(context.Background())
	require.NoError(t, err)
	assert.Positive(t, used, "CAS usage bytes must be positive")

	// Per-protocol stats must show oci bytes.
	stats, err := s.metaStore.CacheSizeByProtocol(context.Background())
	require.NoError(t, err)
	ociStat, ok := stats["oci"]
	require.True(t, ok, "oci stats must exist")
	assert.Positive(t, ociStat.Objects)
	assert.Positive(t, ociStat.Bytes)
}

// ────────────────────────────────────────────────────────────────────────────
// Test 3 — Cold-cache miss → upstream fetch → CAS → serve
// ────────────────────────────────────────────────────────────────────────────

// TestColdCacheMissFetch drives the full cache-miss pipeline:
//  1. A random image is pushed to a gcr in-memory registry (the fake upstream).
//  2. Specula starts with an empty CAS.
//  3. remote.Image() is called; the manifest miss triggers fetchAndStoreManifest
//     which streams bytes through Quarantine → ChecksumVerifier → TofuVerifier
//     → CAS → mutable tag pointer.
//  4. The config blob miss triggers fetchAndStoreBlob with the same pipeline.
//  5. The returned digest matches the original.
//  6. CAS has bytes; stats are positive.
func TestColdCacheMissFetch(t *testing.T) {
	const imageName = "library/cold"
	const tag = "latest"

	fakeReg := newFakeRegistry(t, imageName, tag)

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	_, regHost := newSpeculaServer(t, s,
		ocihandler.WithUpstream(upstream.NewClient(), []upstream.Upstream{fakeReg.asUpstream("gcr-fake")}),
		ocihandler.WithQuarantineDir(tmp),
	)

	img := pullImage(t, regHost, imageName, tag)

	gotDigest, err := img.Digest()
	require.NoError(t, err)
	assert.Equal(t, fakeReg.dig, gotDigest, "cold-cache miss: digest must match upstream image")

	_, err = img.ConfigFile()
	require.NoError(t, err, "cold-cache miss: config must be parseable after CAS promotion")

	// CAS populated from upstream.
	used, err := s.blobStore.UsageBytes(context.Background())
	require.NoError(t, err)
	assert.Positive(t, used, "CAS must have bytes after cold-cache miss pull")

	// Metadata stats.
	stats, err := s.metaStore.CacheSizeByProtocol(context.Background())
	require.NoError(t, err)
	ociStat, ok := stats["oci"]
	require.True(t, ok, "oci stats must exist after cold-cache pull")
	assert.Positive(t, ociStat.Objects)
	assert.Positive(t, ociStat.Bytes)
}

// ────────────────────────────────────────────────────────────────────────────
// Test 4 — Second pull is a cache hit; upstream counter stays zero
// ────────────────────────────────────────────────────────────────────────────

// TestSecondPullCacheHit verifies that after the first pull populates the CAS
// (cold-cache miss path), a second pull is served entirely from the CAS without
// touching the upstream. A countingMux wraps the gcr in-memory registry as the
// SOLE upstream for Specula; counters are sampled before and after each pull.
func TestSecondPullCacheHit(t *testing.T) {
	const imageName = "library/twopulls"
	const tag = "latest"

	// Build a counting wrapper around a fresh in-memory registry.
	counter := &countingMux{inner: ggcrregistry.New()}
	upstreamSrv := httptest.NewServer(counter)
	t.Cleanup(upstreamSrv.Close)
	upstreamHost := strings.TrimPrefix(upstreamSrv.URL, "http://")

	// Push a random image to the counting registry (push does not touch
	// manifest/blob GET counters because countingMux only counts GET/HEAD).
	img, err := random.Image(64, 1)
	require.NoError(t, err)
	wantDigest, err := img.Digest()
	require.NoError(t, err)

	pushRef, err := name.ParseReference(
		fmt.Sprintf("%s/%s:%s", upstreamHost, imageName, tag),
		name.Insecure,
	)
	require.NoError(t, err)
	require.NoError(t, remote.Write(pushRef, img, remote.WithTransport(http.DefaultTransport)))

	// Record baseline after push. remote.Write sends HEAD requests to check existence;
	// countingMux counts HEAD as well as GET since both are upstream reads. We track
	// relative changes rather than asserting absolute zero.
	baseManifests := atomic.LoadInt64(&counter.manifests)
	_ = atomic.LoadInt64(&counter.blobs) // baseline noted; blob counter checked relatively below

	// Wire Specula to the counting registry as the sole upstream.
	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	ups := []upstream.Upstream{{Name: "counter-gcr", BaseURL: upstreamSrv.URL, Priority: 0}}
	_, regHost := newSpeculaServer(t, s,
		ocihandler.WithUpstream(upstream.NewClient(), ups),
		ocihandler.WithQuarantineDir(tmp),
	)

	// First pull — cold cache; manifest and config blob must be fetched from upstream.
	img1 := pullImage(t, regHost, imageName, tag)
	d1, err := img1.Digest()
	require.NoError(t, err)
	assert.Equal(t, wantDigest, d1, "first pull: digest must match upstream")
	_, err = img1.ConfigFile()
	require.NoError(t, err, "first pull: ConfigFile must succeed")

	// Record upstream hit counts after the first pull.
	manifestsAfterFirst := atomic.LoadInt64(&counter.manifests)
	blobsAfterFirst := atomic.LoadInt64(&counter.blobs)
	assert.Greater(t, manifestsAfterFirst, baseManifests, "first pull must have fetched manifest from upstream")

	// Second pull — the manifest is resolved from the mutable-tier entry written
	// by fetchAndStoreManifest, and both manifest and config blob come from CAS.
	// The upstream counter must not increase.
	img2 := pullImage(t, regHost, imageName, tag)
	d2, err := img2.Digest()
	require.NoError(t, err)
	assert.Equal(t, wantDigest, d2, "second pull: digest must match (served from CAS)")
	_, err = img2.ConfigFile()
	require.NoError(t, err, "second pull: ConfigFile must succeed")

	assert.Equal(t, manifestsAfterFirst, atomic.LoadInt64(&counter.manifests),
		"second pull must NOT hit manifest upstream (cache hit)")
	assert.Equal(t, blobsAfterFirst, atomic.LoadInt64(&counter.blobs),
		"second pull must NOT hit blob upstream (cache hit)")
	assert.Equal(t, d1, d2, "both pulls must return the same digest")
}

// TestCacheHitNoUpstreamHit verifies that when the CAS is pre-seeded, two
// consecutive pulls never contact the upstream (counter stays at zero).
func TestCacheHitNoUpstreamHit(t *testing.T) {
	manifestBytes, configBytes, wantDigest := buildTestImage(t)
	digestStr := wantDigest.String()

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	const imageName = "library/cached"
	umeta := artifact.UpstreamMeta{Upstream: "gcr-inmem"}

	// Pre-seed CAS so no upstream fetch is needed.
	s.seedBlob(t, imageName, configBytes, umeta)
	s.seedManifest(t, imageName, manifestBytes, digestStr, umeta)
	s.seedTag(t, imageName, "latest", digestStr)

	// Fake upstream with request counter. Cache is warm, so this should never
	// be called.
	counter := &countingMux{inner: ggcrregistry.New()}
	upstreamSrv := httptest.NewServer(counter)
	t.Cleanup(upstreamSrv.Close)

	ups := []upstream.Upstream{{Name: "gcr-inmem", BaseURL: upstreamSrv.URL, Priority: 0}}
	_, regHost := newSpeculaServer(t, s,
		ocihandler.WithUpstream(upstream.NewClient(), ups),
	)

	// Two pulls — both must be cache hits.
	for _, trial := range []string{"first", "second"} {
		img := pullImage(t, regHost, imageName, "latest")
		d, err := img.Digest()
		require.NoError(t, err)
		assert.Equal(t, wantDigest, d, "%s pull: digest must match", trial)
		_, err = img.ConfigFile()
		require.NoError(t, err, "%s pull: config file must be parseable", trial)
	}

	assert.Equal(t, int64(0), atomic.LoadInt64(&counter.manifests),
		"manifest upstream hits must be 0 (cache warm)")
	assert.Equal(t, int64(0), atomic.LoadInt64(&counter.blobs),
		"blob upstream hits must be 0 (cache warm)")
}

// ────────────────────────────────────────────────────────────────────────────
// Test 5 — verify-on-write: digest mismatch → VerifyError, CAS clean
// ────────────────────────────────────────────────────────────────────────────

// TestVerifyOnWriteRejectsDigestMismatch verifies that cm.Store returns a
// *cache.VerifyError and does not promote the blob when the checksum computed
// during Quarantine does not match the reference digest in ArtifactRef.Digest.
func TestVerifyOnWriteRejectsDigestMismatch(t *testing.T) {
	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	ctx := context.Background()

	goodBytes := []byte("good content for quarantine")
	art, cleanup, err := cache.Quarantine(ctx, tmp, bytes.NewReader(goodBytes), artifact.UpstreamMeta{})
	require.NoError(t, err)
	defer cleanup()

	// Reference digest deliberately differs from art.Digest.
	badDigest := "sha256:" + sha256hex([]byte("completely different content"))
	require.NotEqual(t, art.Digest, badDigest)

	ref := artifact.ArtifactRef{
		Protocol: "oci",
		Name:     "library/tampered",
		Version:  badDigest,
		Digest:   badDigest,
	}

	_, storeErr := s.cm.Store(ctx, ref, art)
	require.Error(t, storeErr, "Store must fail on digest mismatch")

	ve, isVerify := cache.AsVerifyError(storeErr)
	require.True(t, isVerify, "error must be *cache.VerifyError, got %T: %v", storeErr, storeErr)
	assert.Equal(t, artifact.StatusFail, ve.Result.Status)

	// Quarantine file removed on failure.
	_, statErr := os.Stat(art.Path)
	assert.True(t, os.IsNotExist(statErr), "quarantine file must be removed after verify failure")

	// Blob must NOT be in CAS under either the declared or actual digest.
	existsBad, err := s.blobStore.Exists(ctx, badDigest)
	require.NoError(t, err)
	assert.False(t, existsBad, "tampered blob must not be in CAS under declared digest")

	existsActual, err := s.blobStore.Exists(ctx, art.Digest)
	require.NoError(t, err)
	assert.False(t, existsActual, "tampered blob must not be in CAS under actual digest")
}

// TestVerifyOnWriteAcceptsCorrectDigest is the success counterpart: correct
// digest → CAS populated, metadata stats non-zero.
func TestVerifyOnWriteAcceptsCorrectDigest(t *testing.T) {
	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	ctx := context.Background()

	goodBytes := []byte("valid content for CAS promotion")
	art, cleanup, err := cache.Quarantine(ctx, tmp, bytes.NewReader(goodBytes), artifact.UpstreamMeta{Upstream: "fake"})
	require.NoError(t, err)
	defer cleanup()

	ref := artifact.ArtifactRef{
		Protocol: "oci",
		Name:     "library/valid",
		Version:  art.Digest,
		Digest:   art.Digest,
	}

	entry, err := s.cm.Store(ctx, ref, art)
	require.NoError(t, err, "Store must succeed when digests match")
	require.NotNil(t, entry)
	assert.Equal(t, art.Digest, entry.Digest)
	assert.Equal(t, art.Size, entry.Size)

	// Quarantine file removed after promotion.
	_, statErr := os.Stat(art.Path)
	assert.True(t, os.IsNotExist(statErr), "quarantine file must be removed after promotion")

	// Blob in CAS with correct bytes.
	exists, err := s.blobStore.Exists(ctx, art.Digest)
	require.NoError(t, err)
	assert.True(t, exists)

	rc, size, err := s.blobStore.Get(ctx, art.Digest, 0, -1)
	require.NoError(t, err)
	defer rc.Close()
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, goodBytes, got)
	assert.Equal(t, int64(len(goodBytes)), size)

	// Metadata stats.
	stats, err := s.metaStore.CacheSizeByProtocol(ctx)
	require.NoError(t, err)
	ociStat, ok := stats["oci"]
	require.True(t, ok)
	assert.Equal(t, int64(1), ociStat.Objects)
	assert.Equal(t, int64(len(goodBytes)), ociStat.Bytes)
}

// ────────────────────────────────────────────────────────────────────────────
// Test 6 — Cold cache 404 without upstream configured
// ────────────────────────────────────────────────────────────────────────────

// TestColdCacheReturns404WithoutUpstream verifies that without an upstream
// configured, Specula returns the correct OCI 404 error codes.
func TestColdCacheReturns404WithoutUpstream(t *testing.T) {
	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	_, regHost := newSpeculaServer(t, s) // no WithUpstream

	t.Run("manifest unknown", func(t *testing.T) {
		resp, err := http.Get(fmt.Sprintf("http://%s/v2/library/missing/manifests/latest", regHost))
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("blob unknown", func(t *testing.T) {
		fakeDigest := "sha256:" + strings.Repeat("0", 64)
		resp, err := http.Get(fmt.Sprintf("http://%s/v2/library/missing/blobs/%s", regHost, fakeDigest))
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}

// ────────────────────────────────────────────────────────────────────────────
// Test 7 — Multi-arch image index: three separate miss→CAS hops
// ────────────────────────────────────────────────────────────────────────────

// TestMultiArchIndexPull exercises the full index resolution path:
//
//  1. A 2-arch index (amd64 + arm64) is pushed to the gcr in-memory registry.
//  2. Specula starts cold.
//  3. remote.Index() fetches the index: index-manifest miss → upstream fetch →
//     Quarantine → verify → CAS. Mutable tag pointer written.
//  4. The index manifest is parsed client-side; the amd64 descriptor is
//     selected.
//  5. remote.Image(amd64-digest-ref) fetches the amd64 manifest:
//     arch-manifest miss → upstream fetch → verify → CAS.
//  6. ConfigFile() fetches the amd64 config blob:
//     blob miss → upstream fetch → verify → CAS.
//  7. All three digests are verified to match the original.
func TestMultiArchIndexPull(t *testing.T) {
	const imageName = "library/multiarch"
	const tag = "latest"

	// Build a 2-arch index in memory.
	imgAMD64, err := random.Image(64, 1)
	require.NoError(t, err)
	imgARM64, err := random.Image(64, 1)
	require.NoError(t, err)

	idx := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{
			Add: imgAMD64,
			Descriptor: v1.Descriptor{
				Platform: &v1.Platform{OS: "linux", Architecture: "amd64"},
			},
		},
		mutate.IndexAddendum{
			Add: imgARM64,
			Descriptor: v1.Descriptor{
				Platform: &v1.Platform{OS: "linux", Architecture: "arm64"},
			},
		},
	)

	// Push to gcr in-memory registry.
	upstreamSrv := httptest.NewServer(ggcrregistry.New())
	t.Cleanup(upstreamSrv.Close)
	upstreamHost := strings.TrimPrefix(upstreamSrv.URL, "http://")

	pushRef, err := name.ParseReference(
		fmt.Sprintf("%s/%s:%s", upstreamHost, imageName, tag),
		name.Insecure,
	)
	require.NoError(t, err)
	require.NoError(t, remote.WriteIndex(pushRef, idx, remote.WithTransport(http.DefaultTransport)))

	// Set up Specula with gcr as upstream.
	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	ups := []upstream.Upstream{{Name: "gcr-multiarch", BaseURL: upstreamSrv.URL, Priority: 0}}
	_, regHost := newSpeculaServer(t, s,
		ocihandler.WithUpstream(upstream.NewClient(), ups),
		ocihandler.WithQuarantineDir(tmp),
	)

	// Pull the image index through Specula (cold cache).
	idxRef, err := name.ParseReference(
		fmt.Sprintf("%s/%s:%s", regHost, imageName, tag),
		name.Insecure,
	)
	require.NoError(t, err)
	pulledIdx, err := remote.Index(idxRef, remote.WithTransport(http.DefaultTransport))
	require.NoError(t, err, "remote.Index must succeed on cold cache")

	// Verify the index digest.
	wantIdxDigest, err := idx.Digest()
	require.NoError(t, err)
	gotIdxDigest, err := pulledIdx.Digest()
	require.NoError(t, err)
	assert.Equal(t, wantIdxDigest, gotIdxDigest, "index digest must match")

	// Navigate to the amd64 arch manifest.
	idxManifest, err := pulledIdx.IndexManifest()
	require.NoError(t, err)
	require.NotEmpty(t, idxManifest.Manifests, "index must have child manifests")

	// Find the amd64 descriptor.
	var amd64Desc *v1.Descriptor
	for i := range idxManifest.Manifests {
		d := &idxManifest.Manifests[i]
		if d.Platform != nil && d.Platform.Architecture == "amd64" {
			amd64Desc = d
			break
		}
	}
	require.NotNil(t, amd64Desc, "index must contain an amd64 entry")

	// Fetch the amd64 arch-specific manifest through Specula (second miss hop).
	amd64Ref, err := name.NewDigest(
		fmt.Sprintf("%s/%s@%s", regHost, imageName, amd64Desc.Digest.String()),
		name.Insecure,
	)
	require.NoError(t, err)
	amd64Img, err := remote.Image(amd64Ref, remote.WithTransport(http.DefaultTransport))
	require.NoError(t, err, "amd64 manifest fetch through Specula must succeed")

	gotAMD64Digest, err := amd64Img.Digest()
	require.NoError(t, err)
	assert.Equal(t, amd64Desc.Digest, gotAMD64Digest, "amd64 manifest digest must match")

	// ConfigFile() triggers the config blob fetch (third miss hop).
	_, err = amd64Img.ConfigFile()
	require.NoError(t, err, "amd64 config blob fetch through Specula must succeed")

	// Verify CAS was populated.
	used, err := s.blobStore.UsageBytes(context.Background())
	require.NoError(t, err)
	assert.Positive(t, used, "CAS must have content after multi-arch pull")
}

// ────────────────────────────────────────────────────────────────────────────
// Test 8 — Fake upstream fixture validation
// ────────────────────────────────────────────────────────────────────────────

// TestFakeUpstreamGCRRegistry validates the gcr in-memory registry used as the
// fake upstream: it pushes and pulls a random image and verifies digest
// preservation. This test does NOT go through Specula.
func TestFakeUpstreamGCRRegistry(t *testing.T) {
	fr := newFakeRegistry(t, "library/nginx", "latest")

	pullRef, err := name.ParseReference(
		fmt.Sprintf("%s/library/nginx:latest", fr.host),
		name.Insecure,
	)
	require.NoError(t, err)
	pulled, err := remote.Image(pullRef, remote.WithTransport(http.DefaultTransport))
	require.NoError(t, err)

	got, err := pulled.Digest()
	require.NoError(t, err)
	assert.Equal(t, fr.dig, got, "gcr in-memory registry must preserve digest across push/pull")
}

// ────────────────────────────────────────────────────────────────────────────
// Test 9 — Seed from gcr in-memory registry (reference seeding pattern)
// ────────────────────────────────────────────────────────────────────────────

// TestSeedFromGCRInMemoryRegistry demonstrates the pattern used by the
// cold-cache miss tests: image data is extracted from the gcr in-memory
// registry via RawManifest/RawConfigFile and seeded into Specula's CAS using
// seedManifest + seedBlob, then pulled from Specula.
func TestSeedFromGCRInMemoryRegistry(t *testing.T) {
	const imageName = "library/alpine"
	const tag = "3.18"

	fr := newFakeRegistry(t, imageName, tag)

	manifestBytes, err := fr.img.RawManifest()
	require.NoError(t, err)
	configBytes, err := fr.img.RawConfigFile()
	require.NoError(t, err)

	digestStr := fr.dig.String()
	require.Equal(t, "sha256:"+sha256hex(manifestBytes), digestStr,
		"manifest sha256 must match the image digest")

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)
	umeta := artifact.UpstreamMeta{Upstream: fr.host}

	s.seedBlob(t, imageName, configBytes, umeta)
	s.seedManifest(t, imageName, manifestBytes, digestStr, umeta)
	s.seedTag(t, imageName, tag, digestStr)

	_, regHost := newSpeculaServer(t, s)
	img := pullImage(t, regHost, imageName, tag)

	got, err := img.Digest()
	require.NoError(t, err)
	assert.Equal(t, fr.dig, got, "Specula must serve the same digest as the gcr source")

	_, err = img.ConfigFile()
	require.NoError(t, err, "config must be parseable from Specula after seed")
}

// ────────────────────────────────────────────────────────────────────────────
// Live test — gated behind SPECULA_E2E_LIVE=1
// ────────────────────────────────────────────────────────────────────────────

// TestLive pulls a small public image through docker.m.daocloud.io (CN mirror).
// Skipped unless SPECULA_E2E_LIVE=1. Requires CN network access.
//
// The bearer-token dance (G7 fix: parseBearerChallenge + fetchBearerToken) and
// OCI Accept header (G4 fix: WithOCIManifestAccept) are both implemented, so
// this should succeed with working network access.
func TestLive(t *testing.T) {
	if os.Getenv("SPECULA_E2E_LIVE") != "1" {
		t.Skip("set SPECULA_E2E_LIVE=1 to run live network tests (requires CN network access)")
	}

	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	ups := []upstream.Upstream{{
		Name:     "daocloud",
		BaseURL:  "https://docker.m.daocloud.io",
		Priority: 0,
	}}

	_, regHost := newSpeculaServer(t, s,
		ocihandler.WithUpstream(upstream.NewClient(), ups),
		ocihandler.WithQuarantineDir(tmp),
	)

	img := pullImage(t, regHost, "library/hello-world", "latest")
	d, err := img.Digest()
	require.NoError(t, err)
	t.Logf("pulled library/hello-world:latest through Specula → %s", d)
}
