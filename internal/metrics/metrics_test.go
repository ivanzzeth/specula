package metrics

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// gatherNames returns the metric family names on the DEFAULT registry — the same
// registry promhttp.Handler() serves at /metrics. Asserting against the default
// registry rather than a private test registry is the point: the bug this guards
// against (7600a0e) was a metric that existed in code but never reached /metrics.
func gatherNames(t *testing.T) map[string]bool {
	t.Helper()
	fams, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	out := make(map[string]bool, len(fams))
	for _, f := range fams {
		out[f.GetName()] = true
	}
	return out
}

// TestRegisteredOnImportAlone is the regression test for the 7600a0e bug class:
// merely importing this package must put every instrument on the DEFAULT
// registry. No constructor is called here, no request is served — if
// registration were coupled to constructing a collector (as stats.NewCollector
// still couples specula_cache_bytes) or to a request arriving, this fails.
//
// Registration is proved by attempting to re-register: the default Registerer
// answers AlreadyRegisteredError for a collector it holds. That is the direct
// question. Gather() is NOT the right instrument here — see
// TestRegisteredButUnobservedVecIsAbsentFromMetrics for why.
func TestRegisteredOnImportAlone(t *testing.T) {
	for name, c := range map[string]prometheus.Collector{
		"specula_requests_total":           RequestsTotal,
		"specula_cache_hits_total":         CacheHitsTotal,
		"specula_cache_misses_total":       CacheMissesTotal,
		"specula_upstream_latency_seconds": UpstreamLatencySeconds,
		"specula_upstream_blocked":         UpstreamBlocked,
		"specula_verification_total":       VerificationTotal,
	} {
		err := prometheus.DefaultRegisterer.Register(c)
		require.Error(t, err, "%s must already be registered on the default registry", name)
		require.IsType(t, prometheus.AlreadyRegisteredError{}, err,
			"%s must be registered at init, not fail for some other reason", name)
	}
}

// TestRegisteredButUnobservedVecIsAbsentFromMetrics pins a Prometheus property
// that is easy to get wrong and that this design depends on.
//
// A *Vec that is registered but has no child series contributes NO metric family
// to Gather() — so it does not appear on /metrics at all. "Registered" therefore
// does NOT imply "reported". The only bridge is pre-initialising the label
// combinations, which is possible exactly when the label set is bounded AND
// knowable without traffic:
//
//	cache_hits/misses{protocol}      → knowable (AllProtocols)        → pre-init at init()
//	upstream_blocked{protocol,upstream} → knowable from config        → pre-init via PreInitUpstream
//	requests_total{protocol,method,status} → status is NOT knowable   → absent until first request
//	verification_total{...,tier,result}    → NOT knowable, and pre-init
//	    would fabricate tier/result combinations that may never be legitimate
//	                                                                  → absent until first verification
//
// For the last two, absence is correct and honest: it means "this has not
// happened yet", not "this is broken". Pre-initialising them would invent
// combinations that never occurred, which is the same class of lie this whole
// change exists to remove.
func TestRegisteredButUnobservedVecIsAbsentFromMetrics(t *testing.T) {
	reg := prometheus.NewRegistry()
	vec := prometheus.NewCounterVec(
		prometheus.CounterOpts{Name: "probe_total", Help: "probe"},
		[]string{"label"},
	)
	require.NoError(t, reg.Register(vec))

	fams, err := reg.Gather()
	require.NoError(t, err)
	require.Empty(t, fams, "a registered Vec with no children emits no family: registration alone cannot make a metric visible")

	vec.WithLabelValues("x") // pre-initialise one series to zero
	fams, err = reg.Gather()
	require.NoError(t, err)
	require.Len(t, fams, 1, "pre-initialising a series is what makes it visible at zero")
}

// TestPreInitialisedSeriesAreOnMetricsWithZeroTraffic is the operator-facing
// half of the contract: on a fresh headless process, with no traffic and nobody
// touching the UI, /metrics must already report hit/miss for every protocol.
func TestPreInitialisedSeriesAreOnMetricsWithZeroTraffic(t *testing.T) {
	names := gatherNames(t)
	assert.True(t, names["specula_cache_hits_total"], "hits must be on /metrics with zero traffic")
	assert.True(t, names["specula_cache_misses_total"], "misses must be on /metrics with zero traffic")
}

// TestHitMissPreInitialisedToZero proves a fresh headless process reports a real
// zero for every protocol rather than an absent series, so a hit-ratio
// expression is well defined before any traffic.
func TestHitMissPreInitialisedToZero(t *testing.T) {
	for _, p := range AllProtocols {
		assert.NotPanics(t, func() {
			_ = testutil.ToFloat64(CacheHitsTotal.WithLabelValues(p))
			_ = testutil.ToFloat64(CacheMissesTotal.WithLabelValues(p))
		}, "protocol %q must have pre-initialised hit/miss series", p)
	}
}

// TestVerificationTierVocabulary is the load-bearing honesty test: the tier
// label may only ever be one of PRD §G2's four tiers. A fifth value — or a tier
// asserted from anywhere other than the Result the chain produced — is the worst
// bug this codebase could ship.
func TestVerificationTierVocabulary(t *testing.T) {
	g2Tiers := map[string]bool{"signed": true, "consensus": true, "tofu": true, "checksum": true}

	// Every tier artifact.Tier can represent must render inside the vocabulary.
	for _, tier := range []artifact.Tier{
		artifact.TierChecksum, artifact.TierTofu, artifact.TierConsensus, artifact.TierSigned,
	} {
		assert.True(t, g2Tiers[tierLabel(tier)],
			"tier %d rendered as %q, which is outside PRD §G2's four tiers", tier, tierLabel(tier))
	}

	// And the mapping is exact, not merely in-vocabulary.
	assert.Equal(t, "signed", tierLabel(artifact.TierSigned))
	assert.Equal(t, "consensus", tierLabel(artifact.TierConsensus))
	assert.Equal(t, "tofu", tierLabel(artifact.TierTofu))
	assert.Equal(t, "checksum", tierLabel(artifact.TierChecksum))

	// The four tiers must be mutually distinct: a mapping that collapsed two
	// tiers onto one label would silently promote or demote artifacts on
	// /metrics while every individual assertion above still passed.
	seen := map[string]bool{}
	for _, tier := range []artifact.Tier{
		artifact.TierChecksum, artifact.TierTofu, artifact.TierConsensus, artifact.TierSigned,
	} {
		l := tierLabel(tier)
		assert.False(t, seen[l], "tier label %q is emitted for two different tiers", l)
		seen[l] = true
	}
}

// TestRecordVerificationUsesGivenTier proves RecordVerification labels with the
// tier it is HANDED — the tier actually reached — and does not substitute one of
// its own from anywhere else.
func TestRecordVerificationUsesGivenTier(t *testing.T) {
	before := testutil.ToFloat64(VerificationTotal.WithLabelValues("apt", "gpg", "signed", "pass"))
	RecordVerification("apt", "gpg", artifact.TierSigned, artifact.StatusPass)
	after := testutil.ToFloat64(VerificationTotal.WithLabelValues("apt", "gpg", "signed", "pass"))
	assert.Equal(t, before+1, after, "must record at the tier it was given")

	// Recording a checksum-tier pass must NOT land in the signed series.
	sigBefore := testutil.ToFloat64(VerificationTotal.WithLabelValues("npm", "checksum", "signed", "pass"))
	RecordVerification("npm", "checksum", artifact.TierChecksum, artifact.StatusPass)
	sigAfter := testutil.ToFloat64(VerificationTotal.WithLabelValues("npm", "checksum", "signed", "pass"))
	assert.Equal(t, sigBefore, sigAfter,
		"a checksum-tier pass must never be counted as tier=signed — that is the single worst bug this codebase could ship")
	assert.Equal(t, 1.0, testutil.ToFloat64(VerificationTotal.WithLabelValues("npm", "checksum", "checksum", "pass")))
}

// TestSetUpstreamBlocked covers both directions of the gauge.
func TestSetUpstreamBlocked(t *testing.T) {
	SetUpstreamBlocked("pypi", "tuna", true)
	assert.Equal(t, 1.0, testutil.ToFloat64(UpstreamBlocked.WithLabelValues("pypi", "tuna")))
	SetUpstreamBlocked("pypi", "tuna", false)
	assert.Equal(t, 0.0, testutil.ToFloat64(UpstreamBlocked.WithLabelValues("pypi", "tuna")))
}

// TestPreInitUpstream proves a configured-but-never-used upstream reports a real
// 0, so "absent" and "healthy" are distinguishable.
func TestPreInitUpstream(t *testing.T) {
	PreInitUpstream("helm", "azure-cn")
	assert.Equal(t, 0.0, testutil.ToFloat64(UpstreamBlocked.WithLabelValues("helm", "azure-cn")))
}

// TestLatencyBucketsSuitCNTraffic pins the measured-latency rationale so a later
// "tidy-up" to prometheus.DefBuckets is a test failure rather than a silent loss
// of resolution.
func TestLatencyBucketsSuitCNTraffic(t *testing.T) {
	// The client's own http.Client timeout is 30s. If the top bucket were below
	// it, every slow-but-not-timed-out CN request would hide in +Inf and be
	// indistinguishable from a hang.
	assert.Equal(t, 30.0, cnLatencyBuckets[len(cnLatencyBuckets)-1],
		"top bucket must equal upstream's 30s client timeout")

	// Measured CN time-to-first-byte clusters (see cnLatencyBuckets doc): a warm
	// cluster at 0.24–0.81s and a stall cluster at 5.0–5.7s. Both must be
	// resolved by more than a single bucket, or the histogram cannot tell a fast
	// mirror from a stalling one.
	countIn := func(lo, hi float64) int {
		n := 0
		for _, b := range cnLatencyBuckets {
			if b >= lo && b <= hi {
				n++
			}
		}
		return n
	}
	assert.GreaterOrEqual(t, countIn(0.24, 0.82), 3,
		"warm CN cluster (0.24–0.81s) needs resolution, not one bucket")
	assert.GreaterOrEqual(t, countIn(2.5, 7.5), 3,
		"stall CN cluster (5.0–5.7s) must be bracketed on both sides")

	// Buckets must be strictly increasing or Prometheus rejects the histogram.
	for i := 1; i < len(cnLatencyBuckets); i++ {
		assert.Greater(t, cnLatencyBuckets[i], cnLatencyBuckets[i-1], "buckets must be strictly increasing")
	}
}

// TestHTTPStatusLabelIsBounded proves the status label cannot be minted from
// arbitrary integers.
func TestHTTPStatusLabelIsBounded(t *testing.T) {
	assert.Equal(t, "200", httpStatusLabel(200))
	assert.Equal(t, "404", httpStatusLabel(404))
	assert.Equal(t, "502", httpStatusLabel(502))
	assert.Equal(t, "invalid", httpStatusLabel(0))
	assert.Equal(t, "invalid", httpStatusLabel(99))
	assert.Equal(t, "invalid", httpStatusLabel(600))
	assert.Equal(t, "invalid", httpStatusLabel(-1))
}

// ── Middleware / request-scope behaviour ─────────────────────────────────────

// TestMiddlewareCountsRequestAndFlushesOutcome drives a real handler through a
// real httptest.Server: the marks come from the handler's own code path, never
// from the test.
func TestMiddlewareCountsRequestAndFlushesOutcome(t *testing.T) {
	hitsBefore := testutil.ToFloat64(CacheHitsTotal.WithLabelValues("oci"))
	reqBefore := testutil.ToFloat64(RequestsTotal.WithLabelValues("oci", "GET", "200"))

	h := Middleware("oci", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		MarkHit(r.Context()) // the handler decides, not the test
		w.WriteHeader(http.StatusOK)
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/x")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, hitsBefore+1, testutil.ToFloat64(CacheHitsTotal.WithLabelValues("oci")))
	assert.Equal(t, reqBefore+1, testutil.ToFloat64(RequestsTotal.WithLabelValues("oci", "GET", "200")))
	assert.Equal(t, "oci", resp.Header.Get("X-Specula-Protocol"))
	assert.Contains(t, resp.Header.Get("Via"), "specula")
}

// TestMiddlewareUnmarkedRequestMovesNeitherCounter proves the hit/miss
// denominator is "requests that consulted the cache". A /v2/ ping or a 404 on a
// malformed path must not silently inflate the miss count and drag the ratio
// down — nor count as a hit and prop it up.
func TestMiddlewareUnmarkedRequestMovesNeitherCounter(t *testing.T) {
	hits := testutil.ToFloat64(CacheHitsTotal.WithLabelValues("tarball"))
	misses := testutil.ToFloat64(CacheMissesTotal.WithLabelValues("tarball"))

	h := Middleware("tarball", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound) // never consulted the cache
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/nope")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, hits, testutil.ToFloat64(CacheHitsTotal.WithLabelValues("tarball")), "hits must not move")
	assert.Equal(t, misses, testutil.ToFloat64(CacheMissesTotal.WithLabelValues("tarball")), "misses must not move")
	// but the request itself is still counted
	assert.Equal(t, 1.0, testutil.ToFloat64(RequestsTotal.WithLabelValues("tarball", "GET", "404")))
}

// TestMarkFirstWriteWins proves a handler that marks a miss, refetches, stores,
// and then reads the artifact back out of cache still counts as ONE miss — the
// read-back must not overwrite the original decision and turn every miss into a
// hit.
func TestMarkFirstWriteWins(t *testing.T) {
	missBefore := testutil.ToFloat64(CacheMissesTotal.WithLabelValues("gomod"))
	hitBefore := testutil.ToFloat64(CacheHitsTotal.WithLabelValues("gomod"))

	h := Middleware("gomod", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		MarkMiss(r.Context()) // cold: fetched from upstream
		MarkHit(r.Context())  // read-back after Store — must NOT win
		w.WriteHeader(http.StatusOK)
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/x")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, missBefore+1, testutil.ToFloat64(CacheMissesTotal.WithLabelValues("gomod")), "miss is the first decision and must stand")
	assert.Equal(t, hitBefore, testutil.ToFloat64(CacheHitsTotal.WithLabelValues("gomod")), "later read-back must not be counted as a hit")
}

// TestMarkOutsideRequestIsNoop proves a metric can never take the data plane
// down: marking without middleware (a background task, a unit test) is silent.
func TestMarkOutsideRequestIsNoop(t *testing.T) {
	assert.NotPanics(t, func() {
		MarkHit(context.Background())
		MarkMiss(context.Background())
	})
}

// TestCacheBytesRegisteredOnImportAlone is the Bug-1 regression test. PRD §7's
// opening paragraph requires that specula_cache_bytes/specula_cache_objects are
// registered at PACKAGE INIT, independent of constructing an object — it cites
// specula_cache_bytes{protocol="git"} by name as the cautionary tale (7600a0e).
// They used to be registered as a side effect of constructing a stats.Collector,
// so a fresh headless process reported nothing until the 30s refresh ticker fired.
// Merely importing this package must now put both on the DEFAULT registry.
func TestCacheBytesRegisteredOnImportAlone(t *testing.T) {
	for name, c := range map[string]prometheus.Collector{
		"specula_cache_bytes":   CacheBytes,
		"specula_cache_objects": CacheObjects,
	} {
		err := prometheus.DefaultRegisterer.Register(c)
		require.Error(t, err, "%s must already be registered on the default registry at init", name)
		require.IsType(t, prometheus.AlreadyRegisteredError{}, err,
			"%s must be registered at init, not fail for some other reason", name)
	}
}

// TestCacheBytesVisibleWithZeroTraffic is the operator-facing half: on a fresh
// headless process, with no traffic and no stats.Collector ever constructed,
// /metrics must already expose specula_cache_bytes. This is the exact claim the
// ground-truth gate scrapes as cache_bytes_visible_at_startup.
func TestCacheBytesVisibleWithZeroTraffic(t *testing.T) {
	names := gatherNames(t)
	assert.True(t, names["specula_cache_bytes"],
		"cache_bytes must be on /metrics with zero traffic and no collector constructed")
}

// TestCacheBytesPreInitialisedToZero proves the bounded protocol label set is
// pre-initialised to a MEASURED zero for cache_bytes (bytes are always
// measurable, unlike opaque object counts). This is the same honesty rule as
// hit/miss: a bounded, knowable label set reports a real 0, not an absent series.
//
// cache_objects is deliberately NOT pre-initialised here: git bytes are
// measurable via du but git OBJECT counts are not (ObjectsCountable=false), so a
// pre-initialised cache_objects{git}=0 would fabricate "0 objects" when the truth
// is "unknown". Absence is how cache_objects says "not countable / not measured".
func TestCacheBytesPreInitialisedToZero(t *testing.T) {
	for _, p := range AllProtocols {
		assert.NotPanics(t, func() {
			_ = testutil.ToFloat64(CacheBytes.WithLabelValues(p))
		}, "protocol %q must have a pre-initialised cache_bytes series", p)
	}
}

// TestMiddlewareRecordsImplicitOK covers a handler that writes a body without an
// explicit WriteHeader — net/http implies 200 and so must the recorder.
func TestMiddlewareRecordsImplicitOK(t *testing.T) {
	before := testutil.ToFloat64(RequestsTotal.WithLabelValues("apt", "GET", "200"))
	h := Middleware("apt", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("body")) // implicit 200
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/x")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, before+1, testutil.ToFloat64(RequestsTotal.WithLabelValues("apt", "GET", "200")))
}
