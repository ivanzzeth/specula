// Package metrics owns the process-wide Prometheus instruments promised by
// PRD §7. It is a LEAF package: it imports only the Prometheus client and
// internal/artifact (which is itself dependency-free), so every layer — handlers,
// the verify chain, the upstream fetch path — can import it without any risk of
// an import cycle.
//
// # Registration is unconditional
//
// Every instrument is registered on prometheus.DefaultRegisterer in this
// package's variable initialisation, i.e. at process start, merely because
// something imported the package. Nothing is registered lazily as a side effect
// of constructing an object or of a request arriving.
//
// That is deliberate. specula_cache_bytes{protocol="git"} was once invisible on
// /metrics until somebody opened the WebUI, because the gauge was only ever Set
// from a code path the UI triggered (fixed in 7600a0e). A metric that only
// appears once you go looking for it cannot tell an operator anything they did
// not already know. So: registration happens at init, and every instrument whose
// label set is bounded and knowable in advance is pre-initialised to zero (see
// PreInitProtocol / PreInitUpstream) so a fresh headless process reports a real
// zero rather than an absent series.
//
// # Absence is meaningful
//
// Where a label combination is NOT knowable in advance, the series is left
// absent until the event actually happens. Absence means "this did not happen",
// never "this is zero". That is the same honesty rule internal/artifact states
// for SizeStat.ObjectsCountable and specula_cache_objects: absent is how a
// Prometheus metric says "not applicable".
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// AllProtocols is the bounded set of protocol label values. PRD §2 fixes the
// protocol list at eight; this slice is what makes protocol a safe label and is
// the basis for pre-initialising the hit/miss counters.
var AllProtocols = []string{"oci", "pypi", "npm", "gomod", "apt", "helm", "tarball", "git"}

// CheckChain is the reserved `check` label value for the verification Chain's
// aggregate verdict, as opposed to an individual verifier's own result.
//
// The chain-level series is the load-bearing one: its tier is the exact value
// cache.Store persists to CacheEntry.Tier, so specula_verification_total
// {check="chain"} and the tier column in the metadata store are two renderings
// of one number and can be cross-checked against each other.
const CheckChain = "chain"

// cnLatencyBuckets are the histogram buckets for specula_upstream_latency_seconds.
//
// These are derived from measured CN-region time-to-first-byte, NOT from
// prometheus.DefBuckets. Measured (3 samples each, 2026-07):
//
//	mirrors.aliyun.com/ubuntu       0.246s  0.273s  5.071s
//	registry.npmmirror.com          0.365s  0.551s  0.628s
//	sum.golang.google.cn            0.338s  0.344s  0.424s
//	goproxy.cn                      0.387s  0.622s  1.445s
//	mirror.azure.cn/kubernetes      0.757s  0.794s  0.813s
//	pypi.tuna.tsinghua.edu.cn       1.076s  5.638s  5.736s
//	pypi.org                        0.440s  5.277s  5.307s
//
// The distribution is bimodal: a "warm path" cluster at 0.24–0.81s and a stall
// cluster at 5.0–5.7s (connect/TLS stalls, routine on cross-border links).
// DefBuckets would be actively misleading here: it spends five buckets below
// 100ms — no cross-border round trip is ever that fast, so they are dead weight —
// it has no boundary at all between 1s and 2.5s, and it ends at 10s while
// upstream's own http.Client timeout is 30s, so every request between 10s and
// the timeout would vanish into +Inf and be indistinguishable from a hang.
//
// So: start at 0.05 (a same-region/LAN mirror, the only realistic sub-100ms
// case), resolve the warm cluster with 0.25/0.5/0.75/1, bracket the stall
// cluster with 5/7.5, and terminate at 30 = defaultHTTPTimeout so a timeout
// lands in a real bucket instead of +Inf.
var cnLatencyBuckets = []float64{0.05, 0.1, 0.25, 0.5, 0.75, 1, 1.5, 2.5, 5, 7.5, 10, 20, 30}

var (
	// RequestsTotal counts data-plane requests that reached a protocol handler.
	//
	// Labels: protocol (bounded, 8), method (bounded, GET/HEAD/POST/PUT/PATCH/
	// DELETE), status (bounded, the ~15 HTTP codes we actually emit).
	//
	// Deliberately NOT labelled with: path, repo, package name, module path,
	// image name, tag or digest. Each of those is attacker-influenced and
	// unbounded — one `pip install` of a typosquat campaign would mint a fresh
	// series per package name and take the Prometheus server down. Per-package
	// questions belong in logs and the Admin API, not in a metric label.
	RequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "specula_requests_total",
			Help: "Data-plane requests served, by protocol, HTTP method and response status.",
		},
		[]string{"protocol", "method", "status"},
	)

	// CacheHitsTotal counts requests whose response body was produced WITHOUT
	// fetching that body from an upstream. See RecordCacheOutcome for the exact
	// definition and its deliberate limits.
	CacheHitsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "specula_cache_hits_total",
			Help: "Requests whose response body was served from cache without fetching the body from an upstream (a 304 revalidation and a serve-stale both count as hits: the body came from cache).",
		},
		[]string{"protocol"},
	)

	// CacheMissesTotal counts requests whose response body had to be fetched
	// from an upstream. See RecordCacheOutcome.
	CacheMissesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "specula_cache_misses_total",
			Help: "Requests whose response body had to be fetched from an upstream (includes meta-hit-but-blob-missing refetches).",
		},
		[]string{"protocol"},
	)

	// UpstreamLatencySeconds observes upstream responsiveness.
	//
	// IMPORTANT: this is time-to-response-headers, not body download time.
	// upstream.tryFetch stops the clock the instant headers are available,
	// because bodies are streamed straight through to the downstream client and
	// so their duration measures the CLIENT's link speed, not the mirror's.
	//
	// An operator must therefore NOT read this as "how slow is this mirror".
	// A mirror can answer headers in 250ms and then deliver the body at 27 kB/s
	// (measured on a real aliyun link) — a 50 MB .deb would take half an hour
	// and this histogram would still report 0.25s. It answers exactly one
	// question: how long does this upstream take to start responding.
	UpstreamLatencySeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "specula_upstream_latency_seconds",
			Help:    "Upstream time-to-response-headers (NOT body transfer time), by protocol and upstream.",
			Buckets: cnLatencyBuckets,
		},
		[]string{"protocol", "upstream"},
	)

	// UpstreamBlocked reports whether an upstream is currently inside its
	// auto-block window. 1 = blocked, 0 = not blocked.
	//
	// This is a GAUGE, not a counter: PRD §7 describes it as "auto-block 状态"
	// (state), and a state that goes both up and down is a gauge. The name
	// correctly carries no _total suffix (Prometheus reserves _total for
	// counters), so the PRD name is right and is kept.
	//
	// # What an operator may and may NOT conclude (PRD §G5)
	//
	// It means exactly: "upstream.blockTracker has seen defaultMaxFailures (5)
	// CONSECUTIVE TRANSIENT failures for this upstream and is refusing to try it
	// for defaultBlockDuration (30s)". It does NOT mean "this upstream is
	// unreachable".
	//
	// upstream/client.go classifies failures, and the classification is what this
	// gauge inherits:
	//
	//   connection refused → network error  → transient → counts toward blocking
	//   DNS failure        → network error  → transient → counts toward blocking
	//   TCP/TLS timeout    → network error  → transient → counts toward blocking
	//   HTTP 5xx, 429      →                  transient → counts toward blocking
	//   HTTP 451, 403, 404 → 4xx default    → NOT transient → NEVER blocks
	//
	// The CN consequence is sharp and worth stating plainly: GFW-style
	// interference usually surfaces as a connect timeout or reset, which IS
	// transient, so it will trip this gauge to 1. But an HTTP 451 (legal
	// blocking) is a well-formed 4xx: the client moves to the next upstream
	// immediately and the failure streak is never incremented, so an upstream
	// that answers 451 to every request forever will report blocked=0 for ever.
	// "Not blocked" therefore does not mean "healthy" — it can equally mean
	// "failing in a way we deliberately do not retry".
	UpstreamBlocked = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "specula_upstream_blocked",
			Help: "1 when an upstream is inside its auto-block window (5 consecutive TRANSIENT failures: network error/timeout/5xx/429). Non-transient failures such as HTTP 451 or 404 never set this.",
		},
		[]string{"protocol", "upstream"},
	)

	// VerificationTotal counts verification outcomes — the metric that makes the
	// honest tiered trust model (PRD §G2) independently observable.
	//
	// Labels:
	//   protocol — bounded, 8.
	//   check    — the verifier's Name() (checksum, tofu, sumdb, gpg, helm-prov,
	//              git-signed, consensus, cosign), or CheckChain for the chain's
	//              aggregate verdict. Bounded by the registered verifier set.
	//   tier     — the tier ACTUALLY reached, and only ever one of PRD §G2's four:
	//              signed | consensus | tofu | checksum. Never asserted from
	//              config, never a fifth value.
	//   result   — pass | warn | fail.
	//
	// A verifier that SKIPPED (self-gated out: wrong protocol, mutable ref, no
	// digest) emits NO series at all. This is the whole point of the metric: a
	// skipped verifier has reached no tier, and emitting tier="checksum",
	// result="pass" for it — which is literally what artifact.Result carried for
	// skips before StatusSkip existed — would report that the gpg check passed on
	// every npm tarball Specula ever served. Absence is how this metric says
	// "that check did not run here".
	VerificationTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "specula_verification_total",
			Help: "Verification outcomes by protocol, check, honest trust tier actually reached (signed|consensus|tofu|checksum), and result. check=\"chain\" is the aggregate verdict persisted to CacheEntry.Tier. Skipped checks emit no series.",
		},
		[]string{"protocol", "check", "tier", "result"},
	)

	// CacheBytes reports cached bytes per protocol (Grafana sum() for the total).
	//
	// It lives HERE, registered at package init, and NOT inside stats.newCollector,
	// on purpose. Registering it as a side effect of constructing a Collector is
	// the exact bug class PRD §7 opens by forbidding and that shipped once already
	// (7600a0e: specula_cache_bytes{protocol="git"} was invisible on /metrics until
	// somebody opened the WebUI). stats.Collector now writes THROUGH this gauge
	// rather than owning its own, so a fresh headless process — one that never
	// constructs a Collector and serves no traffic — still reports the series.
	//
	// The bytes are AUTHORITATIVE-store values (SUM(size) GROUP BY protocol) for
	// CAS protocols and a du -sb walk for opaque roots (git). Both are always
	// measurable, which is why (unlike CacheObjects) this gauge is pre-initialised
	// to zero for the bounded protocol set at init — see PreInitCacheBytes.
	CacheBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "specula_cache_bytes",
			Help: "Cached bytes per protocol (use sum() in Grafana for the total).",
		},
		[]string{"protocol"},
	)

	// CacheObjects reports the cached object count per protocol.
	//
	// Registered at init like CacheBytes, but DELIBERATELY NOT pre-initialised.
	// Object counts are exact only for CAS-backed protocols; an opaque cache (a git
	// bare mirror) stores its content inside packfiles, not as countable CAS rows,
	// so its object count is UNKNOWABLE (artifact.SizeStat.ObjectsCountable=false).
	// A pre-initialised cache_objects{protocol="git"}=0 would fabricate "zero
	// objects" when the honest answer is "not countable" — the very "render '—',
	// never a fabricated zero" rule this repo set in e181e5a. Absence is how this
	// gauge says "not countable / not measured". The Collector Sets it only for
	// protocols whose count is real.
	CacheObjects = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "specula_cache_objects",
			Help: "Cached object count per protocol (CAS-backed protocols only; opaque caches such as git are absent because their object count is not countable).",
		},
		[]string{"protocol"},
	)
)

func init() {
	prometheus.MustRegister(
		RequestsTotal,
		CacheHitsTotal,
		CacheMissesTotal,
		UpstreamLatencySeconds,
		UpstreamBlocked,
		VerificationTotal,
		CacheBytes,
		CacheObjects,
	)
	// Pre-initialise every bounded label set that is knowable without traffic.
	// Counters and gauges here report a true zero on a fresh headless process,
	// so rate() and hit-ratio expressions are well defined from the first scrape
	// instead of erroring on a missing series.
	for _, p := range AllProtocols {
		CacheHitsTotal.WithLabelValues(p)
		CacheMissesTotal.WithLabelValues(p)
		// cache_bytes is pre-initialised too: bytes are ALWAYS measurable, so 0 on
		// a cold cache is a real "measured, nothing cached", identical in kind to a
		// hit counter reading 0 — not the fabricated zero e181e5a forbids (that rule
		// is about UNKNOWABLE quantities, which is why cache_objects is NOT here).
		// A warm/persistent store overwrites these zeros with the real SUM(size)
		// SYNCHRONOUSLY at startup, before /metrics is reachable (see
		// cmd/specula: collector.Refresh before the servers listen), so an operator
		// never scrapes a stale zero on restart either.
		CacheBytes.WithLabelValues(p)
	}
}

// PreInitUpstream declares a configured (protocol, upstream) pair so its
// blocked gauge reads a real 0 before any traffic. Without this the series would
// be absent until the upstream first failed, and "absent" and "healthy" would be
// indistinguishable at exactly the moment an operator cares.
func PreInitUpstream(protocol, upstream string) {
	UpstreamBlocked.WithLabelValues(protocol, upstream)
}

// tierLabel renders a Tier as its PRD §G2 label. The four tiers are the only
// permitted values; artifact.Tier has exactly four variants, so every value the
// verify chain can produce maps to one of them.
func tierLabel(t artifact.Tier) string { return t.String() }

// RecordVerification records one verification outcome.
//
// tier MUST be the tier the verifier/chain actually reached — the value carried
// in artifact.Result.Tier — never a tier read back from configuration. Callers
// must not invoke this for a skipped check; see VerificationTotal.
func RecordVerification(protocol, check string, tier artifact.Tier, result artifact.Status) {
	VerificationTotal.WithLabelValues(protocol, check, tierLabel(tier), result.String()).Inc()
}

// RecordUpstreamLatency observes one upstream time-to-headers measurement.
func RecordUpstreamLatency(protocol, upstream string, seconds float64) {
	UpstreamLatencySeconds.WithLabelValues(protocol, upstream).Observe(seconds)
}

// SetUpstreamBlocked sets the auto-block state gauge for an upstream.
func SetUpstreamBlocked(protocol, upstream string, blocked bool) {
	v := 0.0
	if blocked {
		v = 1.0
	}
	UpstreamBlocked.WithLabelValues(protocol, upstream).Set(v)
}

// RecordRequest counts one served data-plane request.
func RecordRequest(protocol, method string, status int) {
	RequestsTotal.WithLabelValues(protocol, method, httpStatusLabel(status)).Inc()
}

// httpStatusLabel renders an HTTP status as a label value. Statuses are drawn
// from the bounded set the handlers emit; anything outside 100–599 is a
// programming error and is bucketed rather than minting an unbounded label.
func httpStatusLabel(status int) string {
	if status < 100 || status > 599 {
		return "invalid"
	}
	return statusStrings[status-100]
}

// statusStrings is a precomputed 100..599 decimal table, avoiding a strconv
// allocation on every request in the hot path.
var statusStrings = func() [500]string {
	var out [500]string
	for i := range out {
		n := i + 100
		out[i] = string(rune('0'+n/100)) + string(rune('0'+(n/10)%10)) + string(rune('0'+n%10))
	}
	return out
}()
