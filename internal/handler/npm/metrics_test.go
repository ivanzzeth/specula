package npm

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/metrics"
)

// ── cache-outcome metrics: npm ───────────────────────────────────────────────
//
// These tests assert against the REAL prometheus counters in internal/metrics,
// driven through the REAL metrics.Middleware over a REAL httptest.Server. The
// increments must come from the handler's own MarkHit/MarkMiss calls: nothing in
// this file touches a counter, so a handler that marks nothing fails here.
//
// Counters are process-global and cumulative, so every assertion is a
// before/after DELTA rather than an absolute value.

// outcomeDelta snapshots the hit/miss counters for protocol and returns a
// function reporting how far each has moved since the snapshot.
func outcomeDelta(protocol string) func() (hits, misses float64) {
	hits0 := testutil.ToFloat64(metrics.CacheHitsTotal.WithLabelValues(protocol))
	misses0 := testutil.ToFloat64(metrics.CacheMissesTotal.WithLabelValues(protocol))
	return func() (float64, float64) {
		return testutil.ToFloat64(metrics.CacheHitsTotal.WithLabelValues(protocol)) - hits0,
			testutil.ToFloat64(metrics.CacheMissesTotal.WithLabelValues(protocol)) - misses0
	}
}

// npmMetricsServer mounts h under the real metrics middleware on a real HTTP
// server, so the request-scope plumbing is exercised end to end.
func npmMetricsServer(t *testing.T, h *Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(metrics.Middleware("npm", h))
	t.Cleanup(srv.Close)
	return srv
}

// get performs a real GET and drains the body (the handler streams from cache;
// not draining would leave the outcome recorded but the transfer incomplete).
func get(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp
}

// TestNpmCacheOutcome_ColdPackumentIsMiss_WarmIsHit pins both halves of the
// definition on one warming cache: the first request must fetch the body from
// the upstream (miss), and the second must serve it from cache without any
// upstream body transfer (hit).
func TestNpmCacheOutcome_ColdPackumentIsMiss_WarmIsHit(t *testing.T) {
	packument := []byte(`{"name":"express","versions":{}}`)
	up, packumentHits, _ := fakeNpmRegistry(t,
		map[string][]byte{"express": packument}, nil)

	cm := newNpmTestCache()
	srv := npmMetricsServer(t, newNpmHandlerWithUpstream(cm, up.URL))

	// Cold: nothing in cache → body must come from the upstream.
	delta := outcomeDelta("npm")
	resp := get(t, srv.URL+"/express")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	hits, misses := delta()
	assert.Equal(t, float64(0), hits, "a cold request must not report a hit")
	assert.Equal(t, float64(1), misses, "a cold request must report exactly one miss")
	require.EqualValues(t, 1, *packumentHits, "cold request must have fetched upstream")

	// Warm: the packument is now cached → no upstream body transfer.
	delta = outcomeDelta("npm")
	resp = get(t, srv.URL+"/express")
	require.Equal(t, http.StatusOK, resp.StatusCode)

	hits, misses = delta()
	assert.Equal(t, float64(1), hits, "a warm request must report exactly one hit")
	assert.Equal(t, float64(0), misses, "a warm request must not report a miss")
	assert.EqualValues(t, 1, *packumentHits,
		"a warm request must not have touched the upstream at all")
}

// TestNpmCacheOutcome_ColdTarballIsMiss_WarmIsHit covers the immutable CAS tier,
// which is a separate decision point from the mutable packument pipeline.
func TestNpmCacheOutcome_ColdTarballIsMiss_WarmIsHit(t *testing.T) {
	tgz := []byte("fake tarball bytes")
	up, _, _ := fakeNpmRegistry(t, nil,
		map[string][]byte{"express-4.18.2.tgz": tgz})

	cm := newNpmTestCache()
	srv := npmMetricsServer(t, newNpmHandlerWithUpstream(cm, up.URL))

	delta := outcomeDelta("npm")
	resp := get(t, srv.URL+"/express/-/express-4.18.2.tgz")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	hits, misses := delta()
	assert.Equal(t, float64(0), hits)
	assert.Equal(t, float64(1), misses, "a cold tarball must report one miss")

	delta = outcomeDelta("npm")
	resp = get(t, srv.URL+"/express/-/express-4.18.2.tgz")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	hits, misses = delta()
	assert.Equal(t, float64(1), hits, "a warm tarball must report one hit")
	assert.Equal(t, float64(0), misses)
}

// TestNpmCacheOutcome_ServeStaleOnUpstreamFailureIsHit pins the H1 resolution:
// the upstream is down, the body is served from a stale cache entry, and the
// bytes therefore came from cache — a hit, not a miss.
func TestNpmCacheOutcome_ServeStaleOnUpstreamFailureIsHit(t *testing.T) {
	stale := []byte(`{"name":"express","versions":{}}`)
	ref := packumentRef("express")
	cm := newNpmTestCache()
	cm.seedStale(ref, stale)

	// An upstream that always fails: the handler must fall back to stale bytes.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream down", http.StatusInternalServerError)
	}))
	t.Cleanup(dead.Close)

	srv := npmMetricsServer(t, newNpmHandlerWithUpstream(cm, dead.URL,
		WithFailClosed(false)))

	delta := outcomeDelta("npm")
	resp := get(t, srv.URL+"/express")
	require.Equal(t, http.StatusOK, resp.StatusCode, "stale bytes must still be served")

	hits, misses := delta()
	assert.Equal(t, float64(1), hits,
		"serve-stale on upstream failure is a HIT: the body came from cache")
	assert.Equal(t, float64(0), misses)
}

// TestNpmCacheOutcome_RequestsThatNeverConsultCacheMarkNothing guards the
// denominator. The hit/miss denominator is "requests that consulted the cache";
// a rejected method or a malformed path never asks the cache anything, so
// counting either would poison the ratio.
func TestNpmCacheOutcome_RequestsThatNeverConsultCacheMarkNothing(t *testing.T) {
	cm := newNpmTestCache()
	srv := npmMetricsServer(t, newNpmHandlerWithUpstream(cm, "http://127.0.0.1:1"))

	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
	}{
		{"405_wrong_method", http.MethodPost, "/express", http.StatusMethodNotAllowed},
		{"404_invalid_package_name", http.MethodGet, "/@scope", http.StatusNotFound},
		{"404_empty_path", http.MethodGet, "/", http.StatusNotFound},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			delta := outcomeDelta("npm")

			req, err := http.NewRequest(tc.method, srv.URL+tc.path, nil)
			require.NoError(t, err)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			_, _ = io.ReadAll(resp.Body)
			require.Equal(t, tc.wantStatus, resp.StatusCode)

			hits, misses := delta()
			assert.Equal(t, float64(0), hits,
				"a request that never consulted the cache must not move hits")
			assert.Equal(t, float64(0), misses,
				"a request that never consulted the cache must not move misses")
		})
	}
}
