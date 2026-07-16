package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ivanzzeth/specula/internal/artifact"
)

const (
	// defaultMaxAttempts is the total number of HTTP attempts per upstream
	// before the client gives up and moves to the next one in the chain.
	// 1 = no retry; 3 = initial attempt + 2 retries.
	defaultMaxAttempts = 3

	// defaultBackoffBase is the duration of the first retry backoff; subsequent
	// backoffs double: 100 ms → 200 ms → 400 ms (capped at 2 s).
	defaultBackoffBase = 100 * time.Millisecond

	// defaultHTTPTimeout is the per-request deadline for the underlying
	// http.Client. Individual requests should be bound by the caller's context,
	// but this acts as an outer safety net.
	defaultHTTPTimeout = 30 * time.Second
)

// fallbackClient is the production implementation of Client.
type fallbackClient struct {
	http        *http.Client
	blocker     *blockTracker
	maxAttempts int
	backoffBase time.Duration
}

// newFallbackClient returns a Client with production-ready defaults.
func newFallbackClient() *fallbackClient {
	return &fallbackClient{
		http:        &http.Client{Timeout: defaultHTTPTimeout},
		blocker:     newBlockTracker(),
		maxAttempts: defaultMaxAttempts,
		backoffBase: defaultBackoffBase,
	}
}

// Fetch tries upstreams in ascending Priority order and returns the first
// successful streaming response.
//
// Within each upstream, transient errors (5xx, 429, network errors) are
// retried up to maxAttempts times with exponential back-off. Non-transient
// errors (4xx except 429) cause an immediate move to the next upstream.
//
// Transient failures are counted toward auto-blocking; successful fetches
// reset the counter.
func (c *fallbackClient) Fetch(
	ctx context.Context,
	ref artifact.ArtifactRef,
	upstreams []Upstream,
) (io.ReadCloser, artifact.UpstreamMeta, error) {
	sorted := sortedUpstreams(upstreams)
	var (
		lastErr error
		tried   int
	)
	for _, up := range sorted {
		if c.blocker.isBlocked(up.Name) {
			continue
		}
		tried++
		body, meta, transient, err := c.tryFetch(ctx, ref, up, nil)
		if err == nil {
			c.blocker.recordSuccess(up.Name)
			return body, meta, nil
		}
		if isContextError(err) {
			return nil, artifact.UpstreamMeta{}, err
		}
		if transient {
			c.blocker.recordFailure(up.Name)
		}
		lastErr = err
	}
	if tried == 0 {
		return nil, artifact.UpstreamMeta{}, errors.New("upstream: all upstreams are blocked")
	}
	return nil, artifact.UpstreamMeta{}, fmt.Errorf("upstream: all upstreams failed: %w", lastErr)
}

// Revalidate performs a conditional GET using prev.ETag (If-None-Match) and/or
// prev.LastModified (If-Modified-Since). Upstreams are tried in the same
// priority order as Fetch.
//
// When an upstream replies 304 the method returns notModified=true with a nil
// body; the caller should extend the mutable entry's TTL without re-fetching.
// When the upstream replies 200 the new body and updated meta are returned.
func (c *fallbackClient) Revalidate(
	ctx context.Context,
	ref artifact.ArtifactRef,
	prev artifact.UpstreamMeta,
	upstreams []Upstream,
) (io.ReadCloser, artifact.UpstreamMeta, bool, error) {
	sorted := sortedUpstreams(upstreams)
	var (
		lastErr error
		tried   int
	)
	for _, up := range sorted {
		if c.blocker.isBlocked(up.Name) {
			continue
		}
		tried++
		body, meta, transient, err := c.tryFetch(ctx, ref, up, &prev)
		if err == nil {
			c.blocker.recordSuccess(up.Name)
			if meta.StatusCode == http.StatusNotModified {
				return nil, meta, true, nil
			}
			return body, meta, false, nil
		}
		if isContextError(err) {
			return nil, artifact.UpstreamMeta{}, false, err
		}
		if transient {
			c.blocker.recordFailure(up.Name)
		}
		lastErr = err
	}
	if tried == 0 {
		return nil, artifact.UpstreamMeta{}, false,
			errors.New("upstream: all upstreams are blocked")
	}
	return nil, artifact.UpstreamMeta{}, false,
		fmt.Errorf("upstream: all upstreams failed: %w", lastErr)
}

// tryFetch performs up to c.maxAttempts GET requests against a single upstream.
//
// prev, when non-nil, adds conditional GET headers (If-None-Match /
// If-Modified-Since). Transient errors (5xx, 429, network errors) trigger a
// retry with exponential back-off; non-transient errors (4xx except 429)
// return immediately so the caller can try the next upstream.
//
// Returns (body, meta, transient, error).
//   - body is non-nil and meta.StatusCode in [200,299] on success.
//   - meta.StatusCode == 304 on "not modified"; body is nil.
//   - transient=true means the error should be counted toward auto-blocking.
func (c *fallbackClient) tryFetch(
	ctx context.Context,
	ref artifact.ArtifactRef,
	up Upstream,
	prev *artifact.UpstreamMeta,
) (io.ReadCloser, artifact.UpstreamMeta, bool, error) {
	rawURL := buildURL(up.BaseURL, ref)
	var (
		lastErr   error
		transient bool
	)
	for attempt := 0; attempt < c.maxAttempts; attempt++ {
		if attempt > 0 {
			wait := c.backoffBase * (1 << uint(attempt-1))
			if wait > 2*time.Second {
				wait = 2 * time.Second
			}
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return nil, artifact.UpstreamMeta{}, false, ctx.Err()
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			// Permanent — bad URL, no point retrying.
			return nil, artifact.UpstreamMeta{}, false,
				fmt.Errorf("upstream %s: build request: %w", up.Name, err)
		}
		if prev != nil {
			if prev.ETag != "" {
				req.Header.Set("If-None-Match", prev.ETag)
			}
			if prev.LastModified != "" {
				req.Header.Set("If-Modified-Since", prev.LastModified)
			}
		}

		resp, doErr := c.http.Do(req)
		if doErr != nil {
			if isContextError(doErr) {
				return nil, artifact.UpstreamMeta{}, false, doErr
			}
			lastErr = fmt.Errorf("upstream %s: %w", up.Name, doErr)
			transient = true
			continue // retry on network error
		}

		meta := extractMeta(resp, up.Name)

		switch {
		case resp.StatusCode == http.StatusNotModified:
			_ = resp.Body.Close()
			return nil, meta, false, nil // caller checks meta.StatusCode

		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			return resp.Body, meta, false, nil

		case resp.StatusCode == http.StatusTooManyRequests:
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("upstream %s: HTTP 429 (rate limited)", up.Name)
			transient = true
			continue // retry on rate limit

		case resp.StatusCode >= 500:
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("upstream %s: HTTP %d", up.Name, resp.StatusCode)
			transient = true
			continue // retry on server error

		default:
			// 4xx (other than 304 / 429): non-retryable; move to next upstream.
			_ = resp.Body.Close()
			return nil, meta, false,
				fmt.Errorf("upstream %s: HTTP %d", up.Name, resp.StatusCode)
		}
	}
	return nil, artifact.UpstreamMeta{}, transient, lastErr
}

// sortedUpstreams returns a copy of us sorted by Priority ascending.
func sortedUpstreams(us []Upstream) []Upstream {
	cp := make([]Upstream, len(us))
	copy(cp, us)
	sort.Slice(cp, func(i, j int) bool {
		return cp[i].Priority < cp[j].Priority
	})
	return cp
}

// extractMeta builds an UpstreamMeta from an HTTP response.
func extractMeta(resp *http.Response, upstreamName string) artifact.UpstreamMeta {
	return artifact.UpstreamMeta{
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		Upstream:     upstreamName,
		ContentType:  resp.Header.Get("Content-Type"),
		StatusCode:   resp.StatusCode,
	}
}

// isContextError returns true for errors that originate from context
// cancellation or deadline expiry.
func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// buildURL constructs the fetch URL from an upstream BaseURL and an
// ArtifactRef. The path structure is protocol-specific; see buildPath.
func buildURL(baseURL string, ref artifact.ArtifactRef) string {
	base := strings.TrimRight(baseURL, "/")
	path := buildPath(ref)
	if path == "" {
		return base
	}
	return base + "/" + path
}

// buildPath derives the URL path component from an ArtifactRef following
// ecosystem conventions. Protocol handlers are responsible for populating
// the relevant ref fields correctly before calling Fetch / Revalidate.
func buildPath(ref artifact.ArtifactRef) string {
	switch ref.Protocol {
	case "oci":
		// Mutable (tag) or unresolved → manifest by tag/reference.
		// Immutable (resolved digest) → blob by digest.
		if ref.Mutable || ref.Digest == "" {
			return "v2/" + ref.Name + "/manifests/" + ref.Version
		}
		return "v2/" + ref.Name + "/blobs/" + ref.Digest

	case "gomod":
		// GOPROXY: /{module}/@v/{file}
		return ref.Name + "/@v/" + ref.Version

	case "pypi":
		if ref.Mutable || ref.Digest == "" {
			return "simple/" + ref.Name + "/"
		}
		return "packages/" + ref.Name + "/" + ref.Version

	case "npm":
		if ref.Mutable || ref.Digest == "" {
			return ref.Name
		}
		return ref.Name + "/-/" + ref.Version

	case "apt":
		if ref.Mutable {
			return "dists/" + ref.Version
		}
		return "pool/" + ref.Name + "/" + ref.Version

	case "helm":
		if ref.Mutable {
			return ref.Name + "/index.yaml"
		}
		return ref.Name + "/" + ref.Version

	case "git":
		return ref.Name + "/info/refs"

	case "tarball":
		if ref.Digest != "" {
			return ref.Name + "/" + ref.Digest
		}
		return ref.Name + "/" + ref.Version

	default:
		if ref.Digest != "" {
			return ref.Name + "/" + ref.Digest
		}
		if ref.Version != "" {
			return ref.Name + "/" + ref.Version
		}
		return ref.Name
	}
}
