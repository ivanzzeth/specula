//go:build integration

// Package e2e — behavioural coverage for the Prometheus metrics PRD §7 promises.
//
// These tests drive the REAL Specula stack (LocalDiskDriver + SQLiteStore +
// verify.Chain + cache.New + the real protocol handler) through real HTTP, then
// scrape the process Prometheus registry and assert that what /metrics reports
// matches what actually happened.
//
// The load-bearing assertion is on specula_verification_total{protocol,check,
// tier,result}: its tier label must equal the tier the verify chain actually
// recorded — cross-checked against the tier persisted in the metadata store for
// the same traffic. An unmeasured claim is an unproven claim (PRD §G2).
package e2e

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/metrics"
	"github.com/ivanzzeth/specula/internal/stats"
)

// prdSection7Metrics is the exact metric list PRD §7 promises. This slice is the
// contract: it is transcribed from the doc, not from the code.
var prdSection7Metrics = []string{
	"specula_requests_total",
	"specula_cache_hits_total",
	"specula_cache_misses_total",
	"specula_cache_bytes",
	"specula_cache_objects",
	"specula_upstream_latency_seconds",
	"specula_verification_total",
	"specula_upstream_blocked",
}

// gatheredNames returns the set of metric family names currently exposed by the
// process's default Prometheus registry — i.e. exactly what a `curl /metrics`
// against a fresh headless process would report.
func gatheredNames(t *testing.T) map[string]bool {
	t.Helper()
	fams, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	names := make(map[string]bool, len(fams))
	for _, f := range fams {
		names[f.GetName()] = true
	}
	return names
}

// TestPRDSection7MetricsExist reproduces the reported gap: after real traffic
// through the real stack, wired the way production wires it, /metrics must
// expose every metric PRD §7 promises.
//
// "The way production wires it" is load-bearing. cmd/specula wraps every
// protocol mount in metrics.Middleware; a test that drove the bare handler would
// silently never record specula_requests_total and would then "prove" a metric
// that production emits only because of wiring this test omitted.
func TestPRDSection7MetricsExist(t *testing.T) {
	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	// Production wiring #1: the stats collector registers cache_bytes/objects.
	collector := stats.NewCollectorWithStore(s.metaStore)

	// Real traffic through the real npm handler against a fake upstream ...
	packument := []byte(`{"name":"lodash","versions":{}}`)
	fake, _ := newFakeNpmRegistry(t,
		map[string][]byte{"lodash": packument},
		map[string][]byte{},
	)
	inner := newNpmServer(t, s, fake.URL, 60)

	// Production wiring #2: mounted behind metrics.Middleware, as main.go does.
	proxy := httptest.NewServer(metrics.Middleware("npm", http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			req, err := http.NewRequestWithContext(r.Context(), r.Method, inner.URL+r.URL.Path, nil)
			require.NoError(t, err)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, resp.Body)
		})))
	t.Cleanup(proxy.Close)

	resp, err := http.Get(proxy.URL + "/lodash")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Sanity: the traffic really did reach the verify chain and persist a tier.
	entries, err := s.metaStore.CacheSizeByProtocol(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, entries, "traffic must have populated the metadata store")

	// Production wiring #3: main.go runs `go collector.Run(ctx)`, whose refresh
	// tick is what SETS the cache_bytes/objects gauges. Until a refresh happens
	// those two Vecs have no child series and therefore contribute NO family to
	// /metrics at all — they are registered but invisible.
	//
	// This is not a quirk of the test: on a real, fresh, headless process
	// specula_cache_bytes is genuinely absent from /metrics for the first 30s
	// (the default RefreshInterval), verified by hand against a live daemon —
	// absent at t+5s, present at t+40s. ByProtocol performs the same gauge sync
	// the tick does, so calling it here stands in for the first tick rather than
	// making the test sleep 30 seconds.
	_, err = collector.ByProtocol(context.Background())
	require.NoError(t, err)

	names := gatheredNames(t)
	var missing []string
	for _, m := range prdSection7Metrics {
		if !names[m] {
			missing = append(missing, m)
		}
	}
	require.Empty(t, missing, "PRD §7 promises these metrics but /metrics does not expose them: %v", missing)
}

// TestVerificationMetricMatchesPersistedTier is the in-process half of the
// real-traffic cross-check: for the SAME traffic, the tier on
// specula_verification_total{check="chain"} must equal the tier persisted to
// cache_entries. If these can disagree, one of them is lying and neither is
// trustworthy.
func TestVerificationMetricMatchesPersistedTier(t *testing.T) {
	tmp := t.TempDir()
	s := newSpeculaStack(t, tmp)

	packument := []byte(`{"name":"cross-check-pkg","versions":{}}`)
	fake, _ := newFakeNpmRegistry(t,
		map[string][]byte{"cross-check-pkg": packument},
		map[string][]byte{},
	)
	srv := newNpmServer(t, s, fake.URL, 60)

	resp, err := http.Get(srv.URL + "/cross-check-pkg")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// What the DB recorded for this traffic.
	entry, err := s.metaStore.Get(context.Background(), artifact.ArtifactRef{
		Protocol: "npm", Name: "cross-check-pkg", Version: "packument", Mutable: true,
	})
	require.NoError(t, err)
	require.NotNil(t, entry, "traffic must have persisted a cache entry")
	dbTier := entry.Tier.String()

	// What the metric reports for the same traffic. The chain series must carry
	// a non-zero count at exactly the DB's tier.
	found := false
	for _, status := range []string{"pass", "warn"} {
		v := testutil.ToFloat64(metrics.VerificationTotal.WithLabelValues("npm", metrics.CheckChain, dbTier, status))
		if v > 0 {
			found = true
		}
	}
	require.True(t, found,
		"DB persisted tier=%q for this traffic but specula_verification_total{check=\"chain\",tier=%q} never counted it — metric and DB disagree",
		dbTier, dbTier)
}
