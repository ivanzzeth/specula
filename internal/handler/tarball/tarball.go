// Package tarball implements the generic URL-keyed, content-addressed tarball
// download cache. Supported route (relative to the handler's mount prefix):
//
//	GET/HEAD /<host>/<path...>/<file>   — immutable: a generic remote download
//
// # immutable, content-addressed (DESIGN-REVIEW §3)
//
// Every request maps a remote URL (reconstructed from the path) to a single
// sha256 digest. There is NO mutable layer: a URL either resolves to bytes that
// are streamed through the quarantine/verify-on-write pipeline and promoted to
// the permanent CAS tier, or it is a cache miss and re-fetched. The digest is
// computed while streaming; TOFU pins it on first fetch and alerts on change.
//
// # host allowlist
//
// Because the path encodes an arbitrary upstream location, WithAllowedHosts
// gates which hosts may be proxied (SSRF guard). A request whose host is not
// allowed is rejected (403).
//
// # verify-on-write and optional digest pin
//
// Callers may supply an expected digest via the "digest" query parameter
// (e.g. ?digest=sha256:abc…). The pin is optional; unpinned callers are
// unaffected. When provided it is an integrity assertion — "serve me these
// bytes or fail" — and it is enforced on BOTH paths:
//
//   - cache MISS: the ChecksumVerifier compares the pin against the
//     streaming-computed digest during verify-on-write. A mismatch removes the
//     quarantine file and the handler returns 502.
//   - cache HIT: the CAS is keyed by (protocol, name, version), NOT by digest,
//     so the entry found for this URL may hold any digest. cache.Lookup
//     compares the pin against the entry's digest and returns a
//     *cache.PinMismatchError, which the handler maps to the same 502.
//
// Enforcing only the miss path — as this handler originally did — means the
// assertion works in testing and silently stops working once the cache is warm,
// i.e. almost always in production.
//
// The hit-path check is a metadata comparison, not a re-hash: stored bytes are
// still trusted exactly as ARCHITECTURE §3 specifies ("CAS 永久缓存, 绝不重验").
// A mismatched pin never evicts or invalidates the cached entry, so a bad-faith
// pin cannot be used as a cache-denial lever, and it is answered without a new
// upstream fetch, so it cannot be used to amplify load onto the upstream.
//
// Without a pin the TOFU verifier records the first-seen digest and alerts on
// later changes.
//
// # scheme
//
// The upstream URL is reconstructed as <scheme>://<host>/<path>/<file>.
// The default scheme is "https". Use WithScheme("http") for tests or
// deployments behind an HTTP load balancer.
package tarball

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
	"github.com/ivanzzeth/specula/internal/metrics"
	"github.com/ivanzzeth/specula/internal/store/meta"
	"github.com/ivanzzeth/specula/internal/upstream"
)

// Protocol is the ArtifactRef.Protocol value for generic tarballs.
const Protocol = "tarball"

// ttlNeverRevalidate / ttlAlwaysRevalidate mirror the config sentinel values.
// tarball content is immutable once cached (never revalidate).
const (
	ttlNeverRevalidate  int64 = -1
	ttlAlwaysRevalidate int64 = 0
)

// defaultScheme is the URL scheme used when reconstructing upstream fetch URLs.
const defaultScheme = "https"

// defaultHTTPTimeout is the per-request deadline for the package-level HTTP
// client used when no upstream.Client is configured. Large release assets and
// slow GitHub/CDN paths need more than a short deadline — EOF under 30s was
// observed on cold pulls of modest archives.
const defaultHTTPTimeout = 120 * time.Second

// tarballFetchAttempts is how many times fetchFromURL retries transient
// upstream failures (EOF, timeout, 5xx) before surfacing 502 to the client.
const tarballFetchAttempts = 3

// pkgHTTPClient is the package-level client for direct tarball fetches. Using
// a package-level client reuses connection pools across requests.
var pkgHTTPClient = &http.Client{Timeout: defaultHTTPTimeout}

// tarballUserAgent identifies Specula to upstreams that reject empty/default Go UAs.
const tarballUserAgent = "specula-tarball/1 (+https://github.com/ivanzzeth/specula)"


// Handler serves the generic tarball data-plane API. All bytes served are
// guaranteed to have passed the verify-on-write chain inside the CacheManager
// (fix C2).
type Handler struct {
	cache         cache.CacheManager
	meta          meta.MetadataStore  // optional: direct metadata access
	upstreamClt   upstream.Client     // optional: retained for API symmetry; not used for URL construction
	upstreams     []upstream.Upstream // optional: retained for API symmetry
	pathPrefix    string              // mount prefix trimmed before routing
	mutableTTLSec int64               // retained for option symmetry (tarball is immutable)
	quarantineDir string              // directory for on-disk quarantine temp files
	scheme        string              // URL scheme for upstream fetches ("http" or "https")

	// allowedHosts gates which upstream hosts may be proxied (SSRF guard).
	// Empty = allow none (fail closed).
	allowedHosts []string

	log *slog.Logger
}

// Option is a functional option applied to Handler during construction.
type Option func(*Handler)

// WithMeta injects a MetadataStore for direct metadata access.
func WithMeta(m meta.MetadataStore) Option {
	return func(h *Handler) { h.meta = m }
}

// WithUpstream configures the fallback upstream client and ordered mirror list.
// For tarball, the upstream URL is encoded in the request path itself; this
// option is retained for API symmetry with other handlers.
func WithUpstream(c upstream.Client, ups []upstream.Upstream) Option {
	return func(h *Handler) { h.upstreamClt = c; h.upstreams = ups }
}

// WithMutableTTL is retained for option symmetry with the other handlers.
// tarball content is immutable; this only affects any negative-cache tuning the
// leaf implementation may add.
func WithMutableTTL(secs int64) Option {
	return func(h *Handler) { h.mutableTTLSec = secs }
}

// WithPathPrefix sets a mount prefix stripped from the request path before routing.
func WithPathPrefix(prefix string) Option {
	return func(h *Handler) { h.pathPrefix = strings.TrimRight(prefix, "/") }
}

// WithQuarantineDir sets the directory used for on-disk quarantine files.
func WithQuarantineDir(dir string) Option {
	return func(h *Handler) { h.quarantineDir = dir }
}

// WithLogger injects a structured logger.
func WithLogger(l *slog.Logger) Option {
	return func(h *Handler) { h.log = l }
}

// WithAllowedHosts sets the upstream host allowlist (SSRF guard). A request whose
// host is not listed is rejected with 403. An empty list denies all hosts.
// Known CDN aliases for configured forges (e.g. github.com → codeload.github.com)
// are expanded so path hosts that match the forge still succeed after redirects
// land on sibling hostnames (allowlist is checked on the request path host only;
// expansion keeps intentional CDN hosts usable if callers encode them).
func WithAllowedHosts(hosts []string) Option {
	return func(h *Handler) { h.allowedHosts = expandTarballAllowedHosts(hosts) }
}

// expandTarballAllowedHosts adds well-known CDN siblings for forge hosts.
func expandTarballAllowedHosts(hosts []string) []string {
	seen := make(map[string]struct{}, len(hosts)*2)
	out := make([]string, 0, len(hosts)*2)
	add := func(h string) {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			return
		}
		if _, ok := seen[h]; ok {
			return
		}
		seen[h] = struct{}{}
		out = append(out, h)
	}
	for _, h := range hosts {
		add(h)
		switch strings.ToLower(h) {
		case "github.com":
			add("codeload.github.com")
			add("objects.githubusercontent.com")
			add("release-assets.githubusercontent.com")
			add("github-releases.githubusercontent.com")
			add("raw.githubusercontent.com")
		case "gitlab.com":
			add("cdn.artifacts.gitlab-static.net")
		}
	}
	return out
}

// WithScheme sets the URL scheme used when constructing upstream fetch URLs.
// Valid values are "http" and "https". Defaults to "https".
// Use "http" for in-process tests backed by httptest.Server.
func WithScheme(scheme string) Option {
	return func(h *Handler) { h.scheme = scheme }
}

// NewHandler constructs a tarball Handler backed by the given CacheManager.
func NewHandler(cm cache.CacheManager, opts ...Option) *Handler {
	h := &Handler{
		cache:         cm,
		mutableTTLSec: ttlNeverRevalidate, // immutable once cached
		scheme:        defaultScheme,
		log:           slog.Default(),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Compile-time assertion.
var _ http.Handler = (*Handler)(nil)

// ServeHTTP dispatches generic tarball requests. The path after the mount prefix
// is the URL key: /<host>/<path...>/<file>.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !allowGetHead(w, r) {
		return
	}

	p := r.URL.Path
	if h.pathPrefix != "" {
		p = strings.TrimPrefix(p, h.pathPrefix)
	}
	rest := strings.TrimLeft(p, "/")
	if rest == "" || strings.Contains(rest, "..") {
		writeError(w, http.StatusNotFound, "not a tarball path")
		return
	}

	host, key, file, ok := splitURLKey(rest)
	if !ok {
		writeError(w, http.StatusNotFound, "tarball path must be /<host>/<path>/<file>")
		return
	}
	h.serveTarball(w, r, host, key, file)
}

// --------------------------------------------------------------------------
// Routing helpers
// --------------------------------------------------------------------------

// splitURLKey splits "<host>/<path...>/<file>" into the upstream host, the CAS
// Name key (host + "/" + directory), and the final file component. The host is
// returned separately so the allowlist guard can inspect it.
func splitURLKey(rest string) (host, key, file string, ok bool) {
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 {
		return "", "", "", false
	}
	host = rest[:slash]
	i := strings.LastIndexByte(rest, '/')
	if i == len(rest)-1 {
		return "", "", "", false
	}
	// key is the full directory portion (including host) so distinct upstreams
	// with identical file names never collide in the CAS metadata.
	key = rest[:i]
	file = rest[i+1:]
	return host, key, file, true
}

// --------------------------------------------------------------------------
// ArtifactRef mapping
// --------------------------------------------------------------------------

// tarballRef builds the immutable ref for a generic download. buildPath rebuilds
// the fetch path as Name + "/" + Version (or Name + "/" + Digest once resolved).
// Mutable is always false: tarball content is content-addressed and permanent.
func tarballRef(key, file string) artifact.ArtifactRef {
	return artifact.ArtifactRef{
		Protocol: Protocol,
		Name:     key,
		Version:  file,
		Mutable:  false,
	}
}

// --------------------------------------------------------------------------
// Core endpoint — immutable, CAS, verify-on-write
// --------------------------------------------------------------------------

// serveTarball handles GET/HEAD /<host>/<path>/<file>.
//
// Pipeline:
//  1. SSRF guard — 403 if host is not in the allowlist.
//  2. Optional digest pin — "digest" query param sets ref.Digest for verify-on-write.
//  3. Fast path — CAS lookup; serve immediately if hit.
//  4. Miss path — fetch → quarantine → verify-on-write → CAS promotion → serve.
func (h *Handler) serveTarball(w http.ResponseWriter, r *http.Request, host, key, file string) {
	// 1. SSRF guard.
	if !h.isAllowedHost(host) {
		writeError(w, http.StatusForbidden, "tarball: host not in allowed list")
		return
	}

	ctx := r.Context()
	ref := tarballRef(key, file)

	// 2. Optional caller-supplied digest pin (enables strict verify-on-write).
	// Example: GET /host/path/file?digest=sha256:abc…
	if pin := r.URL.Query().Get("digest"); pin != "" {
		ref.Digest = pin
	}

	// 3. Fast path: verified CAS hit.
	entry, err := h.cache.Lookup(ctx, ref)
	if err != nil {
		// A caller-supplied pin that contradicts the cached entry is the
		// client's assertion failing, not a server fault — and it must fail the
		// same way warm as it does cold, where the verify chain rejects the
		// mismatch with 502. Reported here without re-fetching upstream: the
		// answer is already known, and re-fetching would let any stranger with a
		// bogus ?digest= turn a warm hit into upstream load. The cached entry is
		// deliberately left intact — a bad-faith pin is not an eviction lever.
		if pe, ok := cache.AsPinMismatchError(err); ok {
			h.log.Warn("tarball: digest pin mismatch on cache hit",
				"key", key, "file", file, "pinned", pe.Want, "cached", pe.Got)
			writeError(w, http.StatusBadGateway,
				"digest pin mismatch: requested "+pe.Want+", cache holds "+pe.Got)
			return
		}
		h.log.Error("tarball: CAS lookup", "key", key, "file", file, "err", err)
		writeError(w, http.StatusInternalServerError, "cache lookup failed")
		return
	}
	if entry != nil {
		// CAS hit: the body comes from cache, no upstream body transfer.
		metrics.MarkHit(ctx)
		h.serveFromCache(w, r, ref, entry)
		return
	}

	// 4. Cache miss — fetch → quarantine → verify-on-write → promote → serve.
	rc, umeta, fetchErr := h.fetchFromURL(ctx, key, file)
	if fetchErr != nil {
		h.log.Error("tarball: upstream fetch", "key", key, "file", file, "err", fetchErr)
		if upstream.IsNotFound(fetchErr) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		writeError(w, http.StatusBadGateway, "upstream fetch failed: "+fetchErr.Error())
		return
	}
	defer rc.Close()

	art, cleanup, qErr := cache.Quarantine(ctx, h.quarantineDir, rc, umeta)
	if qErr != nil {
		h.log.Error("tarball: quarantine", "key", key, "file", file, "err", qErr)
		writeError(w, http.StatusInternalServerError, "quarantine failed")
		return
	}

	entry, storeErr := h.cache.Store(ctx, ref, art)
	if storeErr != nil {
		cleanup()
		h.log.Error("tarball: verify-on-write", "key", key, "file", file, "err", storeErr)
		writeError(w, http.StatusBadGateway, "verify-on-write failed: "+storeErr.Error())
		return
	}

	// Cache miss: the body was fetched from the upstream URL and promoted to CAS.
	metrics.MarkMiss(ctx)
	h.serveFromCache(w, r, ref, entry)
}

// --------------------------------------------------------------------------
// Fetch helper — direct HTTP
// --------------------------------------------------------------------------

// fetchFromURL reconstructs the upstream URL as <scheme>://<key>/<file> and
// performs a direct HTTP GET (with short retries on transient failures). The
// response body is returned as a streaming io.ReadCloser; the caller is
// responsible for closing it.
//
// "https" is used by default; override with WithScheme("http") for tests.
func (h *Handler) fetchFromURL(ctx context.Context, key, file string) (io.ReadCloser, artifact.UpstreamMeta, error) {
	scheme := h.scheme
	if scheme == "" {
		scheme = defaultScheme
	}
	rawURL := scheme + "://" + key + "/" + file

	var lastErr error
	for attempt := 1; attempt <= tarballFetchAttempts; attempt++ {
		rc, umeta, err := h.doFetchOnce(ctx, rawURL, key)
		if err == nil {
			return rc, umeta, nil
		}
		lastErr = err
		if !isTransientTarballFetchErr(err) || attempt == tarballFetchAttempts {
			break
		}
		h.log.Warn("tarball: transient upstream fetch, retrying",
			"url", rawURL, "attempt", attempt, "err", err)
		select {
		case <-ctx.Done():
			return nil, artifact.UpstreamMeta{}, ctx.Err()
		case <-time.After(time.Duration(attempt) * 200 * time.Millisecond):
		}
	}
	return nil, artifact.UpstreamMeta{}, lastErr
}

func (h *Handler) doFetchOnce(ctx context.Context, rawURL, key string) (io.ReadCloser, artifact.UpstreamMeta, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, artifact.UpstreamMeta{}, fmt.Errorf("tarball: build request %q: %w", rawURL, err)
	}
	req.Header.Set("User-Agent", tarballUserAgent)
	req.Header.Set("Accept", "*/*")

	resp, err := pkgHTTPClient.Do(req)
	if err != nil {
		return nil, artifact.UpstreamMeta{}, fmt.Errorf("tarball: fetch %q: %w", rawURL, err)
	}

	switch {
	case resp.StatusCode == http.StatusNotFound:
		_ = resp.Body.Close()
		return nil, artifact.UpstreamMeta{}, fmt.Errorf("tarball: %q: not found (404)", rawURL)
	case resp.StatusCode == http.StatusBadGateway ||
		resp.StatusCode == http.StatusServiceUnavailable ||
		resp.StatusCode == http.StatusGatewayTimeout:
		_ = resp.Body.Close()
		return nil, artifact.UpstreamMeta{}, fmt.Errorf("tarball: %q: HTTP %d", rawURL, resp.StatusCode)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		_ = resp.Body.Close()
		return nil, artifact.UpstreamMeta{}, fmt.Errorf("tarball: %q: HTTP %d", rawURL, resp.StatusCode)
	}

	umeta := artifact.UpstreamMeta{
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		ContentType:  resp.Header.Get("Content-Type"),
		StatusCode:   resp.StatusCode,
		Upstream:     key,
	}
	return resp.Body, umeta, nil
}

func isTransientTarballFetchErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "eof"),
		strings.Contains(msg, "timeout"),
		strings.Contains(msg, "temporar"),
		strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "http 502"),
		strings.Contains(msg, "http 503"),
		strings.Contains(msg, "http 504"):
		return true
	default:
		return false
	}
}

// --------------------------------------------------------------------------
// Serve helper — CAS blob → HTTP response
// --------------------------------------------------------------------------

// serveFromCache reads the verified blob from the CAS and writes it to w.
// entry is used to supply Content-Length and ETag when available; the actual
// bytes come from h.cache.Serve (only verified, promoted blobs are returned).
func (h *Handler) serveFromCache(w http.ResponseWriter, r *http.Request, ref artifact.ArtifactRef, entry *artifact.CacheEntry) {
	ctx := r.Context()
	rc, cacheEntry, err := h.cache.Serve(ctx, ref, 0, -1)
	if err != nil {
		switch pe, isPin := cache.AsPinMismatchError(err); {
		case isPin:
			// Serve routes through Lookup, so the pin gate applies here too.
			// Map it to the same 502 rather than letting it read as a server
			// fault; reaching this branch means the entry changed between our
			// Lookup and this call.
			h.log.Warn("tarball: digest pin mismatch on serve",
				"ref", ref, "pinned", pe.Want, "cached", pe.Got)
			writeError(w, http.StatusBadGateway,
				"digest pin mismatch: requested "+pe.Want+", cache holds "+pe.Got)
		case errors.Is(err, cache.ErrCacheMiss):
			writeError(w, http.StatusNotFound, "tarball: artifact not in cache")
		default:
			h.log.Error("tarball: serve from cache", "ref", ref, "err", err)
			writeError(w, http.StatusInternalServerError, "cache serve failed")
		}
		return
	}
	if rc == nil {
		writeError(w, http.StatusNotFound, "tarball: artifact not in cache")
		return
	}
	defer rc.Close()

	// Prefer size from the CacheEntry returned by Serve (post-lookup),
	// falling back to the entry supplied by the caller (pre-lookup).
	var size int64
	if cacheEntry != nil && cacheEntry.Size > 0 {
		size = cacheEntry.Size
	} else if entry != nil && entry.Size > 0 {
		size = entry.Size
	}

	ct := "application/octet-stream"
	if entry != nil {
		if entry.ETag != "" {
			w.Header().Set("ETag", entry.ETag)
		}
	}
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(http.StatusOK)

	if r.Method == http.MethodGet {
		_, _ = io.Copy(w, rc)
	}
}

// --------------------------------------------------------------------------
// Allowlist guard
// --------------------------------------------------------------------------

// isAllowedHost reports whether host is in the configured allowedHosts list.
// Returns false (deny) when the list is empty — fail-closed SSRF posture.
func (h *Handler) isAllowedHost(host string) bool {
	for _, allowed := range h.allowedHosts {
		if allowed == host {
			return true
		}
	}
	return false // empty allowedHosts → deny all
}

// --------------------------------------------------------------------------
// Shared helpers
// --------------------------------------------------------------------------

// allowGetHead enforces GET/HEAD-only semantics, writing 405 otherwise.
func allowGetHead(w http.ResponseWriter, r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return true
	default:
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return false
	}
}

// writeError writes a plain-text error response.
func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(message + "\n"))
}
