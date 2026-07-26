//go:build integration

package e2e

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	helmhandler "github.com/ivanzzeth/specula/internal/handler/helm"
	"github.com/ivanzzeth/specula/internal/upstream"
)

// TestHelmFirstUpstreamDownFallsBack: dead primary → secondary serves chart.
func TestHelmFirstUpstreamDownFallsBack(t *testing.T) {
	const (
		chartName    = "failover-chart"
		chartVersion = "1.0.0"
		repo         = "stable"
	)
	chartFile := fmt.Sprintf("%s-%s.tgz", chartName, chartVersion)
	chartBytes := buildChartTGZ(t, chartName, chartVersion)

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		file := r.URL.Path
		if i := lastSlash(file); i >= 0 {
			file = file[i+1:]
		}
		if file == chartFile {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(chartBytes)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(good.Close)

	tmp := t.TempDir()
	s := newHelmStack(t, tmp, "")
	speculaSrv := newHelmServer(t, s,
		helmhandler.WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "dead", BaseURL: deadURL(t), Priority: 1},
			{Name: "good", BaseURL: good.URL, Priority: 2},
		}),
	)

	resp, err := http.Get(fmt.Sprintf("%s/%s/%s", speculaSrv.URL, repo, chartFile))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, chartBytes, got)
}

// TestHelmRateLimit429Retries: upstream 429 then serves — failsafe Retry recovers.
func TestHelmRateLimit429Retries(t *testing.T) {
	const (
		chartName    = "ratelimit-chart"
		chartVersion = "1.0.0"
		repo         = "stable"
	)
	chartFile := fmt.Sprintf("%s-%s.tgz", chartName, chartVersion)
	chartBytes := buildChartTGZ(t, chartName, chartVersion)
	var hits atomic.Int64

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		file := r.URL.Path
		if i := lastSlash(file); i >= 0 {
			file = file[i+1:]
		}
		if file != chartFile {
			http.NotFound(w, r)
			return
		}
		n := hits.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(chartBytes)
	}))
	t.Cleanup(up.Close)

	tmp := t.TempDir()
	s := newHelmStack(t, tmp, "")
	speculaSrv := newHelmServer(t, s,
		helmhandler.WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "limited", BaseURL: up.URL, Priority: 1},
		}),
	)

	resp, err := http.Get(fmt.Sprintf("%s/%s/%s", speculaSrv.URL, repo, chartFile))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.GreaterOrEqual(t, hits.Load(), int64(3))
}

// TestHelm5xxExhaustsThenFallsBack: primary always 500 → secondary wins.
func TestHelm5xxExhaustsThenFallsBack(t *testing.T) {
	const (
		chartName    = "retry-chart"
		chartVersion = "2.0.0"
		repo         = "stable"
	)
	chartFile := fmt.Sprintf("%s-%s.tgz", chartName, chartVersion)
	chartBytes := buildChartTGZ(t, chartName, chartVersion)

	bad, badHits := countingStatusUpstream(t, http.StatusInternalServerError)
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		file := r.URL.Path
		if i := lastSlash(file); i >= 0 {
			file = file[i+1:]
		}
		if file == chartFile {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(chartBytes)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(good.Close)

	tmp := t.TempDir()
	s := newHelmStack(t, tmp, "")
	speculaSrv := newHelmServer(t, s,
		helmhandler.WithUpstream(upstream.NewClient(), []upstream.Upstream{
			{Name: "bad", BaseURL: bad.URL, Priority: 1},
			{Name: "good", BaseURL: good.URL, Priority: 2},
		}),
	)

	resp, err := http.Get(fmt.Sprintf("%s/%s/%s", speculaSrv.URL, repo, chartFile))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.GreaterOrEqual(t, badHits.Load(), int64(1))
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}
