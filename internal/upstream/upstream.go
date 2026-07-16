// Package upstream implements the fallback-chain upstream Client used by the
// cache-miss path. It handles:
//
//   - Ordered fallback: upstreams are sorted by Priority (ascending) and tried
//     in that order. The first successful response wins.
//   - Bounded retry with exponential back-off: transient errors (5xx, 429,
//     network errors) are retried within the same upstream before falling back.
//   - Conditional GET (mutable tier revalidation): If-None-Match /
//     If-Modified-Since are sent on Revalidate; a 304 is surfaced as
//     notModified=true so the caller can extend the TTL without re-fetching.
//   - Auto-block / auto-unblock (Nexus-style): after maxFailures consecutive
//     transient errors the upstream is blocked for blockDuration, then
//     automatically unblocked on the next isBlocked check.
//
// The body returned by Fetch / Revalidate is a streaming io.ReadCloser; the
// implementation never buffers blob bytes in memory.
package upstream

import (
	"context"
	"io"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// Upstream describes a single upstream mirror in the fallback chain.
type Upstream struct {
	Name     string // logical name (e.g. "daocloud", "npmmirror")
	BaseURL  string // base URL for the mirror
	Priority int    // lower = tried first
	Official bool   // true if this is the authoritative origin (for origin-check)
}

// Client fetches artifacts from a fallback chain of upstreams.
type Client interface {
	// Fetch streams the artifact bytes from the first healthy upstream in
	// upstreams, returning the reader and upstream metadata. Upstreams are
	// tried in ascending Priority order; transient failures trigger bounded
	// retry within the same upstream before the next one is attempted.
	Fetch(ctx context.Context, ref artifact.ArtifactRef, upstreams []Upstream) (io.ReadCloser, artifact.UpstreamMeta, error)

	// Revalidate performs a conditional GET using prev (ETag / Last-Modified).
	// notModified is true when the upstream answered 304 (mutable tier still
	// fresh); in that case the reader is nil and no byte transfer occurred.
	// On 200 the new body and updated meta are returned.
	Revalidate(ctx context.Context, ref artifact.ArtifactRef, prev artifact.UpstreamMeta, upstreams []Upstream) (body io.ReadCloser, meta artifact.UpstreamMeta, notModified bool, err error)
}

// NewClient constructs the default fallback-chain upstream Client.
func NewClient() Client { return newFallbackClient() }
