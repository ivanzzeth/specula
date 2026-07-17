package git

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/metrics"
)

// ── cache-outcome metrics: git ───────────────────────────────────────────────
//
// git has no CacheManager: its byte cache is the bare mirror on disk. The honest
// hit/miss axis is therefore whether the packfile git http-backend builds came
// from a mirror that was already present (no upstream contact — a HIT) or from
// one this request had to clone/fetch from the upstream (a MISS). These tests
// pin exactly that, against the REAL prometheus counters, through the REAL
// middleware, driven by real HTTP against a real upstream git Smart HTTP server.

// gitOutcomeDelta snapshots the git hit/miss counters and reports movement.
func gitOutcomeDelta() func() (hits, misses float64) {
	hits0 := testutil.ToFloat64(metrics.CacheHitsTotal.WithLabelValues("git"))
	misses0 := testutil.ToFloat64(metrics.CacheMissesTotal.WithLabelValues("git"))
	return func() (float64, float64) {
		return testutil.ToFloat64(metrics.CacheHitsTotal.WithLabelValues("git")) - hits0,
			testutil.ToFloat64(metrics.CacheMissesTotal.WithLabelValues("git")) - misses0
	}
}

// gitMetricsFixture mirrors newGitProxyFixture but mounts the handler under the
// real metrics middleware, so the request-scope plumbing is exercised end to end.
func gitMetricsFixture(t *testing.T, staleAfter time.Duration) *gitProxyFixture {
	t.Helper()

	work := t.TempDir()
	gitCmd(t, work, "init", "--quiet", "--initial-branch=master", work)
	require.NoError(t, os.WriteFile(filepath.Join(work, "README"), []byte("v1\n"), 0o644))
	gitCmd(t, work, "add", "README")
	gitCmd(t, work, "commit", "--quiet", "-m", "initial")

	const project = "octocat/Hello-World"
	upRoot := t.TempDir()
	bare := newBareUpstream(t, upRoot, project, work)
	upSrv := newUpstreamGitServer(t, upRoot)

	host := strings.TrimPrefix(upSrv.URL, "http://")
	mirrorDir := t.TempDir()
	ms := newMemMeta()

	h := NewHandler(
		WithMirrorDir(mirrorDir),
		WithAllowedUpstreams([]string{host}),
		WithPublicOnly(false), // the visibility probe only knows gitee/github APIs
		WithUpstreamScheme("http"),
		WithMeta(ms),
		WithSyncStaleAfter(staleAfter),
	)
	proxy := httptest.NewServer(metrics.Middleware("git", h))
	t.Cleanup(proxy.Close)

	return &gitProxyFixture{
		proxy: proxy, mirrorDir: mirrorDir, meta: ms,
		work: work, bare: bare, project: project, host: host,
	}
}

func gitDo(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp
}

// TestGitCacheOutcome_ColdCloneIsMiss_WarmIsHit pins git's hit/miss seam. With a
// long staleAfter the second ref-advertise must be answered from the mirror
// already on disk, with no clone or fetch — a hit.
func TestGitCacheOutcome_ColdCloneIsMiss_WarmIsHit(t *testing.T) {
	f := gitMetricsFixture(t, time.Minute)
	url := f.cloneURL() + "/info/refs?service=git-upload-pack"

	// Cold: no mirror on disk → the handler must clone from the upstream.
	delta := gitOutcomeDelta()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	resp := gitDo(t, req)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	hits, misses := delta()
	assert.Equal(t, float64(0), hits, "a cold clone must not report a hit")
	assert.Equal(t, float64(1), misses,
		"a cold request had to fetch the repo from the upstream: a miss")
	require.DirExists(t, filepath.Join(f.mirrorDir, f.host, f.project+gitSuffix),
		"the cold request must have produced a mirror")

	// Warm: the mirror is present and fresh → served entirely from disk.
	delta = gitOutcomeDelta()
	req, err = http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	resp = gitDo(t, req)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	hits, misses = delta()
	assert.Equal(t, float64(1), hits,
		"a warm request served from the existing mirror is a hit")
	assert.Equal(t, float64(0), misses, "a warm request must not report a miss")
}

// TestGitCacheOutcome_StaleMirrorRefetchIsMiss pins the other side of the seam:
// with staleAfter=0 every request re-fetches from the upstream, so no request
// may claim a hit. This is the conservative direction — a fetch that happens to
// transfer no new objects is still counted a miss — and it is the direction that
// never invents a hit.
func TestGitCacheOutcome_StaleMirrorRefetchIsMiss(t *testing.T) {
	f := gitMetricsFixture(t, 0) // staleAfter=0 → always re-fetch
	url := f.cloneURL() + "/info/refs?service=git-upload-pack"

	// Warm the mirror.
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, gitDo(t, req).StatusCode)

	// Second request: mirror exists but is stale → upstream fetch → miss.
	delta := gitOutcomeDelta()
	req, err = http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	resp := gitDo(t, req)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	hits, misses := delta()
	assert.Equal(t, float64(0), hits,
		"a request that re-fetched from the upstream must not claim a hit")
	assert.Equal(t, float64(1), misses, "a stale-mirror refresh is a miss")
}

// TestGitCacheOutcome_RequestsThatNeverConsultMirrorMarkNothing guards the
// denominator. A disallowed host is rejected before the mirror is touched, and
// an Authorization-bearing request is passed through with zero caching without
// ever consulting the mirror — neither may move either counter.
func TestGitCacheOutcome_RequestsThatNeverConsultMirrorMarkNothing(t *testing.T) {
	f := gitMetricsFixture(t, time.Minute)

	t.Run("404_disallowed_host", func(t *testing.T) {
		delta := gitOutcomeDelta()
		req, err := http.NewRequest(http.MethodGet,
			f.proxy.URL+"/evil.example.com/some/repo.git/info/refs", nil)
		require.NoError(t, err)
		resp := gitDo(t, req)
		require.Equal(t, http.StatusNotFound, resp.StatusCode)

		hits, misses := delta()
		assert.Equal(t, float64(0), hits, "a rejected host never consulted the mirror")
		assert.Equal(t, float64(0), misses, "a rejected host never consulted the mirror")
	})

	t.Run("404_malformed_path", func(t *testing.T) {
		delta := gitOutcomeDelta()
		req, err := http.NewRequest(http.MethodGet, f.proxy.URL+"/"+f.host, nil)
		require.NoError(t, err)
		resp := gitDo(t, req)
		require.Equal(t, http.StatusNotFound, resp.StatusCode)

		hits, misses := delta()
		assert.Equal(t, float64(0), hits, "a malformed path never consulted the mirror")
		assert.Equal(t, float64(0), misses, "a malformed path never consulted the mirror")
	})

	t.Run("authenticated_request_is_passthrough", func(t *testing.T) {
		delta := gitOutcomeDelta()
		req, err := http.NewRequest(http.MethodGet,
			f.cloneURL()+"/info/refs?service=git-upload-pack", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
		gitDo(t, req) // status is the upstream's business; the outcome is ours

		hits, misses := delta()
		assert.Equal(t, float64(0), hits,
			"an authenticated request is passed through and never consults the mirror")
		assert.Equal(t, float64(0), misses,
			"an authenticated request is passed through and never consults the mirror")
	})
}
