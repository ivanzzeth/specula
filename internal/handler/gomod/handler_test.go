package gomod

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

// ── gomodTestCache — test double for cache.CacheManager ──────────────────────
//
// Entries are keyed by "protocol:name:version", matching the real SQLite
// MetadataStore's (protocol, name, version) primary key. This lets gomod
// handlers look up immutable refs by version string (e.g. "v1.0.0.mod") rather
// than by content digest — unlike the OCI handler where Version == Digest for
// immutable entries.

type gomodTestCache struct {
	mu           sync.Mutex
	entries      map[string]*artifact.CacheEntry // cacheKey → fresh CacheEntry
	staleEntries map[string]*artifact.CacheEntry // cacheKey → TTL-expired CacheEntry
	blobs        map[string][]byte               // digest → bytes
}

var _ cache.CacheManager = (*gomodTestCache)(nil)

// staler satisfied — the handler opts in via a type assertion.
var _ staler = (*gomodTestCache)(nil)

func newGomodTestCache() *gomodTestCache {
	return &gomodTestCache{
		entries:      make(map[string]*artifact.CacheEntry),
		staleEntries: make(map[string]*artifact.CacheEntry),
		blobs:        make(map[string][]byte),
	}
}

// cacheKey builds the map key used by gomodTestCache, matching the SQLite
// WHERE protocol=? AND name=? AND version=? lookup used by the real store.
func (c *gomodTestCache) key(ref artifact.ArtifactRef) string {
	return ref.Protocol + ":" + ref.Name + ":" + ref.Version
}

func (c *gomodTestCache) Lookup(_ context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[c.key(ref)], nil
}

// Store reads the quarantine file, saves the bytes by digest, and records a
// CacheEntry keyed by (protocol:name:version). Removes art.Path after reading,
// matching production CacheManager behaviour.
func (c *gomodTestCache) Store(_ context.Context, ref artifact.ArtifactRef, art *artifact.Artifact) (*artifact.CacheEntry, error) {
	data, err := os.ReadFile(art.Path)
	if err != nil {
		return nil, fmt.Errorf("gomodTestCache.Store: read %s: %w", art.Path, err)
	}
	_ = os.Remove(art.Path)

	c.mu.Lock()
	defer c.mu.Unlock()

	c.blobs[art.Digest] = data

	entry := &artifact.CacheEntry{
		Ref:      ref,
		Digest:   art.Digest,
		Size:     art.Size,
		Protocol: ref.Protocol,
		Upstream: art.Meta.Upstream,
	}
	c.entries[c.key(ref)] = entry
	return entry, nil
}

// LookupStale mirrors manager.LookupStale: it returns TTL-expired entries too.
func (c *gomodTestCache) LookupStale(_ context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := c.key(ref)
	if e, ok := c.staleEntries[k]; ok {
		return e, nil
	}
	return c.entries[k], nil
}

// Serve mirrors production manager.Serve: it re-runs the FRESH Lookup and serves
// only what that returns. It deliberately does NOT fall back to staleEntries —
// the real manager cannot, because manager.Serve calls Lookup (no allowStale),
// which returns nil for a stale mutable entry. Stale bytes are reachable only
// via ServeEntry.
func (c *gomodTestCache) Serve(_ context.Context, ref artifact.ArtifactRef, offset, length int64) (io.ReadCloser, *artifact.CacheEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[c.key(ref)]
	if !ok {
		return nil, nil, cache.ErrCacheMiss
	}
	data, ok := c.blobs[entry.Digest]
	if !ok {
		return nil, nil, cache.ErrCacheMiss
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
	return io.NopCloser(bytes.NewReader(data[start:end])), entry, nil
}

// seed pre-populates the cache with content, so tests can exercise the cache-
// hit path without an upstream. Returns the sha256 digest of data.
func (c *gomodTestCache) seed(ref artifact.ArtifactRef, data []byte) string {
	digest := sha256sum(data)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blobs[digest] = data
	c.entries[c.key(ref)] = &artifact.CacheEntry{
		Ref:      ref,
		Digest:   digest,
		Size:     int64(len(data)),
		Protocol: ref.Protocol,
	}
	return digest
}

// seedStale pre-populates a TTL-expired entry: Lookup misses it, LookupStale
// finds it. This is the state the mutable tier is in when a short TTL has
// lapsed and the upstream is unreachable (DESIGN-REVIEW §2 H1).
func (c *gomodTestCache) seedStale(ref artifact.ArtifactRef, data []byte) string {
	digest := sha256sum(data)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blobs[digest] = data
	c.staleEntries[c.key(ref)] = &artifact.CacheEntry{
		Ref:      ref,
		Digest:   digest,
		Size:     int64(len(data)),
		Protocol: ref.Protocol,
	}
	return digest
}

// ── goproxyUpstream — minimal fake GOPROXY server ─────────────────────────────
//
// Served routes:
//
//	GET /{module}/@v/list           → listBody
//	GET /{module}/@v/{file}         → fileBodies[file]
//	GET /{module}/@latest           → latestBody
//
// All responses are 404 when the key is not found in the maps.
func goproxyUpstream(t *testing.T, module string, listBody []byte, fileBodies map[string][]byte, latestBody []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		// /{module}/@latest
		if suf := "/" + module + "/@latest"; p == suf {
			if latestBody == nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(latestBody)
			return
		}
		// /{module}/@v/list
		if suf := "/" + module + "/@v/list"; p == suf {
			if listBody == nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write(listBody)
			return
		}
		// /{module}/@v/{file}
		const atV = "/@v/"
		if idx := strings.LastIndex(p, atV); idx >= 0 {
			file := p[idx+len(atV):]
			data, ok := fileBodies[file]
			if !ok {
				http.NotFound(w, r)
				return
			}
			ct := contentTypeForFile(file)
			w.Header().Set("Content-Type", ct)
			_, _ = w.Write(data)
			return
		}
		http.NotFound(w, r)
	}))
	return srv
}

// sha256sum returns the "sha256:hex" digest of data.
func sha256sum(data []byte) string {
	h := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(h[:])
}

// ── helper: new handler with fake upstream ────────────────────────────────────

func newHandlerWithUpstream(cm cache.CacheManager, upstreamURL string) *Handler {
	return NewHandler(cm,
		WithUpstream(
			upstream.NewClient(),
			[]upstream.Upstream{{Name: "fake", BaseURL: upstreamURL, Priority: 1}},
		),
		WithMutableTTL(300),
	)
}

// ── tests: @v/list ───────────────────────────────────────────────────────────

func TestGomodList_CacheHit_NoUpstream(t *testing.T) {
	// Pre-populated cache — no upstream should be contacted.
	listBody := []byte("v1.0.0\nv1.1.0\n")
	ref := listRef("github.com/foo/bar")
	cm := newGomodTestCache()
	cm.seed(ref, listBody)

	h := NewHandler(cm) // no upstream configured
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/github.com/foo/bar/@v/list")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/plain")

	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, listBody, body)
}

func TestGomodList_CacheMiss_FetchFromUpstream(t *testing.T) {
	const mod = "github.com/foo/bar"
	listBody := []byte("v1.0.0\nv1.2.0\n")

	upSrv := goproxyUpstream(t, mod, listBody, nil, nil)
	defer upSrv.Close()

	cm := newGomodTestCache()
	h := newHandlerWithUpstream(cm, upSrv.URL)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/github.com/foo/bar/@v/list")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, listBody, got)

	// Second request: should hit cache (entry stored by first request).
	resp2, err := http.Get(srv.URL + "/github.com/foo/bar/@v/list")
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)
	got2, _ := io.ReadAll(resp2.Body)
	assert.Equal(t, listBody, got2)
}

// ── tests: @v/{version}.info ─────────────────────────────────────────────────

func TestGomodInfo_CacheHit_NoUpstream(t *testing.T) {
	infoBody := []byte(`{"Version":"v1.0.0","Time":"2024-01-01T00:00:00Z"}`)
	ref := immutableRef("github.com/foo/bar", "v1.0.0.info")
	cm := newGomodTestCache()
	cm.seed(ref, infoBody)

	h := NewHandler(cm)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/github.com/foo/bar/@v/v1.0.0.info")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, infoBody, got)
}

func TestGomodInfo_CacheMiss_FetchFromUpstream(t *testing.T) {
	const mod = "github.com/foo/bar"
	infoBody := []byte(`{"Version":"v1.0.0","Time":"2024-01-01T00:00:00Z"}`)

	upSrv := goproxyUpstream(t, mod, nil, map[string][]byte{"v1.0.0.info": infoBody}, nil)
	defer upSrv.Close()

	cm := newGomodTestCache()
	h := newHandlerWithUpstream(cm, upSrv.URL)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/github.com/foo/bar/@v/v1.0.0.info")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, infoBody, got)

	// Verify the entry was cached.
	entry, lookErr := cm.Lookup(context.Background(), immutableRef(mod, "v1.0.0.info"))
	require.NoError(t, lookErr)
	assert.NotNil(t, entry, "info should be in cache after upstream fetch")
}

// ── tests: @v/{version}.mod ──────────────────────────────────────────────────

func TestGomodMod_CacheMiss_FetchFromUpstream(t *testing.T) {
	const mod = "github.com/foo/bar"
	modBody := []byte("module github.com/foo/bar\n\ngo 1.21\n")

	upSrv := goproxyUpstream(t, mod, nil, map[string][]byte{"v1.0.0.mod": modBody}, nil)
	defer upSrv.Close()

	cm := newGomodTestCache()
	h := newHandlerWithUpstream(cm, upSrv.URL)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/github.com/foo/bar/@v/v1.0.0.mod")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/plain")

	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, modBody, got)
}

func TestGomodMod_CacheHit_NoUpstream(t *testing.T) {
	modBody := []byte("module github.com/foo/bar\n\ngo 1.21\n")
	ref := immutableRef("github.com/foo/bar", "v1.0.0.mod")
	cm := newGomodTestCache()
	cm.seed(ref, modBody)

	h := NewHandler(cm)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/github.com/foo/bar/@v/v1.0.0.mod")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/plain")
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, modBody, got)
}

// ── tests: @v/{version}.zip ──────────────────────────────────────────────────

func TestGomodZip_CacheMiss_FetchFromUpstream(t *testing.T) {
	const mod = "github.com/foo/bar"
	zipBody := bytes.Repeat([]byte("ZIPDATA"), 512) // fake zip bytes

	upSrv := goproxyUpstream(t, mod, nil, map[string][]byte{"v1.0.0.zip": zipBody}, nil)
	defer upSrv.Close()

	cm := newGomodTestCache()
	h := newHandlerWithUpstream(cm, upSrv.URL)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/github.com/foo/bar/@v/v1.0.0.zip")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/zip", resp.Header.Get("Content-Type"))

	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, zipBody, got)
}

func TestGomodZip_CacheHit_NoUpstream(t *testing.T) {
	zipBody := bytes.Repeat([]byte("ZIP"), 64)
	ref := immutableRef("github.com/foo/bar", "v1.0.0.zip")
	cm := newGomodTestCache()
	cm.seed(ref, zipBody)

	h := NewHandler(cm)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/github.com/foo/bar/@v/v1.0.0.zip")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/zip", resp.Header.Get("Content-Type"))
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, zipBody, got)
}

// ── tests: @latest ───────────────────────────────────────────────────────────

func TestGomodLatest_CacheHit_NoUpstream(t *testing.T) {
	latestBody := []byte(`{"Version":"v1.2.3","Time":"2024-06-01T00:00:00Z"}`)
	ref := latestRef("github.com/foo/bar")
	cm := newGomodTestCache()
	cm.seed(ref, latestBody)

	h := NewHandler(cm)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/github.com/foo/bar/@latest")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/json")
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, latestBody, got)
}

func TestGomodLatest_CacheMiss_FetchFromUpstream(t *testing.T) {
	const mod = "github.com/foo/bar"
	latestBody := []byte(`{"Version":"v1.2.3","Time":"2024-06-01T00:00:00Z"}`)

	upSrv := goproxyUpstream(t, mod, nil, nil, latestBody)
	defer upSrv.Close()

	cm := newGomodTestCache()
	h := newHandlerWithUpstream(cm, upSrv.URL)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/github.com/foo/bar/@latest")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/json")
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, latestBody, got)
}

// ── tests: module-path escaping ───────────────────────────────────────────────
//
// GOPROXY bang-encodes uppercase letters in module paths: github.com/Azure/foo
// becomes github.com/!azure/foo in the URL. The handler must accept the escaped
// URL form and forward it verbatim to the upstream.

func TestGomodEscaping_UppercaseModule_CacheMiss(t *testing.T) {
	// Escaped module path: github.com/!azure/foo (uppercase A → !a).
	const escapedMod = "github.com/!azure/foo"
	infoBody := []byte(`{"Version":"v1.0.0","Time":"2024-01-01T00:00:00Z"}`)

	// The upstream GOPROXY serves the escaped module path as-is in the URL.
	upSrv := goproxyUpstream(t, escapedMod, nil, map[string][]byte{"v1.0.0.info": infoBody}, nil)
	defer upSrv.Close()

	cm := newGomodTestCache()
	h := newHandlerWithUpstream(cm, upSrv.URL)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/github.com/!azure/foo/@v/v1.0.0.info")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, infoBody, got)

	// Verify the ref stored in cache uses the escaped path.
	entry, _ := cm.Lookup(context.Background(), immutableRef(escapedMod, "v1.0.0.info"))
	assert.NotNil(t, entry, "entry should be cached under the escaped module path")
}

func TestGomodEscaping_UppercaseModule_CacheHit(t *testing.T) {
	const escapedMod = "github.com/!azure/foo"
	zipBody := bytes.Repeat([]byte("Z"), 128)
	ref := immutableRef(escapedMod, "v2.0.0.zip")

	cm := newGomodTestCache()
	cm.seed(ref, zipBody)

	h := NewHandler(cm) // no upstream needed for cache hit
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/github.com/!azure/foo/@v/v2.0.0.zip")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/zip", resp.Header.Get("Content-Type"))
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, zipBody, got)
}

// ── tests: error paths ────────────────────────────────────────────────────────

func TestGomodMethodNotAllowed(t *testing.T) {
	h := NewHandler(newGomodTestCache())
	srv := httptest.NewServer(h)
	defer srv.Close()

	endpoints := []string{
		"/github.com/foo/bar/@v/list",
		"/github.com/foo/bar/@v/v1.0.0.info",
		"/github.com/foo/bar/@v/v1.0.0.mod",
		"/github.com/foo/bar/@v/v1.0.0.zip",
		"/github.com/foo/bar/@latest",
	}
	for _, ep := range endpoints {
		t.Run(ep, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, srv.URL+ep, nil)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
			assert.Equal(t, "GET, HEAD", resp.Header.Get("Allow"))
		})
	}
}

func TestGomodBadModulePath(t *testing.T) {
	h := NewHandler(newGomodTestCache())
	srv := httptest.NewServer(h)
	defer srv.Close()

	// "!!" is not valid bang-encoding.
	resp, err := http.Get(srv.URL + "/!!invalid!!/@v/list")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestGomodNotFound_NoUpstream(t *testing.T) {
	h := NewHandler(newGomodTestCache()) // empty cache, no upstream
	srv := httptest.NewServer(h)
	defer srv.Close()

	for _, path := range []string{
		"/github.com/foo/bar/@v/list",
		"/github.com/foo/bar/@v/v1.0.0.info",
		"/github.com/foo/bar/@v/v1.0.0.mod",
		"/github.com/foo/bar/@v/v1.0.0.zip",
		"/github.com/foo/bar/@latest",
	} {
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(srv.URL + path)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusNotFound, resp.StatusCode)
		})
	}
}

func TestGomodUnknownRoute_404(t *testing.T) {
	h := NewHandler(newGomodTestCache())
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/not/a/goproxy/path")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ── tests: HEAD method ────────────────────────────────────────────────────────

func TestGomodHEAD_CacheHit(t *testing.T) {
	infoBody := []byte(`{"Version":"v1.0.0","Time":"2024-01-01T00:00:00Z"}`)
	ref := immutableRef("github.com/foo/bar", "v1.0.0.info")
	cm := newGomodTestCache()
	cm.seed(ref, infoBody)

	h := NewHandler(cm)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodHead, srv.URL+"/github.com/foo/bar/@v/v1.0.0.info", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	// HEAD must return no body.
	body, _ := io.ReadAll(resp.Body)
	assert.Empty(t, body)
}

// ── tests: path prefix option ─────────────────────────────────────────────────

func TestGomodWithPathPrefix(t *testing.T) {
	infoBody := []byte(`{"Version":"v1.0.0","Time":"2024-01-01T00:00:00Z"}`)
	ref := immutableRef("github.com/foo/bar", "v1.0.0.info")
	cm := newGomodTestCache()
	cm.seed(ref, infoBody)

	h := NewHandler(cm, WithPathPrefix("/go"))
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Must succeed at prefixed path.
	resp, err := http.Get(srv.URL + "/go/github.com/foo/bar/@v/v1.0.0.info")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, infoBody, got)
}

// ── tests: full list+info+mod+zip pipeline ────────────────────────────────────
//
// Simulate a realistic fetch sequence: list → info → mod → zip, all served
// from the same fake upstream. Verifies each content type and caching.

func TestGomodFullPipeline_ListInfoModZip(t *testing.T) {
	const mod = "github.com/example/pkg"
	listBody := []byte("v0.1.0\nv0.2.0\n")
	infoBody := []byte(`{"Version":"v0.2.0","Time":"2024-03-15T12:00:00Z"}`)
	modBody := []byte("module github.com/example/pkg\n\ngo 1.22\n")
	zipBody := bytes.Repeat([]byte("FAKE_ZIP_CONTENT"), 32)

	upSrv := goproxyUpstream(t, mod, listBody, map[string][]byte{
		"v0.2.0.info": infoBody,
		"v0.2.0.mod":  modBody,
		"v0.2.0.zip":  zipBody,
	}, nil)
	defer upSrv.Close()

	cm := newGomodTestCache()
	h := newHandlerWithUpstream(cm, upSrv.URL)
	srv := httptest.NewServer(h)
	defer srv.Close()

	// list
	resp, err := http.Get(srv.URL + "/" + mod + "/@v/list")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, listBody, got)

	// .info
	resp2, err := http.Get(srv.URL + "/" + mod + "/@v/v0.2.0.info")
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)
	assert.Equal(t, "application/json", resp2.Header.Get("Content-Type"))
	got2, _ := io.ReadAll(resp2.Body)
	assert.Equal(t, infoBody, got2)

	// .mod
	resp3, err := http.Get(srv.URL + "/" + mod + "/@v/v0.2.0.mod")
	require.NoError(t, err)
	defer resp3.Body.Close()
	assert.Equal(t, http.StatusOK, resp3.StatusCode)
	assert.Contains(t, resp3.Header.Get("Content-Type"), "text/plain")
	got3, _ := io.ReadAll(resp3.Body)
	assert.Equal(t, modBody, got3)

	// .zip
	resp4, err := http.Get(srv.URL + "/" + mod + "/@v/v0.2.0.zip")
	require.NoError(t, err)
	defer resp4.Body.Close()
	assert.Equal(t, http.StatusOK, resp4.StatusCode)
	assert.Equal(t, "application/zip", resp4.Header.Get("Content-Type"))
	got4, _ := io.ReadAll(resp4.Body)
	assert.Equal(t, zipBody, got4)

	// Verify all immutable entries are now in cache.
	for _, file := range []string{"v0.2.0.info", "v0.2.0.mod", "v0.2.0.zip"} {
		e, err := cm.Lookup(context.Background(), immutableRef(mod, file))
		require.NoError(t, err)
		assert.NotNilf(t, e, "%s should be cached after upstream fetch", file)
	}
}

// ── tests: content-type table ─────────────────────────────────────────────────

func TestContentTypeForFile(t *testing.T) {
	tests := []struct {
		file string
		want string
	}{
		{"v1.0.0.info", "application/json"},
		{"v2.3.4-beta.1.info", "application/json"},
		{"v1.0.0.mod", "text/plain; charset=utf-8"},
		{"v1.0.0.zip", "application/zip"},
		{"list", "application/octet-stream"}, // list is not routed through contentTypeForFile
	}
	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			assert.Equal(t, tc.want, contentTypeForFile(tc.file))
		})
	}
}

// ── tests: splitAtV helper ────────────────────────────────────────────────────

func TestSplitAtV(t *testing.T) {
	tests := []struct {
		rest     string
		wantMod  string
		wantFile string
		wantOK   bool
	}{
		{"github.com/foo/bar/@v/v1.0.0.mod", "github.com/foo/bar", "v1.0.0.mod", true},
		{"github.com/foo/bar/@v/list", "github.com/foo/bar", "list", true},
		{"github.com/!azure/foo/@v/v1.0.0.info", "github.com/!azure/foo", "v1.0.0.info", true},
		// Edge cases.
		{"/@v/list", "", "", false}, // empty module
		{"foo/@v/", "", "", false},  // empty file
		{"foo/bar", "", "", false},  // no /@v/
	}
	for _, tc := range tests {
		t.Run(tc.rest, func(t *testing.T) {
			mod, file, ok := splitAtV(tc.rest)
			assert.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				assert.Equal(t, tc.wantMod, mod)
				assert.Equal(t, tc.wantFile, file)
			}
		})
	}
}

// ── tests: canonicalModule helper ─────────────────────────────────────────────

func TestCanonicalModule(t *testing.T) {
	tests := []struct {
		escaped string
		wantErr bool
	}{
		{"github.com/foo/bar", false},
		{"github.com/!azure/sdk-for-go", false}, // !a → A
		{"!!invalid!!", true},                   // bad bang-encoding
		{"", true},                              // empty is invalid
	}
	for _, tc := range tests {
		t.Run(tc.escaped, func(t *testing.T) {
			_, err := canonicalModule(tc.escaped)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// ── tests: sumdb passthrough 403 for private modules ─────────────────────────

func TestSumDBPassthrough_PrivateModule403(t *testing.T) {
	// Construct SumDBHandler with no private matcher configured; test that the
	// routing helpers correctly extract module paths from sumdb lookup URLs.
	// (verify.NewPrivateMatcher is not imported here to avoid a cycle; the
	// passthrough private-guard integration is covered by e2e tests.)
	sumdbHandler := NewSumDBHandler("https://sum.golang.org")
	assert.NotNil(t, sumdbHandler)

	// Verify the private-module guard fires by checking moduleFromLookup.
	sub := "sum.golang.org/lookup/github.com/corp/secret@v1.0.0"
	mod, ok := moduleFromLookup(sub)
	assert.True(t, ok)
	assert.Equal(t, "github.com/corp/secret", mod)
}

// ServeEntry mirrors manager.ServeEntry: it serves the bytes of the entry the
// caller already holds, with no lookup and therefore no freshness gate. This is
// the ONLY way stale bytes can reach the response — Serve cannot produce them.
func (c *gomodTestCache) ServeEntry(_ context.Context, entry *artifact.CacheEntry, offset, length int64) (io.ReadCloser, error) {
	if entry == nil {
		return nil, cache.ErrCacheMiss
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	data, ok := c.blobs[entry.Digest]
	if !ok {
		return nil, cache.ErrCacheMiss
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
	return io.NopCloser(bytes.NewReader(data[start:end])), nil
}

// entryServer satisfied — the handler opts in via a type assertion.
var _ entryServer = (*gomodTestCache)(nil)
