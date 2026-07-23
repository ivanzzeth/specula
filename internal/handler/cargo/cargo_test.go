package cargo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

func TestCrateIndexPath(t *testing.T) {
	assert.Equal(t, "1/a", CrateIndexPath("a"))
	assert.Equal(t, "2/ab", CrateIndexPath("ab"))
	assert.Equal(t, "3/a/abc", CrateIndexPath("abc"))
	assert.Equal(t, "li/bc/libc", CrateIndexPath("libc"))
	assert.Equal(t, "se/rd/serde", CrateIndexPath("serde"))
}

func TestRewriteConfigJSON(t *testing.T) {
	in := []byte(`{"dl":"https://static.crates.io/crates/{crate}/{crate}-{version}.crate","api":"https://crates.io"}`)
	out := rewriteConfigJSON(in, "http://127.0.0.1:7732", "/cargo")
	var doc map[string]string
	require.NoError(t, json.Unmarshal(out, &doc))
	assert.Equal(t, "http://127.0.0.1:7732/cargo/crates", doc["dl"])
	assert.Equal(t, "http://127.0.0.1:7732/cargo", doc["api"])
}

func TestServeConfigRewritesDL(t *testing.T) {
	index := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/config.json", r.URL.Path)
		_, _ = io.WriteString(w, `{"dl":"https://static.crates.io/crates/{crate}/{crate}-{version}.crate","api":"https://crates.io"}`)
	}))
	t.Cleanup(index.Close)

	cm := newCargoTestCache()
	h := NewHandler(cm,
		WithPathPrefix("/cargo"),
		WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "idx", BaseURL: index.URL, Priority: 1},
		}),
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/cargo/index/config.json")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var doc map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&doc))
	assert.True(t, strings.HasSuffix(doc["dl"], "/cargo/crates"), "dl=%s", doc["dl"])
	assert.True(t, strings.HasSuffix(doc["api"], "/cargo"), "api=%s", doc["api"])
}

func TestServeCrateDownload(t *testing.T) {
	payload := []byte("fake-crate-bytes")
	dl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/crates/libc/libc-0.2.0.crate", r.URL.Path)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(dl.Close)

	cm := newCargoTestCache()
	h := NewHandler(cm,
		WithPathPrefix("/cargo"),
		WithUpstream(upstream.NewClient(), nil),
		WithDLUpstreams([]upstream.Upstream{{Name: "static", BaseURL: dl.URL, Priority: 1}}),
	)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/cargo/crates/libc/0.2.0/download")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, payload, got)

	resp2, err := http.Get(srv.URL + "/cargo/crates/libc/0.2.0/download")
	require.NoError(t, err)
	defer resp2.Body.Close()
	require.Equal(t, http.StatusOK, resp2.StatusCode)
}

type cargoTestCache struct {
	mu      sync.Mutex
	entries map[string]*artifact.CacheEntry
	blobs   map[string][]byte
}

func newCargoTestCache() *cargoTestCache {
	return &cargoTestCache{
		entries: make(map[string]*artifact.CacheEntry),
		blobs:   make(map[string][]byte),
	}
}

var _ cache.CacheManager = (*cargoTestCache)(nil)

func (c *cargoTestCache) key(ref artifact.ArtifactRef) string {
	return ref.Protocol + ":" + ref.Name + ":" + ref.Version
}

func (c *cargoTestCache) Lookup(_ context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[c.key(ref)], nil
}

func (c *cargoTestCache) Store(_ context.Context, ref artifact.ArtifactRef, art *artifact.Artifact) (*artifact.CacheEntry, error) {
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

func (c *cargoTestCache) Serve(_ context.Context, ref artifact.ArtifactRef, _, _ int64) (io.ReadCloser, *artifact.CacheEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[c.key(ref)]
	if e == nil {
		return nil, nil, cache.ErrCacheMiss
	}
	return io.NopCloser(bytes.NewReader(c.blobs[e.Digest])), e, nil
}

func (c *cargoTestCache) ServeEntry(_ context.Context, entry *artifact.CacheEntry, _, _ int64) (io.ReadCloser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, ok := c.blobs[entry.Digest]
	if !ok {
		return nil, cache.ErrCacheMiss
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (c *cargoTestCache) LookupStale(ctx context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	return c.Lookup(ctx, ref)
}

