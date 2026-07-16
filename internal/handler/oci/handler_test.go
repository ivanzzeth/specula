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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
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
