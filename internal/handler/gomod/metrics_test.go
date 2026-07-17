package gomod

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

// ── cache-outcome metrics: gomod ─────────────────────────────────────────────
//
// As in the npm suite, these assert the REAL prometheus counters through the
// REAL metrics.Middleware over a REAL httptest.Server. Nothing here increments a
// counter itself — every delta observed must originate in the handler's own
// MarkHit/MarkMiss calls.

// outcomeDelta snapshots the hit/miss counters for protocol and returns a
// function reporting how far each has moved since the snapshot. Counters are
// process-global and cumulative, so deltas are the only safe assertion.
func outcomeDelta(protocol string) func() (hits, misses float64) {
	hits0 := testutil.ToFloat64(metrics.CacheHitsTotal.WithLabelValues(protocol))
	misses0 := testutil.ToFloat64(metrics.CacheMissesTotal.WithLabelValues(protocol))
	return func() (float64, float64) {
		return testutil.ToFloat64(metrics.CacheHitsTotal.WithLabelValues(protocol)) - hits0,
			testutil.ToFloat64(metrics.CacheMissesTotal.WithLabelValues(protocol)) - misses0
	}
}

func gomodMetricsServer(t *testing.T, h *Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(metrics.Middleware("gomod", h))
	t.Cleanup(srv.Close)
	return srv
}

func gomodGet(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	_, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp
}

// TestGomodCacheOutcome_ColdZipIsMiss_WarmIsHit exercises the immutable CAS tier
// (.zip): cold must fetch the body from the upstream, warm must not.
func TestGomodCacheOutcome_ColdZipIsMiss_WarmIsHit(t *testing.T) {
	const module = "github.com/foo/bar"
	zipBody := []byte("PK\x03\x04 fake module zip")
	up := goproxyUpstream(t, module, nil,
		map[string][]byte{"v1.0.0.zip": zipBody}, nil)

	cm := newGomodTestCache()
	srv := gomodMetricsServer(t, newHandlerWithUpstream(cm, up.URL))
	url := srv.URL + "/" + module + "/@v/v1.0.0.zip"

	delta := outcomeDelta("gomod")
	resp := gomodGet(t, url)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	hits, misses := delta()
	assert.Equal(t, float64(0), hits, "a cold request must not report a hit")
	assert.Equal(t, float64(1), misses, "a cold request must report exactly one miss")

	delta = outcomeDelta("gomod")
	resp = gomodGet(t, url)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	hits, misses = delta()
	assert.Equal(t, float64(1), hits, "a warm request must report exactly one hit")
	assert.Equal(t, float64(0), misses, "a warm request must not report a miss")
}

// TestGomodCacheOutcome_ColdListIsMiss_WarmIsHit exercises the mutable tier
// (@v/list), a different decision point from the immutable pipeline above.
func TestGomodCacheOutcome_ColdListIsMiss_WarmIsHit(t *testing.T) {
	const module = "github.com/foo/bar"
	listBody := []byte("v1.0.0\nv1.1.0\n")
	up := goproxyUpstream(t, module, listBody, nil, nil)

	cm := newGomodTestCache()
	srv := gomodMetricsServer(t, newHandlerWithUpstream(cm, up.URL))
	url := srv.URL + "/" + module + "/@v/list"

	delta := outcomeDelta("gomod")
	resp := gomodGet(t, url)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	hits, misses := delta()
	assert.Equal(t, float64(0), hits)
	assert.Equal(t, float64(1), misses, "a cold @v/list must report one miss")

	delta = outcomeDelta("gomod")
	resp = gomodGet(t, url)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	hits, misses = delta()
	assert.Equal(t, float64(1), hits, "a warm @v/list must report one hit")
	assert.Equal(t, float64(0), misses)
}

// TestGomodCacheOutcome_RequestsThatNeverConsultCacheMarkNothing guards the
// denominator: a rejected method and a malformed @v file name are answered
// before any cache lookup, so neither counter may move.
func TestGomodCacheOutcome_RequestsThatNeverConsultCacheMarkNothing(t *testing.T) {
	const module = "github.com/foo/bar"
	up := goproxyUpstream(t, module, nil, nil, nil)
	cm := newGomodTestCache()
	srv := gomodMetricsServer(t, newHandlerWithUpstream(cm, up.URL))

	tests := []struct {
		name       string
		method     string
		path       string
		wantStatus int
	}{
		{"405_wrong_method", http.MethodPost, "/" + module + "/@v/list", http.StatusMethodNotAllowed},
		{"404_unknown_at_v_file", http.MethodGet, "/" + module + "/@v/v1.0.0.bogus", http.StatusNotFound},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			delta := outcomeDelta("gomod")

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
