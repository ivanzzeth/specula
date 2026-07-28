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
	"github.com/ivanzzeth/specula/internal/metrics"
)

const (
	// defaultMaxAttempts is the total number of HTTP attempts per upstream
	// before the client gives up and moves to the next one in the chain.
	// 1 = no retry; 3 = initial attempt + 2 retries.
	defaultMaxAttempts = 3

	// defaultBackoffBase is the duration of the first retry backoff; subsequent
	// backoffs double: 100 ms → 200 ms → 400 ms (capped at 2 s).
	defaultBackoffBase = 100 * time.Millisecond

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
	// http is the patient client (full dial/TLS/header budgets) used for the
	// last hop or when httpFast is unset (tests that inject a custom Transport).
	http *http.Client
	// httpFast is the short-dial client for non-final mirrors. Nil → use http.
	httpFast *http.Client
	blocker  *blockTracker
	// rt, when non-nil, is the per-protocol Runtime that records mirror
	// measurements and supplies the operator's runtime overrides. It is
	// optional: a client built with NewClient has no Runtime and behaves
	// exactly as before (config order, no instrumentation). When set, rt.blocker
	// is the same *blockTracker as the blocker field above, so the admin view
	// and the fetch path can never disagree about what is blocked.
	rt          *Runtime
	maxAttempts int
	backoffBase time.Duration

	// proxyClients caches one client per (proxy, fast?) so a proxied origin keeps
	// its connection pool instead of re-dialling per layer.
	proxyMu      sync.Mutex
	proxyClients map[string]*http.Client

	// idleBodyTimeout / maxResumeAttempts configure transparent Range resume
	// on the body returned by Fetch. Zero means package defaults.
	idleBodyTimeout   time.Duration
	maxResumeAttempts int

	// tokenMu guards the token cache for concurrent access.
	tokenMu sync.RWMutex
	// tokens caches bearer tokens keyed by "upstreamName:scope".
	tokens map[string]tokenEntry
}

// newFallbackClient returns a Client with production-ready defaults.
func newFallbackClient() *fallbackClient {
	return &fallbackClient{
		http:              newUpstreamHTTPClient(),
		httpFast:          newUpstreamHTTPClientFast(),
		blocker:           newBlockTracker(),
		maxAttempts:       defaultMaxAttempts,
		backoffBase:       defaultBackoffBase,
		idleBodyTimeout:   defaultIdleBodyTimeout,
		maxResumeAttempts: defaultMaxResumeAttempts,
		tokens:            make(map[string]tokenEntry),
	}
}

// httpFor selects the patient vs fail-fast HTTP client.
func (c *fallbackClient) httpFor(remainingAfter int) *http.Client {
	if remainingAfter > 0 && c.httpFast != nil {
		return c.httpFast
	}
	return c.http
}

// newFallbackClientWithRuntime returns a Client bound to rt: it shares rt's
// block tracker, reports every success/failure into rt, and honours rt's
// enable/disable and reorder overrides when choosing the fallback order.
func newFallbackClientWithRuntime(rt *Runtime) *fallbackClient {
	c := newFallbackClient()
	c.blocker = rt.blocker
	c.rt = rt
	return c
}

// chain returns the fallback order to try: rt's effective order (overrides
// applied) when a Runtime is bound, otherwise plain config priority order.
func (c *fallbackClient) chain(ups []Upstream) []Upstream {
	if c.rt != nil {
		return c.rt.effective(ups)
	}
	return sortedUpstreams(ups)
}

// noteSuccess records the mirror's latency and serve count for the operator
// view. The failsafe CircuitBreaker already recorded success on the attempt;
// we must not tick the breaker again (would double-count).
func (c *fallbackClient) noteSuccess(name string, latency time.Duration) {
	if c.rt != nil {
		c.rt.recordServeStats(name, latency)
	}
}

// noteFailure records the error reason for the operator view. Transient
// failures are already counted by the failsafe CircuitBreaker inside tryFetch;
// calling recordFailure here would double-count and trip the breaker early.
func (c *fallbackClient) noteFailure(name string, err error, _ bool) {
	if c.rt != nil {
		c.rt.setLastErr(name, err)
	}
}

// syncBlocked republishes the auto-block gauge for one upstream.
//
// It is driven from the fetch path rather than from blockTracker because the
// tracker is keyed by upstream name alone and has no idea which protocol it is
// serving, whereas specula_upstream_blocked is labelled {protocol,upstream}.
// ref.Protocol is in scope here and is authoritative.
//
// isBlocked is the right source: it is the same predicate the fetch loop obeys,
// and it performs the lazy auto-unblock, so the gauge can never report a block
// that the fetch path would no longer honour.
func (c *fallbackClient) syncBlocked(protocol, name string) {
	metrics.SetUpstreamBlocked(protocol, name, c.blocker.isBlocked(name))
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
	sorted := c.chain(upstreams)
	var (
		lastErr            error
		statusErr          *StatusError  // first DEFINITIVE upstream status (see resolveFetchError)
		attempts           []attemptNote // one per failed upstream, for the error message
		tried              int
		priorTransportFail bool // any earlier transport failure (not StatusError)
		skipNext           string
	)
	for i, up := range sorted {
		if skipNext != "" && up.Name == skipNext {
			skipNext = ""
			continue
		}
		if c.blocker.isBlocked(up.Name) {
			c.syncBlocked(ref.Protocol, up.Name)
			// Record the skip. A mirror whose breaker is open never produces an
			// error of its own, so without this the summary shows only the origin's
			// timeout and reads as "the origin is down" — when the actual story is
			// "the mirror we rely on is circuit-broken and the origin is the only
			// path left". That is exactly how a CN cluster ends up with every Hub
			// blob GET returning 502.
			attempts = append(attempts, attemptNote{
				Upstream: up.Name,
				Err:      errCircuitOpen,
			})
			continue
		}
		tried++
		remainingAfter := len(sorted) - 1 - i
		budget := c.attemptBudget(statusErr, remainingAfter, up, priorTransportFail)

		var (
			body      io.ReadCloser
			meta      artifact.UpstreamMeta
			latency   time.Duration
			transient bool
			err       error
			winner    Upstream
		)
		winner = up
		if hedgeEligible(ref, remainingAfter, sorted, i, c.blocker) {
			body, meta, latency, transient, err, winner = c.tryFetchHedged(
				ctx, ref, up, sorted[i+1], nil, ropts, budget, remainingAfter,
			)
			if err == nil && winner.Name != up.Name {
				skipNext = winner.Name
				metrics.RecordUpstreamFailover(ref.Protocol, up.Name, "hedge_lost")
			}
		} else {
			body, meta, latency, transient, err = c.tryFetch(
				ctx, ref, up, nil, ropts, budget, remainingAfter,
			)
		}
		if err == nil {
			c.noteSuccess(winner.Name, latency)
			metrics.RecordUpstreamLatency(ref.Protocol, winner.Name, latency.Seconds())
			c.syncBlocked(ref.Protocol, winner.Name)
			// Pin this upstream and wrap for Range resume / cross-upstream fallthrough.
			if body != nil {
				body = c.wrapResuming(ctx, ref, winner, sorted, ropts, body)
			}
			return body, meta, nil
		}
		// Only abort the whole chain when OUR parent context is done.
		// Per-upstream dial / TLS / header deadlines also surface as
		// context.DeadlineExceeded (DaoCloud→R2 CDN dial timeout) — those
		// must fall through to the next mirror, not kill Fetch.
		if isContextError(err) && ctx.Err() != nil {
			return nil, artifact.UpstreamMeta{}, err
		}
		c.noteFailure(up.Name, err, transient)
		if isDialClassError(err) {
			// Weight dial/TLS death higher toward open — CB already counted once.
			c.blocker.hub.recordFailure(up.Name)
		}
		c.syncBlocked(ref.Protocol, up.Name)
		metrics.RecordUpstreamFailover(ref.Protocol, up.Name, failoverReason(err))
		lastErr = err
		attempts = append(attempts, attemptNote{Upstream: up.Name, Err: err})
		rememberStatusErr(&statusErr, err)
		if statusErr == nil {
			priorTransportFail = true
		}
	}
	if tried == 0 {
		return nil, artifact.UpstreamMeta{}, errors.New("upstream: all upstreams are blocked")
	}
	return nil, artifact.UpstreamMeta{}, resolveFetchError(statusErr, lastErr, attempts)
}

// attemptBudget returns how many HTTP attempts a not-yet-tried upstream deserves.
//
// Normally that is c.maxAttempts (initial try + retries), because a transient
// failure on the ONLY path to an artifact is worth recovering. But:
//
//   - Once an EARLIER upstream has already given a definitive answer
//     (statusErr != nil — e.g. goproxy.cn said 404), a later upstream is worth
//     exactly ONE attempt: enough to catch a clean 200, no retries that would
//     multiply a dead origin's latency (CN: proxy.golang.org × 3 × ~10 s).
//   - When MORE mirrors remain after this one (remainingAfter > 0), allow TWO
//     attempts so a single 5xx/429 can recover — but dial/TLS/header timeouts
//     fail-fast (see runFetchAttempt failFastDial) so a dead CDN still yields
//     in one short dial, not maxAttempts × 30s.
//   - On the LAST hop, if it is Official and earlier mirrors already
//     transport-failed (CN: unreachable Hub/pkg.dev), compress to ONE attempt
//     instead of burning the full budget on a GFW-dead origin.
//
// This never suppresses a real 200: an upstream that HAS the artifact and
// answers on its first attempt still wins outright (served > definitive-not-found).
func (c *fallbackClient) attemptBudget(
	statusErr *StatusError,
	remainingAfter int,
	up Upstream,
	priorTransportFail bool,
) int {
	if statusErr != nil {
		return 1
	}
	if remainingAfter > 0 {
		// Soft retry for 5xx/429 only when the client allows retries at all.
		// Dial-class still fail-fast via runFetchAttempt(failFastDial).
		if c.maxAttempts < 2 {
			return 1
		}
		return 2
	}
	if up.Official && priorTransportFail {
		return 1
	}
	return c.maxAttempts
}

// rememberStatusErr records the FIRST definitive upstream status (*StatusError)
// seen while iterating the chain. A later upstream's transport failure must never
// erase this authoritative answer, so once set it is never overwritten.
func rememberStatusErr(dst **StatusError, err error) {
	if *dst != nil {
		return
	}
	var se *StatusError
	if errors.As(err, &se) {
		*dst = se
	}
}

// resolveFetchError picks the error a fully-failed chain returns, encoding the
// precedence "served > definitive-not-found > transport-unknown":
//
//   - A 200 from any upstream already returned before this is reached, so it is
//     never in play here (served wins outright).
//   - A definitive upstream status (4xx: 404/410/403/…) is an AUTHORITATIVE answer
//     — "this artifact does not exist / is refused" — and must win over any later
//     upstream's transport failure, which only means "I don't know". Returning it
//     lets the gomod handler preserve the 404/410 the go client needs for its
//     module-path-boundary walk (PRD §G5, §7.4) instead of flattening to 502.
//   - A pure transport failure (DNS / timeout / connection refused) with no
//     definitive answer anywhere in the chain stays a plain wrapped error, which
//     carries NO StatusError and so keeps mapping to 502: a genuine outage must
//     never be reported as a fake "does not exist" the client would cache.
//
// The StatusError is wrapped (fmt.Errorf %w) so errors.As recovers it while the
// message stays consistent with the transport case.
// errCircuitOpen marks an upstream the chain skipped because its breaker is open.
var errCircuitOpen = errors.New("skipped: circuit breaker open")

// attemptNote records why one upstream in the chain failed.
type attemptNote struct {
	Upstream string
	Err      error
}

// summariseAttempts renders "daocloud: …; dockerhub: …" for the error message.
//
// "all upstreams failed … last error: <the origin timed out>" named only the LAST
// hop, which in CN is the official upstream nothing can reach — so every failure
// read as "the origin is down" and the mirror's actual reason (403, 404, blocked,
// TLS) was invisible. Chasing the wrong upstream costs a debugging session; this
// is one line of output.
func summariseAttempts(attempts []attemptNote) string {
	if len(attempts) == 0 {
		return ""
	}
	parts := make([]string, 0, len(attempts))
	for _, a := range attempts {
		msg := "nil"
		if a.Err != nil {
			msg = a.Err.Error()
		}
		parts = append(parts, a.Upstream+": "+msg)
	}
	return " [tried " + strings.Join(parts, "; ") + "]"
}

func resolveFetchError(statusErr *StatusError, lastErr error, attempts []attemptNote) error {
	if statusErr != nil {
		return fmt.Errorf("upstream: all upstreams failed:%s %w", summariseAttempts(attempts), statusErr)
	}
	return fmt.Errorf("upstream: all upstreams failed:%s %w", summariseAttempts(attempts), lastErr)
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
	sorted := c.chain(upstreams)
	var (
		lastErr            error
		statusErr          *StatusError  // first DEFINITIVE upstream status (see resolveFetchError)
		attempts           []attemptNote // one per failed upstream, for the error message
		tried              int
		priorTransportFail bool
	)
	for i, up := range sorted {
		if c.blocker.isBlocked(up.Name) {
			c.syncBlocked(ref.Protocol, up.Name)
			attempts = append(attempts, attemptNote{Upstream: up.Name, Err: errCircuitOpen})
			continue
		}
		tried++
		remainingAfter := len(sorted) - 1 - i
		budget := c.attemptBudget(statusErr, remainingAfter, up, priorTransportFail)
		body, meta, latency, transient, err := c.tryFetch(
			ctx, ref, up, &prev, ropts, budget, remainingAfter,
		)
		if err == nil {
			c.noteSuccess(up.Name, latency)
			// A 304 is observed here too: it is a real upstream round trip and
			// is precisely the traffic a cache "hit" can hide (see
			// metrics/cacheoutcome.go), so it must be visible in the histogram.
			metrics.RecordUpstreamLatency(ref.Protocol, up.Name, latency.Seconds())
			c.syncBlocked(ref.Protocol, up.Name)
			if meta.StatusCode == http.StatusNotModified {
				return nil, meta, true, nil
			}
			if body != nil {
				body = c.wrapResuming(ctx, ref, up, sorted, ropts, body)
			}
			return body, meta, false, nil
		}
		if isContextError(err) && ctx.Err() != nil {
			return nil, artifact.UpstreamMeta{}, false, err
		}
		c.noteFailure(up.Name, err, transient)
		if isDialClassError(err) {
			c.blocker.hub.recordFailure(up.Name)
		}
		c.syncBlocked(ref.Protocol, up.Name)
		metrics.RecordUpstreamFailover(ref.Protocol, up.Name, failoverReason(err))
		lastErr = err
		attempts = append(attempts, attemptNote{Upstream: up.Name, Err: err})
		rememberStatusErr(&statusErr, err)
		if statusErr == nil {
			priorTransportFail = true
		}
	}
	if tried == 0 {
		return nil, artifact.UpstreamMeta{}, false,
			errors.New("upstream: all upstreams are blocked")
	}
	return nil, artifact.UpstreamMeta{}, false, resolveFetchError(statusErr, lastErr, attempts)
}

// tryFetch performs up to maxAttempts GET requests against a single upstream
// under failsafe-go Retry + CircuitBreaker.
//
// maxAttempts is chosen by the caller (see attemptBudget). remainingAfter
// selects the fail-fast HTTP client and dial-class no-retry policy.
// prev, when non-nil, adds conditional GET headers. The bearer-token dance
// runs inside a single attempt (tryOnce) without consuming a retry slot.
//
// Returns (body, meta, latency, transient, error).
func (c *fallbackClient) tryFetch(
	ctx context.Context,
	ref artifact.ArtifactRef,
	up Upstream,
	prev *artifact.UpstreamMeta,
	opts requestOpts,
	maxAttempts int,
	remainingAfter int,
) (io.ReadCloser, artifact.UpstreamMeta, time.Duration, bool, error) {
	// Per-upstream: a proxy configured on THIS upstream (typically the official
	// origin, which the chain reaches only after every mirror has failed) is used
	// for it alone. Mirrors keep dialling direct — see proxy.go for why that
	// matters on a metered proxy.
	hc := c.httpForUpstream(up, remainingAfter)
	failFastDial := remainingAfter > 0
	attempt, transient, err := c.blocker.hub.runFetchAttempt(
		ctx, up.Name, maxAttempts, c.backoffBase, failFastDial,
		func(actx context.Context) (fetchAttempt, error) {
			body, meta, lat, e := c.tryOnce(actx, hc, ref, up, prev, opts)
			if e != nil {
				return fetchAttempt{}, e
			}
			return fetchAttempt{Body: body, Meta: meta, Latency: lat}, nil
		},
	)
	if err != nil {
		return nil, artifact.UpstreamMeta{}, 0, transient, err
	}
	return attempt.Body, attempt.Meta, attempt.Latency, false, nil
}

// tryOnce performs a single HTTP GET (plus optional in-attempt bearer dance)
// against one upstream. Transient failures are wrapped with asTransient so
// failsafe Retry/CB HandleIf can classify them without string matching.
func (c *fallbackClient) tryOnce(
	ctx context.Context,
	hc *http.Client,
	ref artifact.ArtifactRef,
	up Upstream,
	prev *artifact.UpstreamMeta,
	opts requestOpts,
) (io.ReadCloser, artifact.UpstreamMeta, time.Duration, error) {
	if hc == nil {
		hc = c.http
	}
	rawURL := buildURL(up, ref)
	var (
		authToken    string
		didAuthRetry bool
	)
	if ref.Protocol == "oci" {
		scope := "repository:" + ociFetchName(up, ref.Name) + ":pull"
		if tok := c.getCachedToken(up.Name, scope); tok != "" {
			authToken = tok
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, artifact.UpstreamMeta{}, 0, err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, artifact.UpstreamMeta{}, 0,
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
		if opts.accept != "" {
			req.Header.Set("Accept", opts.accept)
		}
		if opts.hasRange {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", opts.rangeStart))
		}
		if authToken != "" {
			req.Header.Set("Authorization", "Bearer "+authToken)
		}

		started := time.Now()
		resp, doErr := hc.Do(req)
		latency := time.Since(started)
		if doErr != nil {
			// Parent fill/caller ctx done → stop. A per-upstream dial/TLS/
			// ResponseHeaderTimeout also looks like DeadlineExceeded; keep
			// retrying / falling through while parent ctx is still live
			// (CN: DaoCloud blob CDN dial timeout must not abort the chain).
			if isContextError(doErr) && ctx.Err() != nil {
				return nil, artifact.UpstreamMeta{}, 0, doErr
			}
			return nil, artifact.UpstreamMeta{}, 0,
				asTransient(fmt.Errorf("upstream %s: %w", up.Name, doErr))
		}

		meta := extractMeta(resp, up.Name)

		switch {
		case resp.StatusCode == http.StatusNotModified:
			_ = resp.Body.Close()
			return nil, meta, latency, nil

		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			return resp.Body, meta, latency, nil

		case resp.StatusCode == http.StatusUnauthorized && !didAuthRetry:
			_ = resp.Body.Close()
			didAuthRetry = true
			wwwAuth := resp.Header.Get("WWW-Authenticate")
			tok, authErr := c.getOrFetchToken(ctx, wwwAuth, up)
			if authErr != nil {
				return nil, meta, 0,
					fmt.Errorf("upstream %s: HTTP 401 unauthorized: %w", up.Name, authErr)
			}
			authToken = tok
			continue

		case resp.StatusCode == http.StatusTooManyRequests:
			_ = resp.Body.Close()
			return nil, meta, 0, asTransient(
				fmt.Errorf("upstream %s: HTTP 429 (rate limited)", up.Name))

		case resp.StatusCode >= 500:
			_ = resp.Body.Close()
			return nil, meta, 0, asTransient(
				fmt.Errorf("upstream %s: HTTP %d", up.Name, resp.StatusCode))

		default:
			_ = resp.Body.Close()
			return nil, meta, 0,
				&StatusError{Upstream: up.Name, StatusCode: resp.StatusCode}
		}
	}
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
//
// Callers that walk a multi-mirror chain MUST also check ctx.Err(): a
// per-upstream dial / TLS / header timeout is often wrapped as
// context.DeadlineExceeded even when the parent fill context is still live.
// Aborting the whole chain on that shape is what left CN nodes hung on
// DaoCloud's R2 CDN with a working 1ms mirror never tried.
func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// buildURL constructs the fetch URL from an upstream and an ArtifactRef.
// The path structure is protocol-specific; see buildPath. For OCI, a non-empty
// upstream PathPrefix is inserted after /v2/ (Huawei SWR nested layout).
func buildURL(up Upstream, ref artifact.ArtifactRef) string {
	base := strings.TrimRight(up.BaseURL, "/")
	path := buildPath(ref, up)
	if path == "" {
		return base
	}
	return base + "/" + path
}

// ociFetchName returns the repository path used on this upstream (with optional
// PathPrefix). Cache keys and ArtifactRef.Name stay unprefixed; only the
// wire URL / bearer scope use the prefixed name.
func ociFetchName(up Upstream, repo string) string {
	repo = strings.Trim(repo, "/")
	prefix := strings.Trim(strings.TrimSpace(up.PathPrefix), "/")
	if prefix == "" {
		return repo
	}
	if repo == "" {
		return prefix
	}
	return prefix + "/" + repo
}

// buildPath derives the URL path component from an ArtifactRef following
// ecosystem conventions. Protocol handlers are responsible for populating
// the relevant ref fields correctly before calling Fetch / Revalidate.
func buildPath(ref artifact.ArtifactRef, up Upstream) string {
	switch ref.Protocol {
	case "oci":
		name := ociFetchName(up, ref.Name)
		// Mutable (tag) or unresolved → manifest by tag/reference.
		// Immutable (resolved digest) → blob by digest.
		if ref.Mutable || ref.Digest == "" {
			return "v2/" + name + "/manifests/" + ref.Version
		}
		return "v2/" + name + "/blobs/" + ref.Digest

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
		// Warehouse JSON (/pypi/<project>/json) carries upload_time_iso_8601
		// for the maturity cool-down gate. Version sentinel "json" selects it.
		if ref.Mutable && ref.Version == "json" {
			return "pypi/" + ref.Name + "/json"
		}
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
			if ref.Name == "" {
				return "index.yaml"
			}
			return ref.Name + "/index.yaml"
		}
		if ref.Name == "" {
			return ref.Version
		}
		return ref.Name + "/" + ref.Version

	case "git":
		return ref.Name + "/info/refs"

	case "tarball":
		if ref.Digest != "" {
			return ref.Name + "/" + ref.Digest
		}
		return ref.Name + "/" + ref.Version

	case "cargo":
		// Sparse index: Name is the path under index.crates.io (config.json, li/bc/libc).
		// Crate download: static.crates.io/crates/{name}/{name}-{version}.crate
		if ref.Mutable {
			return ref.Name
		}
		return "crates/" + ref.Name + "/" + ref.Name + "-" + ref.Version + ".crate"

	case "conda":
		// Name = "<channel>/<subdir>/…path" relative to channel root (or full
		// relative path including channel when BaseURL is the channel hub root).
		return ref.Name

	case "hf":
		// Name is the Hub-relative path (api/…, models/…, resolve/…).
		return ref.Name

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
