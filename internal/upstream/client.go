package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
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

	// tokenExpiryBuffer is subtracted from the server-reported token TTL so
	// we refresh slightly before expiry rather than exactly at it.
	tokenExpiryBuffer = 30 * time.Second

	// defaultTokenTTL is used when the token endpoint does not report expires_in.
	defaultTokenTTL = 5 * time.Minute
)

// tokenEntry is a cached registry bearer token with its expiry.
type tokenEntry struct {
	token   string
	expires time.Time
}

// fallbackClient is the production implementation of Client.
type fallbackClient struct {
	http        *http.Client
	blocker     *blockTracker
	maxAttempts int
	backoffBase time.Duration

	// tokenMu guards the token cache for concurrent access.
	tokenMu sync.RWMutex
	// tokens caches bearer tokens keyed by "upstreamName:scope".
	tokens map[string]tokenEntry
}

// newFallbackClient returns a Client with production-ready defaults.
func newFallbackClient() *fallbackClient {
	return &fallbackClient{
		http:        &http.Client{Timeout: defaultHTTPTimeout},
		blocker:     newBlockTracker(),
		maxAttempts: defaultMaxAttempts,
		backoffBase: defaultBackoffBase,
		tokens:      make(map[string]tokenEntry),
	}
}

// Fetch tries upstreams in ascending Priority order and returns the first
// successful streaming response.
//
// Within each upstream, transient errors (5xx, 429, network errors) are
// retried up to maxAttempts times with exponential back-off. Non-transient
// errors (4xx except 401 with bearer challenge / 429) cause an immediate
// move to the next upstream.
//
// On 401 with a Bearer WWW-Authenticate challenge, the client fetches a token
// from the realm endpoint and retries once with Authorization: Bearer.
//
// Transient failures are counted toward auto-blocking; successful fetches
// reset the counter.
func (c *fallbackClient) Fetch(
	ctx context.Context,
	ref artifact.ArtifactRef,
	upstreams []Upstream,
	opts ...RequestOption,
) (io.ReadCloser, artifact.UpstreamMeta, error) {
	ropts := buildRequestOpts(opts)
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
		body, meta, transient, err := c.tryFetch(ctx, ref, up, nil, ropts)
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
	opts ...RequestOption,
) (io.ReadCloser, artifact.UpstreamMeta, bool, error) {
	ropts := buildRequestOpts(opts)
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
		body, meta, transient, err := c.tryFetch(ctx, ref, up, &prev, ropts)
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
// retry with exponential back-off; non-transient errors (4xx except 401 with
// bearer challenge) return immediately so the caller can try the next upstream.
//
// On 401 with a Bearer WWW-Authenticate challenge, fetches a token and retries
// once without consuming an attempt slot.
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
	opts requestOpts,
) (io.ReadCloser, artifact.UpstreamMeta, bool, error) {
	rawURL := buildURL(up.BaseURL, ref)
	var (
		lastErr      error
		transient    bool
		authToken    string // bearer token; populated after first 401
		didAuthRetry bool   // true once the bearer dance has been attempted
	)

	// Pre-populate authToken from the cache for subsequent requests to the
	// same upstream+scope (avoids a token round-trip for every blob).
	if ref.Protocol == "oci" {
		scope := "repository:" + ref.Name + ":pull"
		if tok := c.getCachedToken(up.Name, scope); tok != "" {
			authToken = tok
		}
	}

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

		// Conditional GET headers (for Revalidate).
		if prev != nil {
			if prev.ETag != "" {
				req.Header.Set("If-None-Match", prev.ETag)
			}
			if prev.LastModified != "" {
				req.Header.Set("If-Modified-Since", prev.LastModified)
			}
		}

		// Accept header (e.g. OCI manifest content negotiation).
		if opts.accept != "" {
			req.Header.Set("Accept", opts.accept)
		}

		// Bearer auth token (present after first 401 dance, or from cache).
		if authToken != "" {
			req.Header.Set("Authorization", "Bearer "+authToken)
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

		case resp.StatusCode == http.StatusUnauthorized && !didAuthRetry:
			// Bearer token dance: attempt once without consuming a retry slot.
			_ = resp.Body.Close()
			didAuthRetry = true
			wwwAuth := resp.Header.Get("WWW-Authenticate")
			tok, authErr := c.getOrFetchToken(ctx, wwwAuth, up)
			if authErr != nil {
				// No valid challenge or token fetch failed: non-retryable.
				return nil, meta, false,
					fmt.Errorf("upstream %s: HTTP 401 unauthorized: %w", up.Name, authErr)
			}
			authToken = tok
			// Re-run the same attempt slot with the new token.
			// The for-loop post-statement does attempt++, so decrement first.
			attempt--
			continue

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
			// 4xx (other than 304 / 401 with challenge / 429): non-retryable.
			_ = resp.Body.Close()
			return nil, meta, false,
				fmt.Errorf("upstream %s: HTTP %d", up.Name, resp.StatusCode)
		}
	}
	return nil, artifact.UpstreamMeta{}, transient, lastErr
}

// ── Bearer token helpers ──────────────────────────────────────────────────────

// parseBearerChallenge parses a WWW-Authenticate: Bearer ... header.
// It returns the realm, service, and scope extracted from the challenge params,
// and ok=true when at least realm is present.
//
// Example input:
//
//	Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/nginx:pull"
func parseBearerChallenge(header string) (realm, service, scope string, ok bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", "", "", false
	}
	params := strings.TrimSpace(header[len(prefix):])

	for params != "" {
		// Skip leading commas and spaces.
		params = strings.TrimLeft(params, ", ")
		if params == "" {
			break
		}

		// Find the key (up to '=').
		eqIdx := strings.IndexByte(params, '=')
		if eqIdx < 0 {
			break
		}
		key := strings.TrimSpace(params[:eqIdx])
		params = params[eqIdx+1:]

		// Extract the value (quoted or unquoted).
		var value string
		if strings.HasPrefix(params, `"`) {
			// Quoted value: scan to the closing '"'.
			endIdx := strings.IndexByte(params[1:], '"')
			if endIdx < 0 {
				break
			}
			value = params[1 : endIdx+1]
			params = params[endIdx+2:]
		} else {
			// Unquoted: value ends at the next comma.
			commaIdx := strings.IndexByte(params, ',')
			if commaIdx < 0 {
				value = params
				params = ""
			} else {
				value = params[:commaIdx]
				params = params[commaIdx+1:]
			}
		}

		switch key {
		case "realm":
			realm = value
		case "service":
			service = value
		case "scope":
			scope = value
		}
	}

	return realm, service, scope, realm != ""
}

// tokenResponse is the JSON body from a registry bearer token endpoint.
type tokenResponse struct {
	Token     string `json:"token"`
	ExpiresIn int    `json:"expires_in"` // seconds
}

// fetchBearerToken fetches a bearer token from the given realm endpoint,
// adding service and scope as query parameters.
func (c *fallbackClient) fetchBearerToken(ctx context.Context, realm, service, scope string) (string, time.Time, error) {
	u, err := url.Parse(realm)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("bearer: parse realm %q: %w", realm, err)
	}
	q := u.Query()
	if service != "" {
		q.Set("service", service)
	}
	if scope != "" {
		q.Set("scope", scope)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("bearer: build token request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("bearer: token fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("bearer: token endpoint returned HTTP %d", resp.StatusCode)
	}

	var tr tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", time.Time{}, fmt.Errorf("bearer: decode token response: %w", err)
	}
	if tr.Token == "" {
		return "", time.Time{}, errors.New("bearer: empty token in response")
	}

	ttl := defaultTokenTTL
	if tr.ExpiresIn > 0 {
		ttl = time.Duration(tr.ExpiresIn)*time.Second - tokenExpiryBuffer
		if ttl < 0 {
			ttl = 0
		}
	}
	expires := time.Now().Add(ttl)
	return tr.Token, expires, nil
}

// getCachedToken returns a cached, non-expired bearer token for the given
// upstream name and scope. Returns "" when no valid token is cached.
func (c *fallbackClient) getCachedToken(upName, scope string) string {
	key := upName + ":" + scope
	c.tokenMu.RLock()
	e, ok := c.tokens[key]
	c.tokenMu.RUnlock()
	if ok && time.Now().Before(e.expires) {
		return e.token
	}
	return ""
}

// setCachedToken stores a bearer token in the cache.
func (c *fallbackClient) setCachedToken(upName, scope, token string, expires time.Time) {
	key := upName + ":" + scope
	c.tokenMu.Lock()
	c.tokens[key] = tokenEntry{token: token, expires: expires}
	c.tokenMu.Unlock()
}

// getOrFetchToken parses the WWW-Authenticate challenge in wwwAuth, checks
// the token cache, and fetches a new token from the realm endpoint if needed.
// Returns a non-empty token on success or an error when the challenge is
// absent/unparseable or the token fetch fails.
func (c *fallbackClient) getOrFetchToken(ctx context.Context, wwwAuth string, up Upstream) (string, error) {
	realm, service, scope, ok := parseBearerChallenge(wwwAuth)
	if !ok {
		return "", fmt.Errorf("upstream %s: no parseable Bearer challenge in WWW-Authenticate: %q", up.Name, wwwAuth)
	}

	// Return cached token if still valid.
	if tok := c.getCachedToken(up.Name, scope); tok != "" {
		return tok, nil
	}

	tok, expires, err := c.fetchBearerToken(ctx, realm, service, scope)
	if err != nil {
		return "", fmt.Errorf("upstream %s: %w", up.Name, err)
	}

	c.setCachedToken(up.Name, scope, tok, expires)
	return tok, nil
}

// ── Internal helpers ──────────────────────────────────────────────────────────

// buildRequestOpts collapses a slice of RequestOption into a requestOpts struct.
func buildRequestOpts(opts []RequestOption) requestOpts {
	var o requestOpts
	for _, fn := range opts {
		fn(&o)
	}
	return o
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
		// GOPROXY: /{module}/@latest for the latest-version endpoint, else
		// /{module}/@v/{file} where file is list | <v>.info | <v>.mod | <v>.zip.
		// ref.Name is the escaped (URL-form) module path; ref.Version is the
		// @v file component ("@latest" sentinel routes to the /@latest endpoint).
		if ref.Version == "@latest" {
			return ref.Name + "/@latest"
		}
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
