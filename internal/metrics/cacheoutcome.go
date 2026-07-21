package metrics

import (
	"context"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// # Why hit/miss is bound to the request, not to the cache lookup
//
// The obvious place to count hits and misses is cache.Manager.Lookup. It is the
// wrong place. Handlers call Lookup more than once per request (resolve a tag,
// then look up the blob it names; probe, then re-probe after a store), and a
// miss that is refetched and stored is then read back out of cache before being
// served. Counting there makes the denominator "lookups", not "requests", so
// hit_ratio = hits/(hits+misses) would answer a question no operator ever asked
// and would drift with internal refactors of the handlers. It would not be
// wrong by a little; it would be a different quantity wearing the name.
//
// So the outcome is recorded against the REQUEST. A handler marks the outcome at
// the point it decides where the response body came from; the marker is
// first-write-wins, so repeated marks within one request cannot double count;
// and the middleware flushes exactly one observation when the request ends.
//
// # The definitions
//
// hit  — the response body was produced WITHOUT fetching that body from an
//        upstream.
// miss — the response body had to be fetched from an upstream.
//
// The axis is the ORIGIN OF THE BYTES, and it is chosen so that the ratio an
// operator computes means one true thing: the fraction of served bodies that
// cost no upstream body transfer. That is the question the cache exists to
// answer, and it is the one that maps to bandwidth and to CN egress cost.
//
// The three genuinely ambiguous cases PRD/ARCHITECTURE §3 create, and how they
// are resolved:
//
//   - Mutable revalidation returning 304. Counted as a HIT. We spoke to the
//     upstream, but it sent no body; the bytes we served came from cache. Under
//     the bytes-origin definition this is unambiguously a hit.
//
//   - Serve-stale on upstream failure (fix H1). Counted as a HIT. The body came
//     from cache. The upstream failure is not silently swallowed: it is what
//     specula_upstream_blocked and the absence of a latency observation report.
//
//   - Meta hit but blob missing → refetch (fix M1). Counted as a MISS. The
//     metadata layer had an entry, but the bytes were not there and had to be
//     fetched. Counting it a hit because a row existed would be counting the
//     bookkeeping rather than the cache.
//
// # The limit of this definition, stated plainly
//
// "hit" does NOT mean "no upstream contact". A 304 revalidation is a hit that
// still cost a full round trip to a CN mirror — 0.25s on a good link, 5.7s on a
// bad one (measured). An operator reading a 95% hit ratio must not conclude that
// 95% of requests avoided the upstream; they avoided the upstream BODY. The
// question "how often do we touch an upstream at all" is answered by
// specula_upstream_latency_seconds' count, which observes once per upstream
// round trip including 304s. The two metrics together are honest; either one
// read as the other is not.
//
// A request that never consults the cache at all (a /v2/ ping, a 404 on a
// malformed path, a rejected method) marks nothing and is counted in neither
// counter — it appears only in specula_requests_total. The hit/miss denominator
// is therefore "requests that consulted the cache", which is the only
// denominator for which the ratio is meaningful.

// outcome is the cache outcome recorded for a single request.
type outcome int32

const (
	outcomeNone outcome = iota // request did not consult the cache
	outcomeHit
	outcomeMiss
)

// requestScope carries the per-request cache outcome. It is written by handlers
// via MarkHit/MarkMiss and read once by the middleware when the request ends.
type requestScope struct {
	protocol string
	// state is an atomic outcome. Handlers may mark from the request goroutine
	// only, but the atomic makes the first-write-wins rule explicit and keeps
	// the type safe under -race if a handler ever marks from a helper goroutine.
	state atomic.Int32
}

// mark applies first-write-wins semantics: the first decision a handler makes
// about where the body came from is the one that counts. A later mark (e.g. the
// read-back Lookup that follows a store-on-miss) cannot overwrite it.
func (s *requestScope) mark(o outcome) {
	s.state.CompareAndSwap(int32(outcomeNone), int32(o))
}

// flush emits the single hit/miss observation for the request, if any.
func (s *requestScope) flush() {
	switch outcome(s.state.Load()) {
	case outcomeHit:
		CacheHitsTotal.WithLabelValues(s.protocol).Inc()
	case outcomeMiss:
		CacheMissesTotal.WithLabelValues(s.protocol).Inc()
	case outcomeNone:
		// Request never consulted the cache: neither counter moves.
	}
}

type scopeKeyType struct{}

var scopeKey scopeKeyType

// scopeFrom returns the request scope carried by ctx, or nil when the caller is
// outside a metrics-instrumented request (e.g. a unit test, or a background
// task). Marking outside a request is a no-op rather than a panic: a metric must
// never be able to take the data plane down.
func scopeFrom(ctx context.Context) *requestScope {
	s, _ := ctx.Value(scopeKey).(*requestScope)
	return s
}

// MarkHit records that this request's response body was served from cache
// without fetching the body from an upstream. First mark wins.
func MarkHit(ctx context.Context) {
	if s := scopeFrom(ctx); s != nil {
		s.mark(outcomeHit)
	}
}

// MarkMiss records that this request's response body had to be fetched from an
// upstream. First mark wins.
func MarkMiss(ctx context.Context) {
	if s := scopeFrom(ctx); s != nil {
		s.mark(outcomeMiss)
	}
}

// statusRecorder captures the response status and body byte count for
// specula_requests_total / specula_response_bytes_total.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	bytes       int64
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		// Implicit 200 on first write, mirroring net/http.
		r.status = http.StatusOK
		r.wroteHeader = true
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

// ReadFrom counts sendfile/zero-copy paths that bypass Write.
func (r *statusRecorder) ReadFrom(src io.Reader) (int64, error) {
	if !r.wroteHeader {
		r.status = http.StatusOK
		r.wroteHeader = true
	}
	if rf, ok := r.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(src)
		r.bytes += n
		return n, err
	}
	n, err := io.Copy(r.ResponseWriter, src)
	r.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer, preserving
// Flush/Hijack for the streaming and git-protocol handlers.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Middleware instruments a protocol handler: it counts every request into
// specula_requests_total{protocol,method,status}, records response body bytes +
// end-to-end duration for runtime throughput, and flushes exactly one
// specula_cache_hits_total / _misses_total observation for the request, based on
// whatever the handler marked via MarkHit/MarkMiss.
func Middleware(protocol string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scope := &requestScope{protocol: protocol}
		ctx := context.WithValue(r.Context(), scopeKey, scope)

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		// Identify Specula on every data-plane response so operators can prove
		// the client hit Specula and not an ambient HTTP_PROXY / upstream CDN.
		// Set before the handler runs so the header is present even on early 4xx.
		w.Header().Set("X-Specula-Protocol", protocol)
		w.Header().Add("Via", "1.1 specula")
		start := time.Now()
		next.ServeHTTP(rec, r.WithContext(ctx))
		elapsed := time.Since(start)

		RecordRequest(protocol, r.Method, rec.status)
		ResponseBytesTotal.WithLabelValues(protocol).Add(float64(rec.bytes))
		RequestDurationSeconds.WithLabelValues(protocol).Observe(elapsed.Seconds())
		recordTraffic(protocol, rec.bytes, elapsed)
		scope.flush()
	})
}
