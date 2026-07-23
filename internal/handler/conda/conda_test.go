package conda

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func TestServeRepodata(t *testing.T) {
	payload := []byte(`{"packages":{}}`)
	index := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/conda-forge/linux-64/repodata.json", r.URL.Path)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(index.Close)

	cm := newCondaTestCache()
	h := NewHandler(cm,
		WithPathPrefix("/conda"),
		WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "conda", BaseURL: index.URL, Priority: 1},
		}),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/conda/conda-forge/linux-64/repodata.json")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, payload, got)

	resp2, err := http.Get(srv.URL + "/conda/conda-forge/linux-64/repodata.json")
	require.NoError(t, err)
	defer resp2.Body.Close()
	require.Equal(t, http.StatusOK, resp2.StatusCode)
}

func TestServePackage(t *testing.T) {
	payload := []byte("fake-conda-package")
	dl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/conda-forge/linux-64/numpy-1.26.0-py312h123_0.conda", r.URL.Path)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(dl.Close)

	cm := newCondaTestCache()
	h := NewHandler(cm,
		WithPathPrefix("/conda"),
		WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "conda", BaseURL: dl.URL, Priority: 1},
		}),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/conda/conda-forge/linux-64/numpy-1.26.0-py312h123_0.conda")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, payload, got)

	resp2, err := http.Get(srv.URL + "/conda/conda-forge/linux-64/numpy-1.26.0-py312h123_0.conda")
	require.NoError(t, err)
	defer resp2.Body.Close()
	require.Equal(t, http.StatusOK, resp2.StatusCode)
}

type condaTestCache struct {
	mu      sync.Mutex
	entries map[string]*artifact.CacheEntry
	blobs   map[string][]byte
}

func newCondaTestCache() *condaTestCache {
	return &condaTestCache{
		entries: make(map[string]*artifact.CacheEntry),
		blobs:   make(map[string][]byte),
	}
}

var _ cache.CacheManager = (*condaTestCache)(nil)

func (c *condaTestCache) key(ref artifact.ArtifactRef) string {
	return ref.Protocol + ":" + ref.Name + ":" + ref.Version
}

func (c *condaTestCache) Lookup(_ context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[c.key(ref)], nil
}

func (c *condaTestCache) Store(_ context.Context, ref artifact.ArtifactRef, art *artifact.Artifact) (*artifact.CacheEntry, error) {
	data, err := os.ReadFile(art.Path)
	if err != nil {
		return nil, err
	}
	_ = os.Remove(art.Path)
	sum := sha256.Sum256(data)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	entry := &artifact.CacheEntry{
		Ref:      ref,
		Digest:   digest,
		Size:     int64(len(data)),
		Protocol: ref.Protocol,
	}
	c.mu.Lock()
	c.entries[c.key(ref)] = entry
	c.blobs[digest] = data
	c.mu.Unlock()
	return entry, nil
}

func (c *condaTestCache) Serve(_ context.Context, ref artifact.ArtifactRef, _, _ int64) (io.ReadCloser, *artifact.CacheEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[c.key(ref)]
	if e == nil {
		return nil, nil, cache.ErrCacheMiss
	}
	return io.NopCloser(bytes.NewReader(c.blobs[e.Digest])), e, nil
}

func (c *condaTestCache) ServeEntry(_ context.Context, entry *artifact.CacheEntry, _, _ int64) (io.ReadCloser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, ok := c.blobs[entry.Digest]
	if !ok {
		return nil, cache.ErrCacheMiss
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (c *condaTestCache) LookupStale(ctx context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	return c.Lookup(ctx, ref)
}
