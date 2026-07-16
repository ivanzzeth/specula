package oci

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
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
	"github.com/ivanzzeth/specula/internal/upstream"
)

// fakeCacheManager is a test double for cache.CacheManager that serves blobs
// from an in-memory map and resolves mutable refs (tags) via a simple key lookup.
//
// Lookup key format:
//   - immutable (digest):  "oci:<name>:<digest>"
//   - mutable   (tag):     "oci:<name>:<tag>"
//
// Serve key: entry.Digest.
type fakeCacheManager struct {
	// entries maps lookup keys → CacheEntry (used by Lookup).
	entries map[string]*artifact.CacheEntry
	// blobs maps digest → raw bytes (used by Serve).
	blobs map[string][]byte
}

var _ cache.CacheManager = (*fakeCacheManager)(nil)

func (f *fakeCacheManager) Lookup(_ context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	key := lookupKey(ref)
	e, ok := f.entries[key]
	if !ok {
		return nil, nil
	}
	return e, nil
}

func (f *fakeCacheManager) Serve(_ context.Context, ref artifact.ArtifactRef, offset, length int64) (io.ReadCloser, *artifact.CacheEntry, error) {
	digest := ref.Digest
	if digest == "" {
		digest = ref.Version
	}
	data, ok := f.blobs[digest]
	if !ok {
		return nil, nil, fmt.Errorf("fake: blob %s not found", digest)
	}

	// Apply offset / length window.
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

	slice := data[start:end]
	entry := &artifact.CacheEntry{
		Digest:   digest,
		Size:     total,
		Protocol: ref.Protocol,
	}
	return io.NopCloser(bytes.NewReader(slice)), entry, nil
}

func (f *fakeCacheManager) Store(_ context.Context, _ artifact.ArtifactRef, _ *artifact.Artifact) (*artifact.CacheEntry, error) {
	return nil, fmt.Errorf("fake: Store not implemented")
}

// lookupKey produces the map key used by fakeCacheManager.Lookup.
func lookupKey(ref artifact.ArtifactRef) string {
	id := ref.Version
	if ref.Digest != "" {
		id = ref.Digest
	}
	return ref.Protocol + ":" + ref.Name + ":" + id
}

// sha256Digest computes the OCI digest string for data.
func sha256Digest(data []byte) string {
	h := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(h[:])
}

// newTestServer builds a Handler backed by a fake cache and an httptest.Server.
// Callers are responsible for closing the returned server.
func newTestServer(fake *fakeCacheManager) (*Handler, *httptest.Server) {
	h := NewHandler(fake)
	srv := httptest.NewServer(h)
	return h, srv
}

// ---- tests ------------------------------------------------------------------

func TestVersionProbe(t *testing.T) {
	_, srv := newTestServer(&fakeCacheManager{entries: map[string]*artifact.CacheEntry{}, blobs: map[string][]byte{}})
	defer srv.Close()

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			req, _ := http.NewRequest(method, srv.URL+"/v2/", nil)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusOK, resp.StatusCode)
			assert.Equal(t, "registry/2.0", resp.Header.Get("Docker-Distribution-Api-Version"))
		})
	}
}

func TestServeManifestByTag(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","config":{"mediaType":"application/vnd.docker.container.image.v1+json","size":100,"digest":"sha256:abc"},"layers":[]}`)
	mDigest := sha256Digest(manifest)

	fake := &fakeCacheManager{
		entries: map[string]*artifact.CacheEntry{
			// mutable: tag "latest" → digest
			"oci:nginx:latest": {Digest: mDigest, Size: int64(len(manifest)), Protocol: "oci"},
			// immutable: digest lookup (for Serve path)
			"oci:nginx:" + mDigest: {Digest: mDigest, Size: int64(len(manifest)), Protocol: "oci"},
		},
		blobs: map[string][]byte{
			mDigest: manifest,
		},
	}
	_, srv := newTestServer(fake)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/nginx/manifests/latest")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, mDigest, resp.Header.Get("Docker-Content-Digest"))
	assert.Contains(t, resp.Header.Get("Content-Type"), "manifest")

	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, manifest, body)
}

func TestServeManifestByDigest(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`)
	mDigest := sha256Digest(manifest)

	fake := &fakeCacheManager{
		entries: map[string]*artifact.CacheEntry{
			"oci:myrepo/myimage:" + mDigest: {Digest: mDigest, Size: int64(len(manifest)), Protocol: "oci"},
		},
		blobs: map[string][]byte{mDigest: manifest},
	}
	_, srv := newTestServer(fake)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/myrepo/myimage/manifests/" + mDigest)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, mDigest, resp.Header.Get("Docker-Content-Digest"))
	assert.Equal(t, "application/vnd.oci.image.manifest.v1+json", resp.Header.Get("Content-Type"))

	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, manifest, body)
}

func TestServeManifestHEAD(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2}`)
	mDigest := sha256Digest(manifest)

	fake := &fakeCacheManager{
		entries: map[string]*artifact.CacheEntry{
			"oci:img:v1": {Digest: mDigest, Size: int64(len(manifest)), Protocol: "oci"},
		},
		blobs: map[string][]byte{mDigest: manifest},
	}
	_, srv := newTestServer(fake)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodHead, srv.URL+"/v2/img/manifests/v1", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, mDigest, resp.Header.Get("Docker-Content-Digest"))
	body, _ := io.ReadAll(resp.Body)
	assert.Empty(t, body)
}

func TestServeBlobGet(t *testing.T) {
	blobData := bytes.Repeat([]byte("Z"), 2048)
	bDigest := sha256Digest(blobData)

	fake := &fakeCacheManager{
		entries: map[string]*artifact.CacheEntry{
			"oci:img:" + bDigest: {Digest: bDigest, Size: int64(len(blobData)), Protocol: "oci"},
		},
		blobs: map[string][]byte{bDigest: blobData},
	}
	_, srv := newTestServer(fake)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/img/blobs/" + bDigest)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, bDigest, resp.Header.Get("Docker-Content-Digest"))

	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, blobData, got)
}

func TestServeBlobRangeGet(t *testing.T) {
	blobData := bytes.Repeat([]byte("A"), 1024)
	bDigest := sha256Digest(blobData)

	fake := &fakeCacheManager{
		entries: map[string]*artifact.CacheEntry{
			"oci:img:" + bDigest: {Digest: bDigest, Size: int64(len(blobData)), Protocol: "oci"},
		},
		blobs: map[string][]byte{bDigest: blobData},
	}
	_, srv := newTestServer(fake)
	defer srv.Close()

	tests := []struct {
		name         string
		rangeHeader  string
		wantStatus   int
		wantLen      int
		wantRangeHdr string
	}{
		{
			name:         "first 512 bytes",
			rangeHeader:  "bytes=0-511",
			wantStatus:   http.StatusPartialContent,
			wantLen:      512,
			wantRangeHdr: fmt.Sprintf("bytes 0-511/%d", len(blobData)),
		},
		{
			name:         "last 100 bytes (suffix range)",
			rangeHeader:  "bytes=-100",
			wantStatus:   http.StatusPartialContent,
			wantLen:      100,
			wantRangeHdr: fmt.Sprintf("bytes %d-%d/%d", len(blobData)-100, len(blobData)-1, len(blobData)),
		},
		{
			name:         "open-ended from 512",
			rangeHeader:  "bytes=512-",
			wantStatus:   http.StatusPartialContent,
			wantLen:      512,
			wantRangeHdr: fmt.Sprintf("bytes 512-1023/%d", len(blobData)),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/img/blobs/"+bDigest, nil)
			req.Header.Set("Range", tc.rangeHeader)

			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()

			assert.Equal(t, tc.wantStatus, resp.StatusCode)
			assert.Equal(t, tc.wantRangeHdr, resp.Header.Get("Content-Range"))

			body, _ := io.ReadAll(resp.Body)
			assert.Len(t, body, tc.wantLen)
		})
	}
}

func TestServeBlobHEAD(t *testing.T) {
	blobData := []byte("head-check-content")
	bDigest := sha256Digest(blobData)

	fake := &fakeCacheManager{
		entries: map[string]*artifact.CacheEntry{
			"oci:img:" + bDigest: {Digest: bDigest, Size: int64(len(blobData)), Protocol: "oci"},
		},
		blobs: map[string][]byte{bDigest: blobData},
	}
	_, srv := newTestServer(fake)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodHead, srv.URL+"/v2/img/blobs/"+bDigest, nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, fmt.Sprintf("%d", len(blobData)), resp.Header.Get("Content-Length"))
	body, _ := io.ReadAll(resp.Body)
	assert.Empty(t, body)
}

func TestManifest404(t *testing.T) {
	fake := &fakeCacheManager{
		entries: map[string]*artifact.CacheEntry{},
		blobs:   map[string][]byte{},
	}
	_, srv := newTestServer(fake)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/img/manifests/doesnotexist")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "MANIFEST_UNKNOWN")
}

func TestBlob404(t *testing.T) {
	fake := &fakeCacheManager{
		entries: map[string]*artifact.CacheEntry{},
		blobs:   map[string][]byte{},
	}
	_, srv := newTestServer(fake)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/img/blobs/sha256:" + strings.Repeat("0", 64))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "BLOB_UNKNOWN")
}

func TestUnknownRoute404(t *testing.T) {
	fake := &fakeCacheManager{entries: map[string]*artifact.CacheEntry{}, blobs: map[string][]byte{}}
	_, srv := newTestServer(fake)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/img/uploads/")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestMultiSegmentImageName(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json"}`)
	mDigest := sha256Digest(manifest)

	fake := &fakeCacheManager{
		entries: map[string]*artifact.CacheEntry{
			"oci:library/nginx:stable": {Digest: mDigest, Size: int64(len(manifest)), Protocol: "oci"},
		},
		blobs: map[string][]byte{mDigest: manifest},
	}
	_, srv := newTestServer(fake)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/library/nginx/manifests/stable")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, manifest, body)
}

// ---- parseRange unit tests --------------------------------------------------

func TestParseRange(t *testing.T) {
	const size = int64(1000)

	tests := []struct {
		name        string
		header      string
		wantOffset  int64
		wantLength  int64
		wantPartial bool
		wantErr     bool
	}{
		{"no header", "", 0, -1, false, false},
		{"full range", "bytes=0-999", 0, 1000, true, false},
		{"partial", "bytes=0-499", 0, 500, true, false},
		{"open end", "bytes=500-", 500, 500, true, false},
		{"suffix 100", "bytes=-100", 900, 100, true, false},
		{"clamp end", "bytes=0-9999", 0, 1000, true, false},
		{"start > size", "bytes=1001-1999", 0, 0, false, true},
		{"start > end", "bytes=500-100", 0, 0, false, true},
		{"bad unit", "items=0-100", 0, 0, false, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			off, length, partial, err := parseRange(tc.header, size)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantOffset, off, "offset")
			assert.Equal(t, tc.wantLength, length, "length")
			assert.Equal(t, tc.wantPartial, partial, "partial")
		})
	}
}

// ---- isDigestRef unit tests -------------------------------------------------

func TestIsDigestRef(t *testing.T) {
	tests := []struct {
		ref  string
		want bool
	}{
		{"sha256:" + strings.Repeat("a", 64), true},
		{"latest", false},
		{"v1.2.3", false},
		{"sha256:tooshort", false}, // invalid hex length
		{"", false},
	}
	for _, tc := range tests {
		t.Run(tc.ref, func(t *testing.T) {
			assert.Equal(t, tc.want, isDigestRef(tc.ref))
		})
	}
}

// ── storingFakeCache — fake CacheManager that supports Store ─────────────────
//
// Unlike fakeCacheManager (which panics on Store), storingFakeCache reads the
// quarantine file, stores the bytes by digest, and records the immutable and
// optional mutable cache entries. This lets handler tests exercise the full
// cache-miss → upstream-fetch → quarantine → store → serve pipeline without a
// real BlobStore/MetadataStore.

type storingFakeCache struct {
	mu      sync.Mutex
	entries map[string]*artifact.CacheEntry // lookup key → entry
	blobs   map[string][]byte               // digest → bytes
}

var _ cache.CacheManager = (*storingFakeCache)(nil)

func newStoringFakeCache() *storingFakeCache {
	return &storingFakeCache{
		entries: make(map[string]*artifact.CacheEntry),
		blobs:   make(map[string][]byte),
	}
}

func (s *storingFakeCache) Lookup(_ context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := lookupKey(ref)
	e, ok := s.entries[key]
	if !ok {
		return nil, nil
	}
	return e, nil
}

func (s *storingFakeCache) Serve(_ context.Context, ref artifact.ArtifactRef, offset, length int64) (io.ReadCloser, *artifact.CacheEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	digest := ref.Digest
	if digest == "" {
		digest = ref.Version
	}
	data, ok := s.blobs[digest]
	if !ok {
		return nil, nil, fmt.Errorf("fake: blob %s not found", digest)
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
	slice := data[start:end]
	entry := &artifact.CacheEntry{
		Digest:   digest,
		Size:     total,
		Protocol: ref.Protocol,
	}
	return io.NopCloser(bytes.NewReader(slice)), entry, nil
}

// Store reads the quarantine file, saves the bytes, and records cache entries.
// It removes art.Path after reading, matching production CacheManager behaviour.
func (s *storingFakeCache) Store(_ context.Context, ref artifact.ArtifactRef, art *artifact.Artifact) (*artifact.CacheEntry, error) {
	data, err := os.ReadFile(art.Path)
	if err != nil {
		return nil, fmt.Errorf("fake store: read quarantine %s: %w", art.Path, err)
	}
	_ = os.Remove(art.Path) // match production behaviour

	s.mu.Lock()
	defer s.mu.Unlock()

	s.blobs[art.Digest] = data

	entry := &artifact.CacheEntry{
		Ref:      ref,
		Digest:   art.Digest,
		Size:     art.Size,
		Protocol: ref.Protocol,
		Upstream: art.Meta.Upstream,
	}

	// Immutable entry keyed by digest (what Lookup uses for immutable refs).
	immKey := ref.Protocol + ":" + ref.Name + ":" + art.Digest
	s.entries[immKey] = entry

	// Also write mutable pointer so subsequent tag lookups resolve without
	// going to upstream again.
	if ref.Mutable && ref.Version != "" {
		mutKey := ref.Protocol + ":" + ref.Name + ":" + ref.Version
		s.entries[mutKey] = entry
	}

	return entry, nil
}

// ── ociUpstreamServer — minimal fake OCI registry for handler tests ───────────

// ociUpstreamServer creates an httptest.Server that responds to manifest and
// blob requests with pre-seeded content. Optionally wraps requests with bearer
// auth when tokenSecret is non-empty.
//
// Served routes:
//
//	GET /v2/                               → 200 {}
//	GET /v2/<name>/manifests/<ref>         → 200 manifest JSON
//	GET /v2/<name>/blobs/<digest>          → 200 blob bytes
//	GET /token                             → 200 {"token":<tokenSecret>}  (auth only)
func ociUpstreamServer(t *testing.T, manifests map[string][]byte, blobs map[string][]byte, tokenSecret string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Bearer token endpoint.
		if r.URL.Path == "/token" && tokenSecret != "" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"token":%q,"expires_in":3600}`, tokenSecret)
			return
		}

		// Auth check when a token is required.
		if tokenSecret != "" {
			auth := r.Header.Get("Authorization")
			if auth != "Bearer "+tokenSecret {
				w.Header().Set("WWW-Authenticate",
					fmt.Sprintf(`Bearer realm="%s/token",service="fakerepo",scope="repository:library/img:pull"`, srv.URL))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}

		// Version probe.
		if r.URL.Path == "/v2/" || r.URL.Path == "/v2" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
			return
		}

		// Manifest requests: /v2/<name>/manifests/<ref>
		if i := strings.LastIndex(r.URL.Path, "/manifests/"); i >= 0 {
			ref := r.URL.Path[i+len("/manifests/"):]
			data, ok := manifests[ref]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			ct := detectManifestMediaType(data)
			w.Header().Set("Content-Type", ct)
			w.Header().Set("Docker-Content-Digest", sha256Digest(data))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}

		// Blob requests: /v2/<name>/blobs/<digest>
		if i := strings.LastIndex(r.URL.Path, "/blobs/"); i >= 0 {
			digest := r.URL.Path[i+len("/blobs/"):]
			data, ok := blobs[digest]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Docker-Content-Digest", digest)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	return srv
}

// ── Cache-miss pull tests ─────────────────────────────────────────────────────

func TestManifestCacheMiss_FetchFromUpstream(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","config":{"mediaType":"application/vnd.docker.container.image.v1+json","size":10,"digest":"sha256:` + strings.Repeat("a", 64) + `"},"layers":[]}`)
	mDigest := sha256Digest(manifest)

	upSrv := ociUpstreamServer(t,
		map[string][]byte{"latest": manifest, mDigest: manifest},
		nil,
		"", // no auth
	)
	defer upSrv.Close()

	fc := newStoringFakeCache()
	h := NewHandler(fc, WithUpstream(
		upstream.NewClient(),
		[]upstream.Upstream{{Name: "fake", BaseURL: upSrv.URL, Priority: 1}},
	))
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/library/img/manifests/latest")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, mDigest, resp.Header.Get("Docker-Content-Digest"))
	assert.Contains(t, resp.Header.Get("Content-Type"), "manifest")

	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, manifest, body)

	// Second request: served from cache (no upstream hit needed).
	resp2, err := http.Get(srv.URL + "/v2/library/img/manifests/" + mDigest)
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)
	body2, _ := io.ReadAll(resp2.Body)
	assert.Equal(t, manifest, body2)
}

func TestBlobCacheMiss_FetchFromUpstream(t *testing.T) {
	blobData := bytes.Repeat([]byte("X"), 4096)
	bDigest := sha256Digest(blobData)

	manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","layers":[{"digest":%q,"size":%d}]}`, bDigest, len(blobData)))
	mDigest := sha256Digest(manifest)

	upSrv := ociUpstreamServer(t,
		map[string][]byte{"latest": manifest, mDigest: manifest},
		map[string][]byte{bDigest: blobData},
		"",
	)
	defer upSrv.Close()

	fc := newStoringFakeCache()
	h := NewHandler(fc, WithUpstream(
		upstream.NewClient(),
		[]upstream.Upstream{{Name: "fake", BaseURL: upSrv.URL, Priority: 1}},
	))
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Fetch manifest first (populates its CAS entry).
	resp, err := http.Get(srv.URL + "/v2/library/img/manifests/latest")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	// Now fetch the blob (cold miss → upstream → quarantine → verify → CAS).
	resp2, err := http.Get(srv.URL + "/v2/library/img/blobs/" + bDigest)
	require.NoError(t, err)
	defer resp2.Body.Close()

	assert.Equal(t, http.StatusOK, resp2.StatusCode)
	assert.Equal(t, bDigest, resp2.Header.Get("Docker-Content-Digest"))

	got, _ := io.ReadAll(resp2.Body)
	assert.Equal(t, blobData, got)

	// Third request: blob now cached, no upstream hit needed.
	resp3, err := http.Get(srv.URL + "/v2/library/img/blobs/" + bDigest)
	require.NoError(t, err)
	defer resp3.Body.Close()
	assert.Equal(t, http.StatusOK, resp3.StatusCode)
	got3, _ := io.ReadAll(resp3.Body)
	assert.Equal(t, blobData, got3)
}

func TestManifestIndex_MultiArch(t *testing.T) {
	// OCI image index (multi-arch) uses a "manifests" array without "mediaType".
	// detectManifestMediaType must return the OCI index media type.
	archManifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:` + strings.Repeat("b", 64) + `"},"layers":[]}`)
	archDigest := sha256Digest(archManifest)

	index := []byte(fmt.Sprintf(`{"schemaVersion":2,"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":%q,"size":%d,"platform":{"os":"linux","architecture":"amd64"}}]}`,
		archDigest, len(archManifest)))
	indexDigest := sha256Digest(index)

	upSrv := ociUpstreamServer(t,
		map[string][]byte{
			"latest":    index,
			indexDigest: index,
			archDigest:  archManifest,
		},
		nil,
		"",
	)
	defer upSrv.Close()

	fc := newStoringFakeCache()
	h := NewHandler(fc, WithUpstream(
		upstream.NewClient(),
		[]upstream.Upstream{{Name: "fake", BaseURL: upSrv.URL, Priority: 1}},
	))
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Pull the index by tag.
	resp, err := http.Get(srv.URL + "/v2/myrepo/app/manifests/latest")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, indexDigest, resp.Header.Get("Docker-Content-Digest"))
	// Content-Type must identify this as an index/manifest-list, not a single manifest.
	ct := resp.Header.Get("Content-Type")
	assert.True(t,
		ct == "application/vnd.oci.image.index.v1+json" ||
			ct == "application/vnd.docker.distribution.manifest.list.v2+json",
		"expected index media type, got %q", ct)

	// Pull the arch-specific manifest by digest (cold cache miss).
	resp2, err := http.Get(srv.URL + "/v2/myrepo/app/manifests/" + archDigest)
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)
	assert.Equal(t, archDigest, resp2.Header.Get("Docker-Content-Digest"))
}

func TestManifestCacheMiss_BearerAuth(t *testing.T) {
	// Manifest fetch through a bearer-auth protected fake upstream.
	const tokenSecret = "sekret-token-xyz"

	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","config":{},"layers":[]}`)
	mDigest := sha256Digest(manifest)

	upSrv := ociUpstreamServer(t,
		map[string][]byte{"stable": manifest, mDigest: manifest},
		nil,
		tokenSecret,
	)
	defer upSrv.Close()

	fc := newStoringFakeCache()
	h := NewHandler(fc, WithUpstream(
		upstream.NewClient(),
		[]upstream.Upstream{{Name: "secure", BaseURL: upSrv.URL, Priority: 1}},
	))
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/secured/img/manifests/stable")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode, "should succeed after bearer token dance")
	assert.Equal(t, mDigest, resp.Header.Get("Docker-Content-Digest"))

	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, manifest, body)
}

func TestBlobCacheMiss_BearerAuth(t *testing.T) {
	const tokenSecret = "blob-token-abc"

	blobData := bytes.Repeat([]byte("B"), 512)
	bDigest := sha256Digest(blobData)

	upSrv := ociUpstreamServer(t,
		nil,
		map[string][]byte{bDigest: blobData},
		tokenSecret,
	)
	defer upSrv.Close()

	fc := newStoringFakeCache()
	h := NewHandler(fc, WithUpstream(
		upstream.NewClient(),
		[]upstream.Upstream{{Name: "secure", BaseURL: upSrv.URL, Priority: 1}},
	))
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/secured/img/blobs/" + bDigest)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode, "should succeed after bearer token dance")
	assert.Equal(t, bDigest, resp.Header.Get("Docker-Content-Digest"))

	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, blobData, got)
}

// ── Hosted-first pull routing + visibility tests ─────────────────────────────

// fakeHostedResolver reports names as hosted based on a pre-configured set.
type fakeHostedResolver struct {
	hosted map[string]bool
}

func (f *fakeHostedResolver) ResolveHosted(_ context.Context, name string) (bool, error) {
	return f.hosted[name], nil
}

// fakeHostedReadAuthz grants or denies reads per repo name. A nil entry → allow.
type fakeHostedReadAuthz struct {
	deny map[string]error // repoName → error to return (nil = allow)
}

func (f *fakeHostedReadAuthz) AuthorizeRead(_ context.Context, repoName string) error {
	return f.deny[repoName]
}

func TestHostedPrivateManifest_RequiresAuth(t *testing.T) {
	// Private hosted repo: auth check fires before CAS; 401 returned.
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json"}`)
	mDigest := sha256Digest(manifest)

	fake := &fakeCacheManager{
		entries: map[string]*artifact.CacheEntry{
			"oci:acme/myapp:latest":     {Digest: mDigest, Size: int64(len(manifest)), Protocol: "oci"},
			"oci:acme/myapp:" + mDigest: {Digest: mDigest, Size: int64(len(manifest)), Protocol: "oci"},
		},
		blobs: map[string][]byte{mDigest: manifest},
	}

	h := NewHandler(fake,
		WithHostedResolver(&fakeHostedResolver{hosted: map[string]bool{"acme/myapp": true}}),
		WithHostedReadAuthz(&fakeHostedReadAuthz{deny: map[string]error{"acme/myapp": ErrUnauthorized}}),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/acme/myapp/manifests/latest")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "UNAUTHORIZED")
}

func TestHostedPublicManifest_AnonymousOk(t *testing.T) {
	// Public hosted repo: authz returns nil; manifest served from CAS.
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json"}`)
	mDigest := sha256Digest(manifest)

	fake := &fakeCacheManager{
		entries: map[string]*artifact.CacheEntry{
			"oci:acme/public:latest":     {Digest: mDigest, Size: int64(len(manifest)), Protocol: "oci"},
			"oci:acme/public:" + mDigest: {Digest: mDigest, Size: int64(len(manifest)), Protocol: "oci"},
		},
		blobs: map[string][]byte{mDigest: manifest},
	}

	h := NewHandler(fake,
		WithHostedResolver(&fakeHostedResolver{hosted: map[string]bool{"acme/public": true}}),
		WithHostedReadAuthz(&fakeHostedReadAuthz{deny: map[string]error{}}), // no denials
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/acme/public/manifests/latest")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, mDigest, resp.Header.Get("Docker-Content-Digest"))
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, manifest, body)
}

func TestHostedManifest_NotInCAS_Returns404_NotUpstream(t *testing.T) {
	// Hosted repo with content absent from CAS. Upstream is configured but must
	// NOT be contacted; a 404 is the correct response.
	upSrv := ociUpstreamServer(t,
		map[string][]byte{"latest": []byte(`{"schemaVersion":2}`)},
		nil, "",
	)
	defer upSrv.Close()

	fc := newStoringFakeCache()
	h := NewHandler(fc,
		WithUpstream(
			upstream.NewClient(),
			[]upstream.Upstream{{Name: "fake", BaseURL: upSrv.URL, Priority: 1}},
		),
		WithHostedResolver(&fakeHostedResolver{hosted: map[string]bool{"acme/private": true}}),
		WithHostedReadAuthz(&fakeHostedReadAuthz{deny: map[string]error{}}),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/acme/private/manifests/latest")
	require.NoError(t, err)
	defer resp.Body.Close()

	// Must get 404, not 200 from upstream.
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "MANIFEST_UNKNOWN")
}

func TestNonHostedManifest_FallsThroughToUpstream(t *testing.T) {
	// Resolver is wired but returns false for this name. Upstream must be used.
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","config":{},"layers":[]}`)
	mDigest := sha256Digest(manifest)

	upSrv := ociUpstreamServer(t,
		map[string][]byte{"stable": manifest, mDigest: manifest},
		nil, "",
	)
	defer upSrv.Close()

	fc := newStoringFakeCache()
	h := NewHandler(fc,
		WithUpstream(
			upstream.NewClient(),
			[]upstream.Upstream{{Name: "fake", BaseURL: upSrv.URL, Priority: 1}},
		),
		// Resolver present but returns false → non-hosted path unchanged.
		WithHostedResolver(&fakeHostedResolver{hosted: map[string]bool{}}),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/library/img/manifests/stable")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, mDigest, resp.Header.Get("Docker-Content-Digest"))
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, manifest, body)
}

func TestHostedPrivateBlob_RequiresAuth(t *testing.T) {
	// Private hosted repo: auth check fires before CAS; 401 returned for blob.
	blobData := bytes.Repeat([]byte("P"), 512)
	bDigest := sha256Digest(blobData)

	fake := &fakeCacheManager{
		entries: map[string]*artifact.CacheEntry{
			"oci:acme/myapp:" + bDigest: {Digest: bDigest, Size: int64(len(blobData)), Protocol: "oci"},
		},
		blobs: map[string][]byte{bDigest: blobData},
	}

	h := NewHandler(fake,
		WithHostedResolver(&fakeHostedResolver{hosted: map[string]bool{"acme/myapp": true}}),
		WithHostedReadAuthz(&fakeHostedReadAuthz{deny: map[string]error{"acme/myapp": ErrUnauthorized}}),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/acme/myapp/blobs/" + bDigest)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "UNAUTHORIZED")
}

func TestHostedPublicBlob_AnonymousOk(t *testing.T) {
	// Public hosted repo: authz returns nil; blob served from CAS.
	blobData := bytes.Repeat([]byte("Q"), 512)
	bDigest := sha256Digest(blobData)

	fake := &fakeCacheManager{
		entries: map[string]*artifact.CacheEntry{
			"oci:acme/public:" + bDigest: {Digest: bDigest, Size: int64(len(blobData)), Protocol: "oci"},
		},
		blobs: map[string][]byte{bDigest: blobData},
	}

	h := NewHandler(fake,
		WithHostedResolver(&fakeHostedResolver{hosted: map[string]bool{"acme/public": true}}),
		WithHostedReadAuthz(&fakeHostedReadAuthz{deny: map[string]error{}}),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/acme/public/blobs/" + bDigest)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, bDigest, resp.Header.Get("Docker-Content-Digest"))
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, blobData, got)
}

func TestHostedForbiddenBlob_Returns403(t *testing.T) {
	// Hosted repo: token present but insufficient scope → 403 DENIED.
	blobData := []byte("secret-blob")
	bDigest := sha256Digest(blobData)

	fake := &fakeCacheManager{
		entries: map[string]*artifact.CacheEntry{
			"oci:acme/secret:" + bDigest: {Digest: bDigest, Size: int64(len(blobData)), Protocol: "oci"},
		},
		blobs: map[string][]byte{bDigest: blobData},
	}

	h := NewHandler(fake,
		WithHostedResolver(&fakeHostedResolver{hosted: map[string]bool{"acme/secret": true}}),
		WithHostedReadAuthz(&fakeHostedReadAuthz{deny: map[string]error{"acme/secret": ErrForbidden}}),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/acme/secret/blobs/" + bDigest)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "DENIED")
}

func TestNonHostedBlob_FallsThroughToUpstream(t *testing.T) {
	// Resolver returns false for this name; blob must be fetched from upstream.
	blobData := bytes.Repeat([]byte("U"), 256)
	bDigest := sha256Digest(blobData)

	upSrv := ociUpstreamServer(t, nil, map[string][]byte{bDigest: blobData}, "")
	defer upSrv.Close()

	fc := newStoringFakeCache()
	h := NewHandler(fc,
		WithUpstream(
			upstream.NewClient(),
			[]upstream.Upstream{{Name: "fake", BaseURL: upSrv.URL, Priority: 1}},
		),
		WithHostedResolver(&fakeHostedResolver{hosted: map[string]bool{}}),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/library/img/blobs/" + bDigest)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, blobData, got)
}

func TestDetectManifestMediaType(t *testing.T) {
	tests := []struct {
		name     string
		json     string
		wantType string
	}{
		{
			name:     "docker manifest v2 with mediaType",
			json:     `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","layers":[]}`,
			wantType: "application/vnd.docker.distribution.manifest.v2+json",
		},
		{
			name:     "OCI image index with mediaType",
			json:     `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`,
			wantType: "application/vnd.oci.image.index.v1+json",
		},
		{
			name:     "OCI image index without mediaType (has manifests array)",
			json:     `{"schemaVersion":2,"manifests":[{"digest":"sha256:abc","size":100}]}`,
			wantType: "application/vnd.oci.image.index.v1+json",
		},
		{
			name:     "OCI image manifest without mediaType (has layers array)",
			json:     `{"schemaVersion":2,"layers":[{"digest":"sha256:abc","size":100}]}`,
			wantType: "application/vnd.oci.image.manifest.v1+json",
		},
		{
			name:     "fallback for ambiguous JSON",
			json:     `{"schemaVersion":2}`,
			wantType: "application/vnd.docker.distribution.manifest.v2+json",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := detectManifestMediaType([]byte(tc.json))
			assert.Equal(t, tc.wantType, got)
		})
	}
}
