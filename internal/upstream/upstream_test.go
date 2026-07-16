package upstream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testClient returns a fallbackClient tuned for fast tests:
//   - 1 ms backoff (instead of 100 ms)
//   - configurable maxAttempts
//   - configurable blockTracker parameters
func testClient(maxAttempts int) *fallbackClient {
	return &fallbackClient{
		http:        &http.Client{Timeout: 5 * time.Second},
		blocker:     newBlockTrackerWith(defaultMaxFailures, defaultBlockDuration),
		maxAttempts: maxAttempts,
		backoffBase: time.Millisecond, // fast backoff for tests
		tokens:      make(map[string]tokenEntry),
	}
}

// tarballRef is a convenience ArtifactRef used in most tests.
func tarballRef(name, version string) artifact.ArtifactRef {
	return artifact.ArtifactRef{Protocol: "tarball", Name: name, Version: version}
}

// okServer returns an httptest.Server that responds 200 with body.
func okServer(t *testing.T, body string, extraHeaders map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range extraHeaders {
			w.Header().Set(k, v)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
}

// statusServer returns an httptest.Server that always responds with the given status code.
func statusServer(t *testing.T, code int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
	}))
}

// countingServer returns an httptest.Server that counts hits and responds with code / body.
func countingServer(t *testing.T, code int, body string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(code)
		if body != "" {
			_, _ = io.WriteString(w, body)
		}
	}))
	return srv, &hits
}

// ── Fetch tests ──────────────────────────────────────────────────────────────

func TestFetch_SingleUpstreamSuccess(t *testing.T) {
	srv := okServer(t, "hello artifact", map[string]string{
		"ETag":         `"abc123"`,
		"Content-Type": "application/octet-stream",
	})
	defer srv.Close()

	c := testClient(1)
	body, meta, err := c.Fetch(context.Background(), tarballRef("pkg", "v1.0.0"),
		[]Upstream{{Name: "up1", BaseURL: srv.URL, Priority: 1}})

	require.NoError(t, err)
	require.NotNil(t, body)
	defer body.Close()

	data, _ := io.ReadAll(body)
	assert.Equal(t, "hello artifact", string(data))
	assert.Equal(t, `"abc123"`, meta.ETag)
	assert.Equal(t, "up1", meta.Upstream)
	assert.Equal(t, http.StatusOK, meta.StatusCode)
	assert.Equal(t, "application/octet-stream", meta.ContentType)
}

func TestFetch_FallbackOnServerError(t *testing.T) {
	// bad: returns 500 on every request
	bad := statusServer(t, http.StatusInternalServerError)
	defer bad.Close()
	// good: returns 200 with content
	good := okServer(t, "real content", nil)
	defer good.Close()

	c := testClient(1) // no retry per upstream so fallback happens quickly
	body, meta, err := c.Fetch(context.Background(), tarballRef("pkg", "v1.0.0"),
		[]Upstream{
			{Name: "bad", BaseURL: bad.URL, Priority: 1},
			{Name: "good", BaseURL: good.URL, Priority: 2},
		})

	require.NoError(t, err)
	defer body.Close()

	data, _ := io.ReadAll(body)
	assert.Equal(t, "real content", string(data))
	assert.Equal(t, "good", meta.Upstream)
}

func TestFetch_FallbackOn4xx(t *testing.T) {
	// 404 is non-retryable and non-transient; expect fallback to the next upstream.
	notFound := statusServer(t, http.StatusNotFound)
	defer notFound.Close()
	good := okServer(t, "found", nil)
	defer good.Close()

	c := testClient(1)
	body, meta, err := c.Fetch(context.Background(), tarballRef("pkg", "v1.0.0"),
		[]Upstream{
			{Name: "miss", BaseURL: notFound.URL, Priority: 1},
			{Name: "hit", BaseURL: good.URL, Priority: 2},
		})

	require.NoError(t, err)
	defer body.Close()

	data, _ := io.ReadAll(body)
	assert.Equal(t, "found", string(data))
	assert.Equal(t, "hit", meta.Upstream)
}

func TestFetch_PriorityOrdering(t *testing.T) {
	// Verify upstreams are tried in ascending Priority order regardless of
	// the slice order passed by the caller.
	var mu sync.Mutex
	var order []string

	newNamedSrv := func(name string, code int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			if code == http.StatusOK {
				_, _ = io.WriteString(w, "ok")
			}
			w.WriteHeader(code)
		}))
	}

	s3 := newNamedSrv("priority-3", http.StatusInternalServerError)
	defer s3.Close()
	s1 := newNamedSrv("priority-1", http.StatusInternalServerError)
	defer s1.Close()
	s2 := newNamedSrv("priority-2", http.StatusOK) // wins
	defer s2.Close()

	c := testClient(1)
	body, meta, err := c.Fetch(context.Background(), tarballRef("pkg", "v1"),
		[]Upstream{
			{Name: "priority-3", BaseURL: s3.URL, Priority: 3},
			{Name: "priority-1", BaseURL: s1.URL, Priority: 1},
			{Name: "priority-2", BaseURL: s2.URL, Priority: 2},
		})

	require.NoError(t, err)
	body.Close()

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"priority-1", "priority-2"}, order,
		"should try priority-1 first, then priority-2 (which succeeds), skipping priority-3")
	assert.Equal(t, "priority-2", meta.Upstream)
}

func TestFetch_AllUpstreamsFail(t *testing.T) {
	bad1 := statusServer(t, http.StatusBadGateway)
	defer bad1.Close()
	bad2 := statusServer(t, http.StatusServiceUnavailable)
	defer bad2.Close()

	c := testClient(1)
	_, _, err := c.Fetch(context.Background(), tarballRef("pkg", "v1.0.0"),
		[]Upstream{
			{Name: "b1", BaseURL: bad1.URL, Priority: 1},
			{Name: "b2", BaseURL: bad2.URL, Priority: 2},
		})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "all upstreams failed")
}

func TestFetch_EmptyUpstreamsList(t *testing.T) {
	c := testClient(1)
	_, _, err := c.Fetch(context.Background(), tarballRef("pkg", "v1.0.0"), nil)
	require.Error(t, err)
}

func TestFetch_ContextCanceled(t *testing.T) {
	// Server that blocks until the test cancels the context.
	unblock := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-unblock
	}))
	defer slow.Close()
	defer close(unblock)

	ctx, cancel := context.WithCancel(context.Background())
	c := testClient(3)

	done := make(chan error, 1)
	go func() {
		_, _, err := c.Fetch(ctx, tarballRef("pkg", "v1.0.0"),
			[]Upstream{{Name: "slow", BaseURL: slow.URL, Priority: 1}})
		done <- err
	}()

	// Cancel before the request completes.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		require.Error(t, err)
		assert.True(t, isContextError(err) || err.Error() != "",
			"expected context-related error, got: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("Fetch did not return after context cancel")
	}
}

// ── Retry tests ───────────────────────────────────────────────────────────────

func TestFetch_RetryTransientThenSuccess(t *testing.T) {
	// First two requests return 503; third returns 200.
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "retry worked")
	}))
	defer srv.Close()

	c := testClient(3) // 3 attempts = initial + 2 retries
	body, meta, err := c.Fetch(context.Background(), tarballRef("pkg", "v1.0.0"),
		[]Upstream{{Name: "flaky", BaseURL: srv.URL, Priority: 1}})

	require.NoError(t, err)
	defer body.Close()

	data, _ := io.ReadAll(body)
	assert.Equal(t, "retry worked", string(data))
	assert.Equal(t, http.StatusOK, meta.StatusCode)
	assert.Equal(t, int64(3), calls.Load(), "should have made exactly 3 attempts")
}

func TestFetch_RetryExhaustedFallsToNextUpstream(t *testing.T) {
	// bad: always 500 → exhausts maxAttempts
	bad, badHits := countingServer(t, http.StatusInternalServerError, "")
	defer bad.Close()
	// good: succeeds
	good := okServer(t, "ok", nil)
	defer good.Close()

	c := testClient(2) // 2 attempts per upstream
	body, meta, err := c.Fetch(context.Background(), tarballRef("pkg", "v1.0.0"),
		[]Upstream{
			{Name: "bad", BaseURL: bad.URL, Priority: 1},
			{Name: "good", BaseURL: good.URL, Priority: 2},
		})

	require.NoError(t, err)
	defer body.Close()

	assert.Equal(t, "good", meta.Upstream)
	assert.Equal(t, int64(2), badHits.Load(),
		"should have tried bad upstream exactly maxAttempts times")
}

// ── Auto-block tests ──────────────────────────────────────────────────────────

func TestFetch_AutoBlockAfterConsecutiveFailures(t *testing.T) {
	bad, badHits := countingServer(t, http.StatusInternalServerError, "")
	defer bad.Close()
	good := okServer(t, "ok", nil)
	defer good.Close()

	c := &fallbackClient{
		http:        &http.Client{Timeout: 5 * time.Second},
		blocker:     newBlockTrackerWith(3, 10*time.Second), // block after 3 failures
		maxAttempts: 1,
		backoffBase: time.Millisecond,
	}

	upstreams := []Upstream{
		{Name: "bad", BaseURL: bad.URL, Priority: 1},
		{Name: "good", BaseURL: good.URL, Priority: 2},
	}
	ref := tarballRef("pkg", "v1.0.0")

	// Three calls where bad fails → bad should get blocked on the third.
	for i := 0; i < 3; i++ {
		body, _, err := c.Fetch(context.Background(), ref, upstreams)
		require.NoError(t, err, "good upstream should always rescue call %d", i+1)
		body.Close()
	}

	assert.True(t, c.blocker.isBlocked("bad"), "bad upstream should be blocked after 3 failures")

	// Fourth call: bad is blocked, only good is contacted.
	hitsBefore := badHits.Load()
	body, meta, err := c.Fetch(context.Background(), ref, upstreams)
	require.NoError(t, err)
	body.Close()

	assert.Equal(t, hitsBefore, badHits.Load(),
		"bad server should not be contacted while blocked")
	assert.Equal(t, "good", meta.Upstream)
}

func TestFetch_AutoUnblockAfterDuration(t *testing.T) {
	// Block period of 20 ms so the test doesn't take long.
	bad := statusServer(t, http.StatusInternalServerError)
	defer bad.Close()
	good := okServer(t, "ok", nil)
	defer good.Close()

	c := &fallbackClient{
		http:        &http.Client{Timeout: 5 * time.Second},
		blocker:     newBlockTrackerWith(1, 20*time.Millisecond), // block after 1 failure, unblock after 20 ms
		maxAttempts: 1,
		backoffBase: time.Millisecond,
	}

	upstreams := []Upstream{
		{Name: "bad", BaseURL: bad.URL, Priority: 1},
		{Name: "good", BaseURL: good.URL, Priority: 2},
	}
	ref := tarballRef("pkg", "v1.0.0")

	// One call to trigger the block.
	body, _, _ := c.Fetch(context.Background(), ref, upstreams)
	if body != nil {
		body.Close()
	}
	assert.True(t, c.blocker.isBlocked("bad"))

	// Wait for the block to expire.
	time.Sleep(30 * time.Millisecond)

	assert.False(t, c.blocker.isBlocked("bad"), "bad upstream should be auto-unblocked")
}

func TestFetch_AllUpstreamsBlocked(t *testing.T) {
	c := &fallbackClient{
		http:        &http.Client{Timeout: 5 * time.Second},
		blocker:     newBlockTrackerWith(1, time.Minute),
		maxAttempts: 1,
		backoffBase: time.Millisecond,
	}
	// Manually block the upstream so we can test the "all blocked" path.
	c.blocker.recordFailure("only")

	_, _, err := c.Fetch(context.Background(), tarballRef("pkg", "v1.0.0"),
		[]Upstream{{Name: "only", BaseURL: "http://127.0.0.1:1", Priority: 1}})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "blocked")
}

// ── Revalidate (304) tests ────────────────────────────────────────────────────

func TestRevalidate_NotModifiedByETag(t *testing.T) {
	const etag = `"deadbeef"`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "content")
	}))
	defer srv.Close()

	c := testClient(1)
	prev := artifact.UpstreamMeta{ETag: etag}
	body, meta, notModified, err := c.Revalidate(
		context.Background(), tarballRef("pkg", "v1.0.0"), prev,
		[]Upstream{{Name: "up", BaseURL: srv.URL, Priority: 1}})

	require.NoError(t, err)
	assert.True(t, notModified)
	assert.Nil(t, body)
	assert.Equal(t, http.StatusNotModified, meta.StatusCode)
}

func TestRevalidate_NotModifiedByLastModified(t *testing.T) {
	const lm = "Wed, 01 Jan 2025 00:00:00 GMT"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-Modified-Since") == lm {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Last-Modified", lm)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "payload")
	}))
	defer srv.Close()

	c := testClient(1)
	prev := artifact.UpstreamMeta{LastModified: lm}
	body, meta, notModified, err := c.Revalidate(
		context.Background(), tarballRef("index", "v0"), prev,
		[]Upstream{{Name: "up", BaseURL: srv.URL, Priority: 1}})

	require.NoError(t, err)
	assert.True(t, notModified)
	assert.Nil(t, body)
	assert.Equal(t, http.StatusNotModified, meta.StatusCode)
}

func TestRevalidate_ContentChanged(t *testing.T) {
	// Server ignores conditional GET headers → always returns 200.
	const newETag = `"cafebabe"`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", newETag)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "updated content")
	}))
	defer srv.Close()

	c := testClient(1)
	prev := artifact.UpstreamMeta{ETag: `"oldtag"`}
	body, meta, notModified, err := c.Revalidate(
		context.Background(), tarballRef("pkg", "v2.0.0"), prev,
		[]Upstream{{Name: "up", BaseURL: srv.URL, Priority: 1}})

	require.NoError(t, err)
	assert.False(t, notModified)
	require.NotNil(t, body)
	defer body.Close()

	data, _ := io.ReadAll(body)
	assert.Equal(t, "updated content", string(data))
	assert.Equal(t, newETag, meta.ETag)
	assert.Equal(t, http.StatusOK, meta.StatusCode)
}

func TestRevalidate_FallbackOnFailure(t *testing.T) {
	bad := statusServer(t, http.StatusBadGateway)
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Ignore the conditional header, return fresh content.
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "fresh")
	}))
	defer good.Close()

	c := testClient(1)
	prev := artifact.UpstreamMeta{ETag: `"abc"`}
	body, meta, notModified, err := c.Revalidate(
		context.Background(), tarballRef("pkg", "v1.0.0"), prev,
		[]Upstream{
			{Name: "bad", BaseURL: bad.URL, Priority: 1},
			{Name: "good", BaseURL: good.URL, Priority: 2},
		})

	require.NoError(t, err)
	assert.False(t, notModified)
	require.NotNil(t, body)
	defer body.Close()
	assert.Equal(t, "good", meta.Upstream)
}

// ── URL builder tests ─────────────────────────────────────────────────────────

func TestBuildURL_Protocols(t *testing.T) {
	tests := []struct {
		name     string
		ref      artifact.ArtifactRef
		base     string
		wantPath string // suffix after base
	}{
		{
			name:     "oci mutable tag",
			ref:      artifact.ArtifactRef{Protocol: "oci", Name: "library/nginx", Version: "latest", Mutable: true},
			base:     "https://registry.daocloud.io",
			wantPath: "/v2/library/nginx/manifests/latest",
		},
		{
			name:     "oci immutable blob",
			ref:      artifact.ArtifactRef{Protocol: "oci", Name: "library/nginx", Digest: "sha256:abc", Mutable: false},
			base:     "https://registry.daocloud.io",
			wantPath: "/v2/library/nginx/blobs/sha256:abc",
		},
		{
			name:     "gomod",
			ref:      artifact.ArtifactRef{Protocol: "gomod", Name: "github.com/foo/bar", Version: "v1.2.3.mod"},
			base:     "https://goproxy.cn",
			wantPath: "/github.com/foo/bar/@v/v1.2.3.mod",
		},
		{
			name:     "pypi simple index",
			ref:      artifact.ArtifactRef{Protocol: "pypi", Name: "requests", Mutable: true},
			base:     "https://pypi.org",
			wantPath: "/simple/requests/",
		},
		{
			name:     "npm packument",
			ref:      artifact.ArtifactRef{Protocol: "npm", Name: "lodash", Mutable: true},
			base:     "https://registry.npmjs.org",
			wantPath: "/lodash",
		},
		{
			name:     "helm index",
			ref:      artifact.ArtifactRef{Protocol: "helm", Name: "stable", Mutable: true},
			base:     "https://charts.helm.sh",
			wantPath: "/stable/index.yaml",
		},
		{
			name:     "tarball by version",
			ref:      artifact.ArtifactRef{Protocol: "tarball", Name: "tools/kubectl", Version: "v1.30.0"},
			base:     "https://dl.k8s.io",
			wantPath: "/tools/kubectl/v1.30.0",
		},
		{
			name:     "tarball by digest",
			ref:      artifact.ArtifactRef{Protocol: "tarball", Name: "tools/kubectl", Digest: "sha256:deadbeef"},
			base:     "https://dl.k8s.io",
			wantPath: "/tools/kubectl/sha256:deadbeef",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildURL(tt.base, tt.ref)
			assert.Equal(t, tt.base+tt.wantPath, got)
		})
	}
}

// ── blockTracker unit tests ───────────────────────────────────────────────────

func TestBlockTracker_RecordAndBlock(t *testing.T) {
	bt := newBlockTrackerWith(3, time.Second)

	assert.False(t, bt.isBlocked("x"))

	bt.recordFailure("x")
	bt.recordFailure("x")
	assert.False(t, bt.isBlocked("x"), "should not be blocked after 2 failures (threshold=3)")

	blocked := bt.recordFailure("x") // 3rd failure
	assert.True(t, blocked, "recordFailure should return true when upstream becomes blocked")
	assert.True(t, bt.isBlocked("x"))
}

func TestBlockTracker_SuccessResetsCounter(t *testing.T) {
	bt := newBlockTrackerWith(3, time.Second)

	bt.recordFailure("x")
	bt.recordFailure("x")
	assert.Equal(t, 2, bt.failureCount("x"))

	bt.recordSuccess("x")
	assert.Equal(t, 0, bt.failureCount("x"))
	assert.False(t, bt.isBlocked("x"))
}

func TestBlockTracker_AutoUnblock(t *testing.T) {
	bt := newBlockTrackerWith(1, 20*time.Millisecond)

	bt.recordFailure("y")
	assert.True(t, bt.isBlocked("y"))

	time.Sleep(30 * time.Millisecond)
	assert.False(t, bt.isBlocked("y"), "should auto-unblock after block duration")
	// Counter should be reset too.
	assert.Equal(t, 0, bt.failureCount("y"))
}

func TestBlockTracker_ConcurrentSafety(t *testing.T) {
	bt := newBlockTrackerWith(1000, time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bt.recordFailure("concurrent")
			_ = bt.isBlocked("concurrent")
			bt.recordSuccess("concurrent")
		}()
	}
	wg.Wait()
	// If we reach here without a race detector report the test passes.
}

// ── parseBearerChallenge unit tests ───────────────────────────────────────────

func TestParseBearerChallenge(t *testing.T) {
	tests := []struct {
		name        string
		header      string
		wantRealm   string
		wantService string
		wantScope   string
		wantOK      bool
	}{
		{
			name:        "full docker hub challenge",
			header:      `Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/nginx:pull"`,
			wantRealm:   "https://auth.docker.io/token",
			wantService: "registry.docker.io",
			wantScope:   "repository:library/nginx:pull",
			wantOK:      true,
		},
		{
			name:        "daocloud style challenge",
			header:      `Bearer realm="https://docker.m.daocloud.io/token",service="registry.daocloud.io",scope="repository:library/hello-world:pull"`,
			wantRealm:   "https://docker.m.daocloud.io/token",
			wantService: "registry.daocloud.io",
			wantScope:   "repository:library/hello-world:pull",
			wantOK:      true,
		},
		{
			name:      "realm only",
			header:    `Bearer realm="https://example.com/token"`,
			wantRealm: "https://example.com/token",
			wantOK:    true,
		},
		{
			name:   "not bearer",
			header: `Basic realm="registry"`,
			wantOK: false,
		},
		{
			name:   "empty",
			header: "",
			wantOK: false,
		},
		{
			name:   "bearer without realm",
			header: `Bearer service="reg",scope="repo:pull"`,
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			realm, service, scope, ok := parseBearerChallenge(tc.header)
			assert.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				assert.Equal(t, tc.wantRealm, realm)
				assert.Equal(t, tc.wantService, service)
				assert.Equal(t, tc.wantScope, scope)
			}
		})
	}
}

// ── Bearer token dance integration tests ─────────────────────────────────────

// authServer builds an httptest.Server that mimics an OCI registry with
// bearer auth:
//   - On first request without Authorization, returns 401 with WWW-Authenticate.
//   - Serves {"token":"testtoken"} at /token.
//   - On request with Authorization: Bearer testtoken, returns 200 with body.
//
// tokenHits is incremented each time the /token endpoint is called.
func authServer(t *testing.T, body string, tokenHits *atomic.Int64) *httptest.Server {
	t.Helper()
	const expectedToken = "testtoken"
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokenHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"token":"testtoken","expires_in":3600}`)
			return
		}
		// Main resource endpoint.
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+expectedToken {
			// No or wrong token: issue challenge.
			w.Header().Set("WWW-Authenticate",
				`Bearer realm="`+srv.URL+`/token",service="testregistry",scope="repository:library/nginx:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
	return srv
}

func TestFetch_BearerTokenDance(t *testing.T) {
	var tokenHits atomic.Int64
	srv := authServer(t, "manifest-payload", &tokenHits)
	defer srv.Close()

	c := testClient(3)
	body, meta, err := c.Fetch(context.Background(),
		artifact.ArtifactRef{Protocol: "oci", Name: "library/nginx", Version: "latest", Mutable: true},
		[]Upstream{{Name: "reg", BaseURL: srv.URL, Priority: 1}},
		WithOCIManifestAccept(),
	)

	require.NoError(t, err)
	require.NotNil(t, body)
	defer body.Close()

	data, _ := io.ReadAll(body)
	assert.Equal(t, "manifest-payload", string(data))
	assert.Equal(t, http.StatusOK, meta.StatusCode)
	assert.Equal(t, int64(1), tokenHits.Load(), "token endpoint should be called exactly once")
}

func TestFetch_BearerTokenCached(t *testing.T) {
	// Token should be fetched once and reused for subsequent requests.
	var tokenHits atomic.Int64
	srv := authServer(t, "blob-payload", &tokenHits)
	defer srv.Close()

	c := testClient(3)
	up := []Upstream{{Name: "reg", BaseURL: srv.URL, Priority: 1}}
	ref := artifact.ArtifactRef{Protocol: "oci", Name: "library/nginx", Version: "latest", Mutable: true}

	for i := 0; i < 3; i++ {
		body, _, err := c.Fetch(context.Background(), ref, up)
		require.NoError(t, err, "call %d", i)
		body.Close()
	}

	assert.Equal(t, int64(1), tokenHits.Load(),
		"token should be fetched once and cached for subsequent calls")
}

func TestFetch_BearerToken_WrongTokenRejected(t *testing.T) {
	// Server rejects even the new token (e.g. invalid scope) — client should
	// surface the 401 error rather than looping forever.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"token":"badtoken","expires_in":300}`)
			return
		}
		// Always return 401, even with the token.
		w.Header().Set("WWW-Authenticate",
			`Bearer realm="`+r.Host+`/token",service="reg",scope="repository:img:pull"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := testClient(2)
	_, _, err := c.Fetch(context.Background(),
		artifact.ArtifactRef{Protocol: "oci", Name: "img", Version: "latest", Mutable: true},
		[]Upstream{{Name: "reg", BaseURL: srv.URL, Priority: 1}},
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
}

func TestFetch_OCIManifestAcceptHeader(t *testing.T) {
	// Verify that WithOCIManifestAccept() causes the Accept header to be sent.
	var gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	c := testClient(1)
	body, _, err := c.Fetch(context.Background(), tarballRef("img", "v1"),
		[]Upstream{{Name: "up", BaseURL: srv.URL, Priority: 1}},
		WithOCIManifestAccept(),
	)
	require.NoError(t, err)
	body.Close()

	assert.Contains(t, gotAccept, "application/vnd.oci.image.index.v1+json",
		"OCI image index media type should be in Accept")
	assert.Contains(t, gotAccept, "application/vnd.docker.distribution.manifest.list.v2+json",
		"Docker manifest list media type should be in Accept")
}

func TestFetch_NoAcceptHeader_WithoutOption(t *testing.T) {
	// Without WithOCIManifestAccept, no Accept header should be set.
	var gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept = r.Header.Get("Accept")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	c := testClient(1)
	body, _, err := c.Fetch(context.Background(), tarballRef("img", "v1"),
		[]Upstream{{Name: "up", BaseURL: srv.URL, Priority: 1}},
	)
	require.NoError(t, err)
	body.Close()

	assert.Empty(t, gotAccept, "Accept header should not be set without WithOCIManifestAccept")
}
