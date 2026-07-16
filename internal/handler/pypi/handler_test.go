package pypi

import (
	"bytes"
	"context"
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

// ── pypiTestCache — fake cache.CacheManager ──────────────────────────────────
//
// Entries are keyed by "protocol:name:version", matching the real SQLite
// MetadataStore's composite primary key so lookups and stores are symmetric.

type pypiTestCache struct {
	mu      sync.Mutex
	entries map[string]*artifact.CacheEntry
	blobs   map[string][]byte
}

var _ cache.CacheManager = (*pypiTestCache)(nil)

func newPypiTestCache() *pypiTestCache {
	return &pypiTestCache{
		entries: make(map[string]*artifact.CacheEntry),
		blobs:   make(map[string][]byte),
	}
}

func (c *pypiTestCache) key(ref artifact.ArtifactRef) string {
	return ref.Protocol + ":" + ref.Name + ":" + ref.Version
}

func (c *pypiTestCache) Lookup(_ context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[c.key(ref)], nil
}

func (c *pypiTestCache) Store(_ context.Context, ref artifact.ArtifactRef, art *artifact.Artifact) (*artifact.CacheEntry, error) {
	data, err := os.ReadFile(art.Path)
	if err != nil {
		return nil, fmt.Errorf("pypiTestCache.Store: read %s: %w", art.Path, err)
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

func (c *pypiTestCache) Serve(_ context.Context, ref artifact.ArtifactRef, offset, length int64) (io.ReadCloser, *artifact.CacheEntry, error) {
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

// seed pre-populates the cache so tests can exercise the cache-hit path.
func (c *pypiTestCache) seed(ref artifact.ArtifactRef, data []byte) {
	digest := "sha256:test-" + ref.Name + "-" + ref.Version
	c.mu.Lock()
	defer c.mu.Unlock()
	c.blobs[digest] = data
	c.entries[c.key(ref)] = &artifact.CacheEntry{
		Ref:      ref,
		Digest:   digest,
		Size:     int64(len(data)),
		Protocol: ref.Protocol,
	}
}

// ── fakePyPIServer — minimal fake PyPI simple index server ────────────────────

// fakePyPIServer serves:
//
//	GET /simple/<project>/            → simplePages[normalised project]
//	GET /packages/<path>/<file>       → packageFiles[file]
func fakePyPIServer(t *testing.T, simplePages map[string][]byte, packageFiles map[string][]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path

		if strings.HasPrefix(p, "/simple/") {
			project, ok := projectFromSimplePath(p)
			if !ok {
				http.NotFound(w, r)
				return
			}
			body, ok := simplePages[project]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", ctSimpleHTML)
			_, _ = w.Write(body)
			return
		}

		if strings.HasPrefix(p, "/packages/") {
			_, file, ok := splitPackageFile(p)
			if !ok {
				http.NotFound(w, r)
				return
			}
			data, ok := packageFiles[file]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", ctWheel)
			_, _ = w.Write(data)
			return
		}

		http.NotFound(w, r)
	}))
	return srv
}

// ── handler helpers ────────────────────────────────────────────────────────────

func newPypiHandlerWithUpstream(cm cache.CacheManager, upstreamURL string) *Handler {
	return NewHandler(cm,
		WithUpstream(
			upstream.NewClient(),
			[]upstream.Upstream{{Name: "fake-pypi", BaseURL: upstreamURL, Priority: 1}},
		),
		WithMutableTTL(300),
	)
}

// ── tests: PEP 503 normalisation ─────────────────────────────────────────────

func TestNormalizeProject(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Flask", "flask"},
		{"Flask", "flask"},
		{"my_lib", "my-lib"},
		{"My.Package", "my-package"},
		{"my---lib", "my-lib"},
		{"my_.lib", "my-lib"},
		{"numpy", "numpy"},
		{"Django_REST_framework", "django-rest-framework"},
		{"A", "a"},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := normalizeProject(tc.input)
			assert.Equal(t, tc.want, got)
		})
	}
}

// ── tests: path helpers ───────────────────────────────────────────────────────

func TestProjectFromSimplePath(t *testing.T) {
	tests := []struct {
		path   string
		want   string
		wantOK bool
	}{
		{"/simple/flask/", "flask", true},
		{"/simple/Flask/", "flask", true},
		{"/simple/my_lib/", "my-lib", true},
		{"/simple/numpy/", "numpy", true},
		// Edge cases: no project or extra path segments.
		{"/simple/", "", false},
		{"/simple/flask/extra/", "", false},
		{"/simple/flask/extra", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			got, ok := projectFromSimplePath(tc.path)
			assert.Equal(t, tc.wantOK, ok, "ok")
			if tc.wantOK {
				assert.Equal(t, tc.want, got)
			}
		})
	}
}

func TestSplitPackageFile(t *testing.T) {
	tests := []struct {
		path     string
		wantName string
		wantFile string
		wantOK   bool
	}{
		{"/packages/ab/data/flask-2.0.whl", "ab/data", "flask-2.0.whl", true},
		{"/packages/a1/numpy-1.0-cp39.whl", "a1", "numpy-1.0-cp39.whl", true},
		// No file component.
		{"/packages/ab/", "", "", false},
		{"/packages/", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			name, file, ok := splitPackageFile(tc.path)
			assert.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				assert.Equal(t, tc.wantName, name)
				assert.Equal(t, tc.wantFile, file)
			}
		})
	}
}

func TestExtractProjectFromFile(t *testing.T) {
	tests := []struct {
		file string
		want string
	}{
		{"numpy-1.21.0-cp39-cp39-linux_x86_64.whl", "numpy"},
		{"Flask-2.3.0.tar.gz", "flask"},
		{"Django-4.0-py3-none-any.whl", "django"},
		{"my_lib-0.1.0.whl", "my-lib"},
		{"my_lib-0.1.0.tar.gz", "my-lib"},
		// No separator — can't extract.
		{"noversion.whl", ""},
	}
	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			assert.Equal(t, tc.want, extractProjectFromFile(tc.file))
		})
	}
}

// ── tests: GET /simple/<project>/ ────────────────────────────────────────────

func TestSimpleIndex_CacheHit_NoUpstream(t *testing.T) {
	const project = "flask"
	body := []byte(`<!DOCTYPE html><html><body><a href="Flask-2.0.whl">Flask-2.0.whl</a></body></html>`)

	ref := indexRefForRequest(project, httptest.NewRequest(http.MethodGet, "/simple/flask/", nil))
	cm := newPypiTestCache()
	cm.seed(ref, body)

	h := NewHandler(cm) // no upstream
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/simple/flask/")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")

	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, body, got)
}

func TestSimpleIndex_CacheMiss_FetchFromUpstream(t *testing.T) {
	const project = "numpy"
	body := []byte(`<!DOCTYPE html><html><body><a href="numpy-1.0.whl">numpy-1.0.whl</a></body></html>`)

	upSrv := fakePyPIServer(t, map[string][]byte{project: body}, nil)
	defer upSrv.Close()

	cm := newPypiTestCache()
	h := newPypiHandlerWithUpstream(cm, upSrv.URL)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/simple/numpy/")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")

	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, body, got)

	// Second request should come from cache.
	resp2, err := http.Get(srv.URL + "/simple/numpy/")
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)
	got2, _ := io.ReadAll(resp2.Body)
	assert.Equal(t, body, got2)
}

// TestSimpleIndex_NormalisedName verifies the same cache entry is hit regardless
// of the casing / separator variant used in the URL (PEP 503 normalisation).
func TestSimpleIndex_NormalisedName(t *testing.T) {
	// Seed using the normalised key "my-lib".
	ref := artifact.ArtifactRef{
		Protocol: Protocol,
		Name:     "my-lib",
		Version:  indexVersion,
		Mutable:  true,
	}
	body := []byte(`<html><body><a href="my_lib-1.0.whl">my_lib-1.0.whl</a></body></html>`)
	cm := newPypiTestCache()
	cm.seed(ref, body)

	h := NewHandler(cm)
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Request using "my_lib" → normalises to "my-lib" → cache hit.
	resp, err := http.Get(srv.URL + "/simple/my_lib/")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, body, got)
}

// ── tests: GET /packages/<path>/<file> ────────────────────────────────────────

func TestPackageFile_CacheHit_NoUpstream(t *testing.T) {
	const whlData = "fake-wheel-bytes-for-test"
	ref := fileRef("ab/data", "mypackage-1.0-py3-none-any.whl")

	cm := newPypiTestCache()
	cm.seed(ref, []byte(whlData))

	h := NewHandler(cm) // no upstream
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/packages/ab/data/mypackage-1.0-py3-none-any.whl")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, []byte(whlData), got)
}

func TestPackageFile_CacheMiss_FetchFromUpstream(t *testing.T) {
	whlBytes := bytes.Repeat([]byte("WHEEL_CONTENT"), 16)

	upSrv := fakePyPIServer(t, nil, map[string][]byte{
		"flask-2.0-py3-none-any.whl": whlBytes,
	})
	defer upSrv.Close()

	cm := newPypiTestCache()
	h := newPypiHandlerWithUpstream(cm, upSrv.URL)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/packages/ab/cd/flask-2.0-py3-none-any.whl")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, whlBytes, got)

	// Verify the file is now in cache.
	ref := fileRef("ab/cd", "flask-2.0-py3-none-any.whl")
	entry, lookErr := cm.Lookup(context.Background(), ref)
	require.NoError(t, lookErr)
	assert.NotNil(t, entry, "wheel should be cached after upstream fetch")
}

// ── tests: method enforcement ─────────────────────────────────────────────────

func TestMethodNotAllowed(t *testing.T) {
	h := NewHandler(newPypiTestCache())
	srv := httptest.NewServer(h)
	defer srv.Close()

	for _, path := range []string{
		"/simple/flask/",
		"/packages/ab/cd/flask-2.0-py3-none-any.whl",
	} {
		t.Run(path, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, srv.URL+path, nil)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
			assert.Equal(t, "GET, HEAD", resp.Header.Get("Allow"))
		})
	}
}

// ── tests: dependency-confusion guard ─────────────────────────────────────────

func TestPrivateName_FailClosed_NoPrivateUpstream(t *testing.T) {
	// "corp-internal" is configured as private but no private upstream is given.
	// With failClosed=true (default), this must return 503.
	h := NewHandler(newPypiTestCache(),
		WithPrivateNames([]string{"corp-internal"}),
		// no WithPrivateUpstream
		WithFailClosed(true),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/simple/corp-internal/")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

func TestPrivateName_FailClosed_PrivateUpstreamDown(t *testing.T) {
	// Private upstream is configured but unreachable.
	// The handler must return 503, not fall through to the public mirror.
	h := NewHandler(newPypiTestCache(),
		WithPrivateNames([]string{"corp-lib"}),
		WithPrivateUpstream(upstream.Upstream{
			Name:     "private",
			BaseURL:  "http://127.0.0.1:0", // nothing listening here
			Priority: 0,
		}),
		WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "public", BaseURL: "https://pypi.org", Priority: 1},
		}),
		WithFailClosed(true),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/simple/corp-lib/")
	require.NoError(t, err)
	defer resp.Body.Close()
	// Any non-200 is acceptable; the key assertion is it is NOT 200 from the
	// public mirror that would be proxied under a different private package name.
	assert.NotEqual(t, http.StatusOK, resp.StatusCode)
}

func TestPublicName_NotRoutedToPrivate(t *testing.T) {
	const publicBody = `<html><body><a href="flask-2.0.whl">flask-2.0.whl</a></body></html>`

	// Public upstream serves the public package.
	pubSrv := fakePyPIServer(t,
		map[string][]byte{"flask": []byte(publicBody)},
		nil,
	)
	defer pubSrv.Close()

	// Private upstream serves nothing for "flask" — if it were contacted, the
	// test would fail with 404.
	prvSrv := fakePyPIServer(t, nil, nil)
	defer prvSrv.Close()

	h := NewHandler(newPypiTestCache(),
		WithPrivateNames([]string{"corp-internal"}), // flask is NOT private
		WithPrivateUpstream(upstream.Upstream{
			Name: "private", BaseURL: prvSrv.URL, Priority: 0,
		}),
		WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "public", BaseURL: pubSrv.URL, Priority: 1},
		}),
		WithMutableTTL(300),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/simple/flask/")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	got, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(got), "flask-2.0.whl")
}

// ── tests: path prefix option ─────────────────────────────────────────────────

func TestPathPrefix(t *testing.T) {
	const project = "requests"
	body := []byte(`<html><body><a href="requests-2.0.whl">requests-2.0.whl</a></body></html>`)

	ref := indexRefForRequest(project, httptest.NewRequest(http.MethodGet, "/pypi/simple/requests/", nil))
	cm := newPypiTestCache()
	cm.seed(ref, body)

	h := NewHandler(cm, WithPathPrefix("/pypi"))
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Must succeed at prefixed path.
	resp, err := http.Get(srv.URL + "/pypi/simple/requests/")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, body, got)
}

// ── tests: unknown route ──────────────────────────────────────────────────────

func TestUnknownPath_404(t *testing.T) {
	h := NewHandler(newPypiTestCache())
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/not/a/pypi/path")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ── tests: not-found without upstream ─────────────────────────────────────────

func TestNoUpstream_NotFound(t *testing.T) {
	h := NewHandler(newPypiTestCache()) // empty cache, no upstream
	srv := httptest.NewServer(h)
	defer srv.Close()

	for _, path := range []string{
		"/simple/flask/",
		"/packages/ab/cd/flask-2.0-py3-none-any.whl",
	} {
		t.Run(path, func(t *testing.T) {
			resp, err := http.Get(srv.URL + path)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusNotFound, resp.StatusCode)
		})
	}
}

// ── tests: HEAD request ───────────────────────────────────────────────────────

func TestHEAD_CacheHit(t *testing.T) {
	const project = "flask"
	body := []byte(`<html><body><a href="flask-2.0.whl">flask-2.0.whl</a></body></html>`)

	ref := indexRefForRequest(project, httptest.NewRequest(http.MethodGet, "/simple/flask/", nil))
	cm := newPypiTestCache()
	cm.seed(ref, body)

	h := NewHandler(cm)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodHead, srv.URL+"/simple/flask/", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")
	// HEAD must not return a body.
	body2, _ := io.ReadAll(resp.Body)
	assert.Empty(t, body2)
}

// ── tests: dependency-confusion guard (Depconf / Confusion) ──────────────────

// TestDepconfGuard_IsPrivate_PypiNameNorm verifies that the wired guard performs
// PEP 503 normalisation on both the configured name and the incoming project so
// variants like "Corp_Internal" and "corp-internal" are treated identically.
func TestDepconfGuard_IsPrivate_PypiNameNorm(t *testing.T) {
	h := NewHandler(newPypiTestCache(),
		WithPrivateNames([]string{"Corp_Internal", "mycompany-*"}),
	)
	require.NotNil(t, h.guard, "guard must be wired when private names are set")

	tests := []struct {
		project string
		want    bool
	}{
		{"corp-internal", true},        // PEP 503 normalised match
		{"Corp_Internal", true},        // raw name also normalised
		{"mycompany-foo", true},        // glob match
		{"mycompany-bar", true},        // glob match (second pattern)
		{"flask", false},               // public
		{"corp-internal-extra", false}, // no prefix-only match
	}
	for _, tc := range tests {
		t.Run(tc.project, func(t *testing.T) {
			assert.Equal(t, tc.want, h.isPrivate(tc.project))
		})
	}
}

// TestDepconfPypi_PrivateNameServedFromPrivateOnly verifies that a private name
// is routed exclusively to the private upstream and the public mirror is NEVER
// contacted, even when the public mirror would serve it.
func TestDepconfPypi_PrivateNameServedFromPrivateOnly(t *testing.T) {
	const privatePkg = "corp-internal"
	privateBody := []byte(`<html><body><a href="corp_internal-1.0.whl">corp_internal-1.0.whl</a></body></html>`)
	publicBody := []byte(`<html><body><a href="corp_internal-2.0.whl">corp_internal-2.0.whl</a></body></html>`)

	// Private upstream — the only one that should be contacted.
	prvSrv := fakePyPIServer(t,
		map[string][]byte{privatePkg: privateBody},
		nil,
	)
	defer prvSrv.Close()

	// Public upstream — has "higher" version but must NEVER be used.
	var publicHits int
	pubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		publicHits++
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(publicBody)
	}))
	defer pubSrv.Close()

	cm := newPypiTestCache()
	h := NewHandler(cm,
		WithPrivateNames([]string{privatePkg}),
		WithPrivateUpstream(upstream.Upstream{Name: "private", BaseURL: prvSrv.URL, Priority: 0}),
		WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "public", BaseURL: pubSrv.URL, Priority: 1},
		}),
		WithMutableTTL(300),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/simple/corp-internal/")
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, privateBody, got, "response must be from private upstream, not public")
	assert.Zero(t, publicHits, "public upstream must never be contacted for a private name")
}

// TestDepconfPypi_PublicHigherVersionIgnored verifies that a public index offering
// a higher version is ignored for a private package: the private upstream's
// response is served and the public mirror receives no requests.
func TestDepconfPypi_PublicHigherVersionIgnored(t *testing.T) {
	const privatePkg = "acme-sdk"
	privateIndex := []byte(`<html><body><a href="acme_sdk-1.0.0.whl">acme_sdk-1.0.0.whl</a></body></html>`)
	publicIndex := []byte(`<html><body><a href="acme_sdk-9.9.9.whl">acme_sdk-9.9.9.whl</a></body></html>`)

	prvSrv := fakePyPIServer(t, map[string][]byte{privatePkg: privateIndex}, nil)
	defer prvSrv.Close()

	publicHits := 0
	pubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		publicHits++
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(publicIndex)
	}))
	defer pubSrv.Close()

	cm := newPypiTestCache()
	h := NewHandler(cm,
		WithPrivateNames([]string{privatePkg}),
		WithPrivateUpstream(upstream.Upstream{Name: "private", BaseURL: prvSrv.URL}),
		WithUpstream(upstream.NewClient(), []upstream.Upstream{{Name: "public", BaseURL: pubSrv.URL}}),
		WithMutableTTL(300),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/simple/" + privatePkg + "/")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, privateIndex, got, "private index must be served (not public higher-version)")
	assert.Zero(t, publicHits, "public mirror must receive zero requests for private names")
}

// TestDepconfPypi_PrivateDown_FailClosed verifies that when the private upstream
// is unreachable, Specula returns 5xx and NEVER falls through to the public mirror.
func TestDepconfPypi_PrivateDown_FailClosed(t *testing.T) {
	const privatePkg = "corp-lib"
	publicBody := []byte(`<html><body><a href="corp_lib-1.0.whl">corp_lib-1.0.whl</a></body></html>`)

	// Public upstream is healthy and would serve the package — but must NEVER be used.
	pubSrv := fakePyPIServer(t, map[string][]byte{privatePkg: publicBody}, nil)
	defer pubSrv.Close()

	cm := newPypiTestCache()
	h := NewHandler(cm,
		WithPrivateNames([]string{privatePkg}),
		WithPrivateUpstream(upstream.Upstream{
			Name:    "private",
			BaseURL: "http://127.0.0.1:0", // nothing listening → connection refused
		}),
		WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "public", BaseURL: pubSrv.URL},
		}),
		WithFailClosed(true),
		WithMutableTTL(0), // always revalidate so upstream is contacted
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/simple/corp-lib/")
	require.NoError(t, err)
	defer resp.Body.Close()

	// Must NOT be 200 from the public mirror.
	assert.NotEqual(t, http.StatusOK, resp.StatusCode,
		"private-upstream-down must not return 200 from public (dep-confusion protection)")
	assert.GreaterOrEqual(t, resp.StatusCode, 500,
		"response must be a 5xx error, not a redirect to the public mirror")
}

// TestDepconfPypi_PrivateNamed_NoPublicFallthrough_EvenWithFailClosedFalse ensures
// that the "public fallthrough" bug is fixed: even when failClosed=false, a private
// name with a DOWN private upstream must NOT serve from the public mirror (only
// stale cache or 5xx is permitted).
func TestDepconfPypi_PrivateNamed_NoPublicFallthrough_EvenWithFailClosedFalse(t *testing.T) {
	const privatePkg = "internal-pkg"
	publicBody := []byte(`<html><body>public version 2.0</body></html>`)

	// Public upstream has the package — but must NEVER be consulted for private names.
	pubSrv := fakePyPIServer(t, map[string][]byte{privatePkg: publicBody}, nil)
	defer pubSrv.Close()

	cm := newPypiTestCache()
	h := NewHandler(cm,
		WithPrivateNames([]string{privatePkg}),
		// No private upstream → used to fall through to public when failClosed=false.
		WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "public", BaseURL: pubSrv.URL},
		}),
		WithFailClosed(false), // previously caused the public fallthrough bug
		WithMutableTTL(0),
	)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/simple/" + privatePkg + "/")
	require.NoError(t, err)
	defer resp.Body.Close()

	// The fix: selectUpstreams error → always fail, never fall to public.
	assert.NotEqual(t, http.StatusOK, resp.StatusCode,
		"private name with no private upstream must NEVER return 200 from public (dep-confusion)")
}

// ── tests: JSON index (PEP 691 Accept header) ─────────────────────────────────

func TestSimpleIndex_JSONAccept_CacheHit(t *testing.T) {
	const project = "flask"
	jsonBody := []byte(`{"meta":{"api-version":"1.0"},"name":"flask","files":[]}`)

	// Seed the JSON format slot (indexVersionJSON).
	ref := artifact.ArtifactRef{
		Protocol: Protocol,
		Name:     project,
		Version:  indexVersionJSON,
		Mutable:  true,
	}
	cm := newPypiTestCache()
	cm.seed(ref, jsonBody)

	h := NewHandler(cm)
	srv := httptest.NewServer(h)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/simple/flask/", nil)
	req.Header.Set("Accept", ctSimpleJSON)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), ctSimpleJSON)
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, jsonBody, got)
}
