// Package upstream implements the fallback-chain upstream Client used by the
// cache-miss path. It handles:
//
//   - Ordered fallback: upstreams are sorted by Priority (ascending) and tried
//     in that order. The first successful response wins.
//   - failsafe-go Retry + CircuitBreaker per upstream for transient errors
//     (5xx, 429, network). Header waits use Transport.ResponseHeaderTimeout —
//     failsafe Timeout is NOT wrapped around the HTTP request so streaming
//     bodies are not cancelled when headers return.
//   - Conditional GET (mutable tier revalidation): If-None-Match /
//     If-Modified-Since are sent on Revalidate; a 304 is surfaced as
//     notModified=true so the caller can extend the TTL without re-fetching.
//   - Auto-block / auto-unblock via failsafe CircuitBreaker (threshold +
//     delay), with optional BlockPersister for HA sync.
//   - Registry bearer-token auth dance: on 401 the client parses the
//     WWW-Authenticate challenge, fetches a token from the realm endpoint,
//     and retries with Authorization: Bearer. Tokens are cached per
//     upstream+scope to avoid a round-trip per blob request.
//   - Transparent Range resume on the Fetch body, with cross-upstream
//     fallthrough when the pinned mirror dies mid-stream.
//
// The body returned by Fetch / Revalidate is a streaming io.ReadCloser; the
// implementation never buffers blob bytes in memory.
package upstream

import (
	"context"
	"fmt"
	"io"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// StatusError is returned by Fetch / Revalidate when the reason a fetch failed
// is a DEFINITIVE, non-retryable upstream HTTP status (e.g. 404 Not Found,
// 410 Gone, 403 Forbidden) rather than a transport failure (DNS, connection
// refused, timeout) or an exhausted 5xx retry.
//
// It exists so a data-plane handler can preserve the semantic status the client
// relies on — for GOPROXY that is the 404/410 the go command uses to resolve
// module-path boundaries — instead of flattening every upstream error to 502.
// A transport failure or a 5xx that exhausts retries never yields a StatusError,
// so those keep mapping to 502: a genuine outage must not be reported as a fake
// "does not exist" that the client would then cache.
//
// Recover it with errors.As; it is always wrapped (fmt.Errorf %w) by Fetch.
type StatusError struct {
	Upstream   string // upstream name that produced the status
	StatusCode int    // the upstream HTTP status code
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("upstream %s: HTTP %d", e.Upstream, e.StatusCode)
}

// Upstream describes a single upstream mirror in the fallback chain.
type Upstream struct {
	Name     string // logical name (e.g. "daocloud", "npmmirror")
	BaseURL  string // base URL for the mirror
	Priority int    // lower = tried first
	Official bool   // true if this is the authoritative origin (for origin-check)

	// PathPrefix, when set for OCI, is inserted after /v2/ before the repo
	// name: /v2/{PathPrefix}/{repo}/manifests|blobs/…. Used for nested mirror
	// layouts (Huawei SWR: ddn-k8s/registry.k8s.io/…). Transparent mirrors
	// leave this empty.
	PathPrefix string

	// Proxy, when set, is the HTTP/HTTPS/SOCKS5 proxy this ONE upstream is fetched
	// through ("http://10.0.0.5:3128", "socks5://127.0.0.1:1080").
	//
	// Per-upstream rather than global on purpose. Specula's job is that CN mirrors
	// carry the bulk of the bytes; sending those through a metered proxy as well
	// (which is what HTTPS_PROXY does) pays for traffic the mirrors exist to make
	// free. Put the proxy on the official origin only: it sits last in the chain,
	// so it is reached solely after every mirror has failed.
	//
	// Empty keeps the previous behaviour, including http.ProxyFromEnvironment.
	Proxy string
}

// RequestOption configures an individual upstream HTTP request.
// Options are applied in order before the request is sent.
type RequestOption func(*requestOpts)

// requestOpts holds per-request configuration assembled from RequestOption values.
type requestOpts struct {
	accept     string // value for the Accept request header; empty = no header sent
	hasRange   bool   // when true, send Range: bytes={rangeStart}-
	rangeStart int64  // byte offset for Range resume (only if hasRange)
}

// ociManifestAccept is the full Accept header for OCI manifest content negotiation.
// Order matters: prefer OCI index, then Docker manifest list, then single-arch formats.
const ociManifestAccept = "application/vnd.oci.image.index.v1+json," +
	"application/vnd.docker.distribution.manifest.list.v2+json," +
	"application/vnd.oci.image.manifest.v1+json," +
	"application/vnd.docker.distribution.manifest.v2+json"

// WithOCIManifestAccept sets the Accept header required for correct OCI manifest
// content negotiation. Without this header many registries return schema-1 or
// refuse to serve image indexes (multi-arch manifests).
func WithOCIManifestAccept() RequestOption {
	return func(o *requestOpts) { o.accept = ociManifestAccept }
}

// WithAcceptHeader sets an arbitrary Accept header on the upstream request
// (e.g. PEP 691 application/vnd.pypi.simple.v1+json). Empty accept is a no-op.
func WithAcceptHeader(accept string) RequestOption {
	return func(o *requestOpts) { o.accept = accept }
}

// WithByteRange asks the upstream for bytes starting at start (Range:
// bytes={start}-). Used for durable quarantine resume across aborted cold
// fills. start <= 0 is a no-op (full GET). Mid-stream resume on a live body
// still goes through the Fetch wrapper; this option is for a fresh Fetch that
// already has partial bytes on disk.
func WithByteRange(start int64) RequestOption {
	return func(o *requestOpts) {
		if start > 0 {
			o.hasRange = true
			o.rangeStart = start
		}
	}
}

// Client fetches artifacts from a fallback chain of upstreams.
type Client interface {
	// Fetch streams the artifact bytes from the first healthy upstream in
	// upstreams, returning the reader and upstream metadata. Upstreams are
	// tried in ascending Priority order; transient failures trigger bounded
	// retry within the same upstream before the next one is attempted.
	// opts may set per-request headers such as Accept (see WithOCIManifestAccept).
	Fetch(ctx context.Context, ref artifact.ArtifactRef, upstreams []Upstream, opts ...RequestOption) (io.ReadCloser, artifact.UpstreamMeta, error)

	// Revalidate performs a conditional GET using prev (ETag / Last-Modified).
	// notModified is true when the upstream answered 304 (mutable tier still
	// fresh); in that case the reader is nil and no byte transfer occurred.
	// On 200 the new body and updated meta are returned.
	// opts may set per-request headers (same as Fetch).
	Revalidate(ctx context.Context, ref artifact.ArtifactRef, prev artifact.UpstreamMeta, upstreams []Upstream, opts ...RequestOption) (body io.ReadCloser, meta artifact.UpstreamMeta, notModified bool, err error)
}

// NewClient constructs the default fallback-chain upstream Client. It keeps its
// own auto-block state and records no measurements; use NewClientWithRuntime
// when the mirror chain must be observable or operator-controllable.
func NewClient() Client { return newFallbackClient() }

// NewClientWithRuntime constructs a fallback-chain Client bound to rt, the
// per-protocol Runtime that both observes and steers the chain:
//
//   - every successful fetch records the mirror's latency, serve count and
//     last-served time into rt;
//   - every failure records its reason into rt;
//   - rt's auto-block state is shared with (not merely mirrored from) the fetch
//     path, so the admin view can never disagree with what is actually blocked;
//   - rt's operator overrides (disable / reorder) are applied before each fetch.
//
// rt must be non-nil; pass upstream.NewRuntime(protocol) or a Registry's
// Runtime(protocol).
func NewClientWithRuntime(rt *Runtime) Client { return newFallbackClientWithRuntime(rt) }
