package tarball

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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
)

// ────────────────────────────────────────────────────────────────────────────
// Test helpers
// ────────────────────────────────────────────────────────────────────────────

// sha256sum returns "sha256:<hex>" for data.
func sha256sum(data []byte) string {
	h := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(h[:])
}

// ────────────────────────────────────────────────────────────────────────────
// fakeCacheManager — minimal double for routing/allowlist tests
// ────────────────────────────────────────────────────────────────────────────

// fakeCacheManager is a test double that reports a persistent cache miss and
// refuses Store. Sufficient for exercising the routing/validation/allowlist
// layer without triggering an upstream fetch.
type fakeCacheManager struct{}

func (f *fakeCacheManager) Lookup(_ context.Context, _ artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	return nil, nil
}

func (f *fakeCacheManager) Store(_ context.Context, _ artifact.ArtifactRef, _ *artifact.Artifact) (*artifact.CacheEntry, error) {
	return nil, fmt.Errorf("fake: Store not implemented")
}

func (f *fakeCacheManager) Serve(_ context.Context, _ artifact.ArtifactRef, _, _ int64) (io.ReadCloser, *artifact.CacheEntry, error) {
	return nil, nil, cache.ErrCacheMiss
}

var _ cache.CacheManager = (*fakeCacheManager)(nil)

// ────────────────────────────────────────────────────────────────────────────
// storingFakeCache — double that stores blobs in memory and serves them back
// ────────────────────────────────────────────────────────────────────────────

type storingFakeCache struct {
	blobs   map[string][]byte               // digest → bytes
	entries map[string]*artifact.CacheEntry // "proto:name:version" → entry
}

func newStoringFakeCache() *storingFakeCache {
	return &storingFakeCache{
		blobs:   make(map[string][]byte),
		entries: make(map[string]*artifact.CacheEntry),
	}
}

var _ cache.CacheManager = (*storingFakeCache)(nil)

func cacheKey(ref artifact.ArtifactRef) string {
	return ref.Protocol + ":" + ref.Name + ":" + ref.Version
}

func (s *storingFakeCache) Lookup(_ context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	e, ok := s.entries[cacheKey(ref)]
	if !ok {
		return nil, nil
	}
	return e, nil
}

func (s *storingFakeCache) Store(_ context.Context, ref artifact.ArtifactRef, art *artifact.Artifact) (*artifact.CacheEntry, error) {
	data, err := os.ReadFile(art.Path)
	if err != nil {
		return nil, fmt.Errorf("fake store: read quarantine: %w", err)
	}
	_ = os.Remove(art.Path)

	// Optionally enforce digest pin if ref.Digest is set.
	if ref.Digest != "" && ref.Digest != art.Digest {
		return nil, &cache.VerifyError{
			Ref: ref,
			Result: artifact.Result{
				Status:  artifact.StatusFail,
				Tier:    artifact.TierChecksum,
				Message: fmt.Sprintf("checksum: digest mismatch: got %s, expected %s", art.Digest, ref.Digest),
			},
		}
	}

	s.blobs[art.Digest] = data
	entry := &artifact.CacheEntry{
		Ref:      ref,
		Digest:   art.Digest,
		Size:     art.Size,
		Protocol: ref.Protocol,
		Upstream: art.Meta.Upstream,
	}
	s.entries[cacheKey(ref)] = entry
	return entry, nil
}

func (s *storingFakeCache) Serve(_ context.Context, ref artifact.ArtifactRef, offset, length int64) (io.ReadCloser, *artifact.CacheEntry, error) {
	e, ok := s.entries[cacheKey(ref)]
	if !ok {
		return nil, nil, cache.ErrCacheMiss
	}
	data := s.blobs[e.Digest]
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
	return io.NopCloser(bytes.NewReader(data[start:end])), e, nil
}

// ────────────────────────────────────────────────────────────────────────────
// splitURLKey unit tests
// ────────────────────────────────────────────────────────────────────────────

func TestSplitURLKey(t *testing.T) {
	tests := []struct {
		name     string
		rest     string
		wantHost string
		wantKey  string
		wantFile string
		wantOK   bool
	}{
		{
			name:     "host/dir/file",
			rest:     "example.com/path/archive.tar.gz",
			wantHost: "example.com",
			wantKey:  "example.com/path",
			wantFile: "archive.tar.gz",
			wantOK:   true,
		},
		{
			name:     "host/file (no subdir)",
			rest:     "example.com/archive.tar.gz",
			wantHost: "example.com",
			wantKey:  "example.com",
			wantFile: "archive.tar.gz",
			wantOK:   true,
		},
		{
			name:     "host:port/dir/file",
			rest:     "127.0.0.1:9876/files/pkg.tar.gz",
			wantHost: "127.0.0.1:9876",
			wantKey:  "127.0.0.1:9876/files",
			wantFile: "pkg.tar.gz",
			wantOK:   true,
		},
		{
			name:     "host:port/deep/nested/file",
			rest:     "releases.example.com:8080/v1/packages/linux/amd64/tool.tar.gz",
			wantHost: "releases.example.com:8080",
			wantKey:  "releases.example.com:8080/v1/packages/linux/amd64",
			wantFile: "tool.tar.gz",
			wantOK:   true,
		},
		{
			name:   "no slash (host only)",
			rest:   "example.com",
			wantOK: false,
		},
		{
			name:   "empty",
			rest:   "",
			wantOK: false,
		},
		{
			name:   "trailing slash",
			rest:   "example.com/path/",
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			host, key, file, ok := splitURLKey(tc.rest)
			assert.Equal(t, tc.wantOK, ok, "ok mismatch")
			if !tc.wantOK {
				return
			}
			assert.Equal(t, tc.wantHost, host, "host mismatch")
			assert.Equal(t, tc.wantKey, key, "key mismatch")
			assert.Equal(t, tc.wantFile, file, "file mismatch")
		})
	}
}

// ────────────────────────────────────────────────────────────────────────────
// tarballRef unit tests
// ────────────────────────────────────────────────────────────────────────────

func TestTarballRef(t *testing.T) {
	ref := tarballRef("example.com/releases", "v1.0.0.tar.gz")
	assert.Equal(t, Protocol, ref.Protocol)
	assert.Equal(t, "example.com/releases", ref.Name)
	assert.Equal(t, "v1.0.0.tar.gz", ref.Version)
	assert.False(t, ref.Mutable, "tarball refs must always be immutable")
	assert.Empty(t, ref.Digest, "digest is not set by tarballRef")
}

// ────────────────────────────────────────────────────────────────────────────
// isAllowedHost unit tests
// ────────────────────────────────────────────────────────────────────────────

func TestIsAllowedHost(t *testing.T) {
	h := NewHandler(&fakeCacheManager{},
		WithAllowedHosts([]string{"example.com", "releases.example.com", "127.0.0.1:9999"}),
	)

	tests := []struct {
		host    string
		allowed bool
	}{
		{"example.com", true},
		{"releases.example.com", true},
		{"127.0.0.1:9999", true},
		{"evil.com", false},
		{"example.com.evil.com", false},
		{"", false},
	}
	for _, tc := range tests {
		t.Run(tc.host, func(t *testing.T) {
			assert.Equal(t, tc.allowed, h.isAllowedHost(tc.host))
		})
	}
}

func TestIsAllowedHostEmptyList(t *testing.T) {
	// Fail-closed: empty allowlist denies all.
	h := NewHandler(&fakeCacheManager{})
	assert.False(t, h.isAllowedHost("example.com"), "empty allowlist must deny all hosts")
}

// ────────────────────────────────────────────────────────────────────────────
// Handler routing unit tests (no upstream, fake cache manager)
// ────────────────────────────────────────────────────────────────────────────

func TestHandlerMethodNotAllowed(t *testing.T) {
	h := NewHandler(&fakeCacheManager{},
		WithAllowedHosts([]string{"example.com"}),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			req, err := http.NewRequest(method, srv.URL+"/example.com/path/file.tar.gz", nil)
			require.NoError(t, err)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			_ = resp.Body.Close()
			assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode, "non-GET/HEAD must return 405")
			assert.Equal(t, "GET, HEAD", resp.Header.Get("Allow"))
		})
	}
}

func TestHandlerNotFoundEmptyPath(t *testing.T) {
	h := NewHandler(&fakeCacheManager{})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandlerNotFoundNoFile(t *testing.T) {
	h := NewHandler(&fakeCacheManager{},
		WithAllowedHosts([]string{"example.com"}),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Path with only a host and no file component.
	resp, err := http.Get(srv.URL + "/example.com")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandlerPathTraversal(t *testing.T) {
	h := NewHandler(&fakeCacheManager{},
		WithAllowedHosts([]string{"example.com"}),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/example.com/../../../etc/passwd")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "path traversal must return 404")
}

func TestHandlerForbiddenHost(t *testing.T) {
	h := NewHandler(&fakeCacheManager{},
		WithAllowedHosts([]string{"allowed.example.com"}),
		WithScheme("http"),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/evil.example.com/path/file.tar.gz")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "disallowed host must return 403")
}

func TestHandlerHEADReturnsNoBody(t *testing.T) {
	data := []byte("hello tarball content")
	digest := sha256sum(data)

	sc := newStoringFakeCache()
	// Pre-populate the cache so HEAD is a cache hit without needing an upstream.
	ref := tarballRef("example.com/files", "hello.tar.gz")
	sc.entries[cacheKey(ref)] = &artifact.CacheEntry{
		Ref:      ref,
		Digest:   digest,
		Size:     int64(len(data)),
		Protocol: Protocol,
	}
	sc.blobs[digest] = data

	h := NewHandler(sc, WithAllowedHosts([]string{"example.com"}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, err := http.NewRequest(http.MethodHead, srv.URL+"/example.com/files/hello.tar.gz", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Empty(t, body, "HEAD response must have no body")
}

// ────────────────────────────────────────────────────────────────────────────
// Fetch-and-cache unit test (fake upstream + storingFakeCache)
// ────────────────────────────────────────────────────────────────────────────

func TestHandlerFetchAndCache(t *testing.T) {
	content := []byte("fake tarball content for unit test")

	// Fake upstream file server.
	var hits int
	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("ETag", `"etag-abc"`)
		_, _ = w.Write(content)
	}))
	defer fakeUpstream.Close()

	host := fakeUpstream.Listener.Addr().String()
	sc := newStoringFakeCache()
	tmp := t.TempDir()

	h := NewHandler(sc,
		WithAllowedHosts([]string{host}),
		WithScheme("http"),
		WithQuarantineDir(tmp),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	// First fetch — cold cache: must contact upstream.
	resp, err := http.Get(srv.URL + "/" + host + "/files/archive.tar.gz")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, content, body, "response bytes must match upstream content")
	assert.Equal(t, 1, hits, "first fetch must contact upstream exactly once")

	// Second fetch — warm cache: upstream must NOT be contacted again.
	resp2, err := http.Get(srv.URL + "/" + host + "/files/archive.tar.gz")
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)

	body2, _ := io.ReadAll(resp2.Body)
	assert.Equal(t, content, body2, "second fetch must return same bytes")
	assert.Equal(t, 1, hits, "second fetch must NOT re-contact upstream (cache hit)")
}

// ────────────────────────────────────────────────────────────────────────────
// Digest pin unit tests
// ────────────────────────────────────────────────────────────────────────────

func TestHandlerDigestPinCorrect(t *testing.T) {
	content := []byte("pinned tarball content for unit test")
	correctDigest := sha256sum(content)

	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	defer fakeUpstream.Close()

	host := fakeUpstream.Listener.Addr().String()
	sc := newStoringFakeCache()
	tmp := t.TempDir()

	h := NewHandler(sc,
		WithAllowedHosts([]string{host}),
		WithScheme("http"),
		WithQuarantineDir(tmp),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	url := srv.URL + "/" + host + "/releases/pkg.tar.gz?digest=" + correctDigest
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "correct digest pin must succeed")

	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, content, body)
}

func TestHandlerDigestPinMismatch(t *testing.T) {
	content := []byte("real tarball content")
	wrongDigest := sha256sum([]byte("completely different content — NOT what the server returns"))
	require.NotEqual(t, sha256sum(content), wrongDigest, "precondition: digests must differ")

	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	defer fakeUpstream.Close()

	host := fakeUpstream.Listener.Addr().String()
	sc := newStoringFakeCache()
	tmp := t.TempDir()

	h := NewHandler(sc,
		WithAllowedHosts([]string{host}),
		WithScheme("http"),
		WithQuarantineDir(tmp),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	url := srv.URL + "/" + host + "/releases/pkg.tar.gz?digest=" + wrongDigest
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode,
		"wrong digest pin must cause verify-on-write failure (502)")

	// CAS must remain empty — the quarantine file must have been removed.
	assert.Empty(t, sc.blobs, "CAS must not contain any blob after verify-on-write failure")
}

// ────────────────────────────────────────────────────────────────────────────
// Upstream 404 forwarding
// ────────────────────────────────────────────────────────────────────────────

func TestHandlerUpstream404(t *testing.T) {
	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer fakeUpstream.Close()

	host := fakeUpstream.Listener.Addr().String()
	h := NewHandler(newStoringFakeCache(),
		WithAllowedHosts([]string{host}),
		WithScheme("http"),
		WithQuarantineDir(t.TempDir()),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/" + host + "/missing/file.tar.gz")
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode,
		"upstream 404 must produce 502 (upstream fetch failed)")
}

// ────────────────────────────────────────────────────────────────────────────
// WithPathPrefix
// ────────────────────────────────────────────────────────────────────────────

func TestHandlerPathPrefix(t *testing.T) {
	content := []byte("prefix test content")

	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	defer fakeUpstream.Close()

	host := fakeUpstream.Listener.Addr().String()
	h := NewHandler(newStoringFakeCache(),
		WithAllowedHosts([]string{host}),
		WithScheme("http"),
		WithQuarantineDir(t.TempDir()),
		WithPathPrefix("/tarball"),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/tarball/" + host + "/dir/file.tar.gz")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "WithPathPrefix must strip prefix before routing")

	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, content, body)
}
