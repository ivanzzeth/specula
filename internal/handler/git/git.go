// Package git implements the public git-clone acceleration data-plane handler
// (ARCHITECTURE §9, DESIGN-REVIEW §6). It is a direct port of ai-sandbox
// internal/controlplane/ptc/gitproxy/{proxy,mirror,serve,path,public}.go.
//
// Supported routes (Smart HTTP, relative to the handler's mount prefix):
//
//	GET  /<host>/<project>.git/info/refs?service=git-upload-pack   — ref advertise
//	POST /<host>/<project>.git/git-upload-pack                     — fetch/clone
//	GET  /<host>/<project>.git/info/refs?service=git-receive-pack  — push (bypass)
//	POST /<host>/<project>.git/git-receive-pack                    — push (bypass)
//
// # bare-mirror model (DESIGN-REVIEW §3 / §9)
//
// The cache is an on-disk bare mirror, NOT the CAS blob store: git objects are
// already content-addressed by SHA (immutable), and refs (branch/tag → SHA) are
// the mutable layer with a short staleness window. A per-mirror keyed mutex plus
// the staleness window coalesce concurrent clones into a single `git remote
// update` (stampede protection). Serving is via `git http-backend` (CGI).
//
// # trust boundary
//
//   - host allowlist: a request whose host is not in AllowedUpstreams is 404.
//   - push / Authorization-bearing / private requests are PASSED THROUGH with
//     zero caching (httputil.NewSingleHostReverseProxy), never mirrored.
//   - public-only: only anonymously-readable repos are mirrored; the visibility
//     probe fails closed to passthrough (fail_closed) — the probe-failure window
//     is exactly when an attacker's public copy could win.
//   - TOFU: ref→SHA pins are recorded in MetadataStore; non-fast-forward updates
//     trigger a logged warning (force-push / history-rewrite detection). This is
//     the ceiling a repo reaches with no signed-refs anchor configured — see
//     RepoTier.
//   - signed refs: REACHED when a verify.GitSignedVerifier is configured
//     (WithSignedRefsVerifier). After each sync, updateSignedRefs verifies the
//     signature on every ref tip against the allowed-signers anchor; a ref that
//     verifies earns the `signed` tier (a per-ref pin + a signed/pass series on
//     /metrics), fulfilling PRD §G2 ("签名 tag/commit（配 allowed-signers）；否则
//     tofu"). A ref with no signature stays at tofu (opt-in). Under policy=enforce
//     an untrusted signature fails closed. See signed.go.
package git

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
	"github.com/ivanzzeth/specula/internal/metrics"
	"github.com/ivanzzeth/specula/internal/store/meta"
	"github.com/ivanzzeth/specula/internal/upstream"
	"github.com/ivanzzeth/specula/internal/verify"
)

// Protocol is the ArtifactRef.Protocol value for git.
const Protocol = "git"

// gitSuffix marks the boundary between the repository path and the Smart HTTP
// tail (e.g. "/info/refs").
const gitSuffix = ".git"

// Default bare-mirror settings mirroring the ai-sandbox gitproxy Phase-1 values.
const (
	defaultSyncStaleAfter  = 30 * time.Second
	defaultUpstreamTimeout = 10 * time.Minute
)

// ttlNeverRevalidate / ttlAlwaysRevalidate mirror the config sentinel values.
const (
	ttlNeverRevalidate  int64 = -1
	ttlAlwaysRevalidate int64 = 0
)

// Handler serves the git-clone acceleration data-plane API via a disk bare
// mirror. Unlike the CAS-backed handlers, git objects live in the mirror
// (content-addressed by SHA); the CacheManager/upstream seams below are reserved
// for recording ref→SHA TOFU pins and metadata, not for byte storage.
type Handler struct {
	cache         cache.CacheManager  // optional: reserved for ref-pin metadata
	meta          meta.MetadataStore  // optional: ref→SHA TOFU pins (mutable tier)
	upstreamClt   upstream.Client     // optional: reserved; git uses reverse-proxy passthrough
	upstreams     []upstream.Upstream // optional: reserved
	pathPrefix    string              // mount prefix trimmed before routing
	mutableTTLSec int64               // TTL for refs (mutable); default 30s

	// Bare-mirror settings (ported from ai-sandbox gitproxy Config).
	mirrorDir       string              // on-disk root for bare mirrors
	allowed         map[string]struct{} // host allowlist (AllowedUpstreams)
	syncStaleAfter  time.Duration       // staleness window before a remote update
	upstreamTimeout time.Duration       // upstream operation deadline
	publicOnly      bool                // only mirror anonymously-readable repos
	failClosed      bool                // probe failure → passthrough (never stale)

	// upstreamScheme is the URL scheme used when building upstream URLs.
	// Default "https"; set to "http" in tests pointing at plain httptest.Server.
	upstreamScheme string

	// signedRefs is the optional signed tag/commit verifier (signed tier).
	signedRefs *verify.GitSignedVerifier

	log *slog.Logger

	// runtime-initialized internals (set in NewHandler after options applied).
	mirror    *mirrorStore
	pubCheck  *publicChecker
	transport *http.Transport
}

// Option is a functional option applied to Handler during construction.
type Option func(*Handler)

// WithMeta injects a MetadataStore used to record ref→SHA TOFU pins (mutable
// tier) and detect non-fast-forward (force-push / history rewrite) updates.
func WithMeta(m meta.MetadataStore) Option {
	return func(h *Handler) { h.meta = m }
}

// WithUpstream is retained for option symmetry with the other handlers. git does
// not use the generic fetch client — it reverse-proxies passthrough requests and
// mirrors public repos — so the client is reserved for future use.
func WithUpstream(c upstream.Client, ups []upstream.Upstream) Option {
	return func(h *Handler) { h.upstreamClt = c; h.upstreams = ups }
}

// WithMutableTTL overrides the refs (mutable) TTL in seconds. -1 / 0 sentinels.
func WithMutableTTL(secs int64) Option {
	return func(h *Handler) { h.mutableTTLSec = secs }
}

// WithPathPrefix sets a mount prefix stripped from the request path before routing.
func WithPathPrefix(prefix string) Option {
	return func(h *Handler) { h.pathPrefix = strings.TrimRight(prefix, "/") }
}

// WithLogger injects a structured logger.
func WithLogger(l *slog.Logger) Option {
	return func(h *Handler) { h.log = l }
}

// WithMirrorDir sets the on-disk root for bare mirrors (git objects live here).
func WithMirrorDir(dir string) Option {
	return func(h *Handler) { h.mirrorDir = dir }
}

// WithAllowedUpstreams sets the host allowlist. A request whose host is not
// listed is rejected (404) — never proxied.
func WithAllowedUpstreams(hosts []string) Option {
	return func(h *Handler) {
		h.allowed = make(map[string]struct{}, len(hosts))
		for _, host := range hosts {
			if host = strings.TrimSpace(host); host != "" {
				h.allowed[host] = struct{}{}
			}
		}
	}
}

// WithSyncStaleAfter sets the staleness window before a mirror is re-fetched.
func WithSyncStaleAfter(d time.Duration) Option {
	return func(h *Handler) { h.syncStaleAfter = d }
}

// WithUpstreamTimeout sets the deadline for a mirror clone/update operation.
func WithUpstreamTimeout(d time.Duration) Option {
	return func(h *Handler) { h.upstreamTimeout = d }
}

// WithPublicOnly restricts caching to anonymously-readable repos. Private repos
// and Authorization-bearing requests are passed through with zero caching.
func WithPublicOnly(publicOnly bool) Option {
	return func(h *Handler) { h.publicOnly = publicOnly }
}

// WithFailClosed selects passthrough (rather than serving a stale mirror) when
// the public-visibility probe fails.
func WithFailClosed(failClosed bool) Option {
	return func(h *Handler) { h.failClosed = failClosed }
}

// WithSignedRefsVerifier injects the signed tag/commit verifier. When set, each
// sync runs updateSignedRefs: a ref whose tip carries a valid signature from the
// allowed-signers anchor is lifted to the `signed` tier (RepoTier reports it and
// a signed/pass series appears on /metrics). Under policy=enforce an untrusted
// signature fails the serve closed. Leaving it nil keeps the repo at tofu.
func WithSignedRefsVerifier(v *verify.GitSignedVerifier) Option {
	return func(h *Handler) { h.signedRefs = v }
}

// WithUpstreamScheme overrides the URL scheme used when building upstream URLs.
// Default is "https". Set to "http" in tests that point at plain HTTP servers.
// This option has no effect in production (upstream URLs are always HTTPS).
func WithUpstreamScheme(scheme string) Option {
	return func(h *Handler) { h.upstreamScheme = scheme }
}

// NewHandler constructs a git-clone acceleration Handler. Unlike the CAS-backed
// handlers there is no required CacheManager: git's byte cache is the disk bare
// mirror (WithMirrorDir). Configure the host allowlist with WithAllowedUpstreams.
//
// SPECULA_GIT_UPSTREAM_SCHEME: when set to "http", overrides the upstream URL
// scheme used for git clone --mirror and passthrough requests. Intended only for
// integration-test environments where the upstream is a plain HTTP server.
// Production deployments leave this unset (the default is "https").
func NewHandler(opts ...Option) *Handler {
	scheme := "https"
	if s := os.Getenv("SPECULA_GIT_UPSTREAM_SCHEME"); s == "http" {
		scheme = "http"
	}
	h := &Handler{
		allowed:         map[string]struct{}{},
		mutableTTLSec:   30, // 30 seconds default for refs
		syncStaleAfter:  defaultSyncStaleAfter,
		upstreamTimeout: defaultUpstreamTimeout,
		publicOnly:      true,
		failClosed:      true,
		upstreamScheme:  scheme,
		log:             slog.Default(),
	}
	for _, opt := range opts {
		opt(h)
	}

	// Initialise runtime internals after options are applied so they reflect
	// any overrides (e.g., syncStaleAfter, upstreamScheme).
	h.transport = &http.Transport{Proxy: http.ProxyFromEnvironment}
	h.mirror = newMirrorStore(h.mirrorDir, h.syncStaleAfter, h.upstreamTimeout)
	h.pubCheck = newPublicChecker(defaultPublicProbeTTL, h.transport)
	return h
}

// Compile-time assertion.
var _ http.Handler = (*Handler)(nil)

// ServeHTTP dispatches git Smart HTTP requests. It parses the proxy path, gates
// on the host allowlist, and classifies push / authenticated requests for
// passthrough. Public upload-pack requests route to the bare-mirror serve path.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if h.pathPrefix != "" {
		p = strings.TrimPrefix(p, h.pathPrefix)
	}

	ref, ok := parseProxyPath(p, h.allowed)
	if !ok {
		http.NotFound(w, r)
		return
	}

	// Push (receive-pack) and Authorization-bearing requests are never cached.
	// gitprotocol-http §push discovery: `GET /info/refs?service=git-receive-pack`
	// encodes the service in the query string, not the path tail.  Check both so
	// push-discovery is not incorrectly served from the (pull-only) mirror.
	if ref.isReceivePack() || strings.Contains(r.URL.RawQuery, "git-receive-pack") || hasAuth(r) {
		h.passthrough(w, r, ref, "push-or-auth")
		return
	}

	h.serveMirror(w, r, ref)
}

// --------------------------------------------------------------------------
// Route parsing (ported from ai-sandbox gitproxy path.go)
// --------------------------------------------------------------------------

// repoRef identifies a Git Smart HTTP repository behind the proxy.
type repoRef struct {
	Host        string // upstream host (allowlisted)
	ProjectPath string // owner/repo or group/sub/repo (no .git suffix)
	Tail        string // e.g. "/info/refs" or "/git-upload-pack"
}

// parseProxyPath parses "/<host>/<project>.git/<tail>" and enforces the host
// allowlist. Returns ok=false for malformed paths or disallowed hosts.
func parseProxyPath(path string, allowed map[string]struct{}) (repoRef, bool) {
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return repoRef{}, false
	}
	slash := strings.IndexByte(path, '/')
	if slash <= 0 {
		return repoRef{}, false
	}
	host := path[:slash]
	if _, ok := allowed[host]; !ok {
		return repoRef{}, false
	}
	rest := path[slash+1:]
	dotGit := strings.Index(rest, gitSuffix)
	if dotGit < 0 {
		return repoRef{}, false
	}
	project := strings.TrimSuffix(rest[:dotGit], "/")
	if project == "" || strings.Contains(project, "..") {
		return repoRef{}, false
	}
	tail := rest[dotGit+len(gitSuffix):]
	if tail == "" {
		tail = "/"
	} else if !strings.HasPrefix(tail, "/") {
		return repoRef{}, false
	}
	return repoRef{Host: host, ProjectPath: project, Tail: tail}, true
}

// mirrorRelPath returns the bare-mirror path relative to the mirror root.
func (r repoRef) mirrorRelPath() string {
	return r.Host + "/" + r.ProjectPath + gitSuffix
}

// upstreamURLWithScheme returns the upstream Smart HTTP base URL using scheme.
func (r repoRef) upstreamURLWithScheme(scheme string) string {
	return scheme + "://" + r.Host + "/" + r.ProjectPath + gitSuffix
}

// isReceivePack reports push-related Smart HTTP paths (always passthrough).
func (r repoRef) isReceivePack() bool {
	low := strings.ToLower(r.Tail)
	return strings.Contains(low, "git-receive-pack")
}

// isRefAdvertise reports the mutable ref-advertisement endpoint (/info/refs).
func (r repoRef) isRefAdvertise() bool {
	return strings.HasPrefix(r.Tail, "/info/refs")
}

// hasAuth reports whether the request carries credentials (→ passthrough).
func hasAuth(r *http.Request) bool {
	return strings.TrimSpace(r.Header.Get("Authorization")) != "" ||
		strings.TrimSpace(r.Header.Get("Proxy-Authorization")) != ""
}

// --------------------------------------------------------------------------
// ArtifactRef mapping
// --------------------------------------------------------------------------

// refToArtifact maps a repoRef to the canonical ArtifactRef. Name is the repo
// identity (host/project); Version is the Smart HTTP tail. Mutable is true for
// the ref advertisement (branch/tag → SHA changes over time); the packfile that
// git-upload-pack streams carries immutable objects addressed by SHA inside the
// bare mirror.
func refToArtifact(ref repoRef) artifact.ArtifactRef {
	return artifact.ArtifactRef{
		Protocol: Protocol,
		Name:     ref.Host + "/" + ref.ProjectPath,
		Version:  strings.TrimPrefix(ref.Tail, "/"),
		Mutable:  ref.isRefAdvertise(),
	}
}

// --------------------------------------------------------------------------
// serveMirror — bare-mirror sync + git http-backend serve
// --------------------------------------------------------------------------

// serveMirror ensures the bare mirror is synced and serves it via `git
// http-backend`. The flow is:
//
//  1. Public-repo probe (when publicOnly=true).
//  2. EnsureSynced: clone or refresh the bare mirror (per-path mutex).
//  3. Update TOFU ref→SHA pins (non-fast-forward → logged warning).
//  4. Serve the request from the mirror via git http-backend CGI.
//  5. On any failure → passthrough to upstream.
func (h *Handler) serveMirror(w http.ResponseWriter, r *http.Request, ref repoRef) {
	ctx, cancel := context.WithTimeout(r.Context(), h.upstreamTimeout)
	defer cancel()

	// 1. Public-repo probe.
	if h.publicOnly {
		pub, err := h.pubCheck.IsPublic(ctx, ref)
		if err != nil {
			h.log.Warn("git: public probe failed",
				slog.String("repo", ref.mirrorRelPath()),
				slog.Any("err", err))
			if h.failClosed {
				h.passthrough(w, r, ref, "probe-fail")
				return
			}
			// failClosed=false: proceed to mirror even if probe failed.
		} else if !pub {
			h.passthrough(w, r, ref, "not-public")
			return
		}
	}

	// 2. Sync the mirror.
	//
	// This is git's cache-outcome decision: unlike the CAS-backed handlers, git's
	// byte cache is the bare mirror on disk, so the honest hit/miss axis is
	// whether the packfile git http-backend is about to build came from a mirror
	// that was already here (no upstream contact — a hit) or from one this
	// request had to clone/fetch (a miss). Marked from r.Context(), not ctx: ctx
	// is the timeout-bounded derivative, and the metrics scope rides on both, but
	// the request context is the one the middleware installed.
	upURL := ref.upstreamURLWithScheme(h.upstreamScheme)
	contactedUpstream, err := h.mirror.EnsureSynced(ctx, ref, upURL)
	if err != nil {
		// The passthrough below reverse-proxies the body from the upstream, and
		// the sync that just failed had already reached for it: a miss either way.
		metrics.MarkMiss(r.Context())
		h.log.Warn("git: mirror sync failed",
			slog.String("repo", ref.mirrorRelPath()),
			slog.Any("err", err))
		h.passthrough(w, r, ref, "sync-fail")
		return
	}
	if contactedUpstream {
		metrics.MarkMiss(r.Context())
	} else {
		metrics.MarkHit(r.Context())
	}

	// 3. Update TOFU ref→SHA pins (best-effort; non-fatal on error).
	if h.meta != nil {
		alerts := updateTOFUPins(ctx, h.meta, h.mirrorDir, ref, h.log)
		for _, a := range alerts {
			h.log.Warn(a)
		}

		// 3b. Verify signed refs (signed tier). A ref whose tip carries a valid
		// signature from the allowed-signers anchor earns `signed`; the rest stay
		// at their tofu pins. Under policy=enforce, an UNTRUSTED signature (present
		// but unauthenticated — a forged/rotated tag) fails closed: refuse to
		// serve rather than hand the client bytes an authenticity policy rejected.
		if h.signedRefs != nil {
			failClosed, sAlerts := updateSignedRefs(ctx, h.signedRefs, h.meta, h.mirrorDir, ref, h.log)
			for _, a := range sAlerts {
				h.log.Warn(a)
			}
			if failClosed {
				h.log.Error("git: refusing to serve — untrusted ref signature under enforce policy",
					slog.String("repo", ref.mirrorRelPath()))
				writeError(w, http.StatusBadGateway,
					"git: refusing to serve — a signed ref failed signature verification (enforce policy)")
				return
			}
		}
	}

	// 4. Serve the request from the mirror.
	pathInfo := "/" + ref.mirrorRelPath() + ref.Tail
	if err := serveGitHTTPBackend(w, r, h.mirrorDir, pathInfo); err != nil {
		h.log.Warn("git: http-backend serve failed",
			slog.String("repo", ref.mirrorRelPath()),
			slog.Any("err", err))
		h.passthrough(w, r, ref, "serve-fail")
	}
}

// --------------------------------------------------------------------------
// passthrough — reverse-proxy with zero caching
// --------------------------------------------------------------------------

// passthrough reverse-proxies r to the upstream host with zero caching.
// Used for push requests, authenticated requests, private repos, probe
// failures, and mirror-serve failures.
func (h *Handler) passthrough(w http.ResponseWriter, r *http.Request, ref repoRef, reason string) {
	upstream, err := url.Parse(h.upstreamScheme + "://" + ref.Host)
	if err != nil {
		h.log.Error("git: bad upstream URL",
			slog.String("host", ref.Host),
			slog.Any("err", err))
		http.Error(w, "git: bad upstream", http.StatusBadGateway)
		return
	}

	proxy := httputil.NewSingleHostReverseProxy(upstream)
	proxy.Transport = h.transport

	origDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		origDirector(req)
		// Strip the proxy prefix: /<host>/<project>.git/... → /<project>.git/...
		req.URL.Path = "/" + ref.ProjectPath + gitSuffix + ref.Tail
		req.URL.RawPath = ""
		req.URL.Scheme = h.upstreamScheme
		req.URL.Host = ref.Host
		req.Host = ref.Host
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, req *http.Request, err error) {
		h.log.Warn("git: passthrough error",
			slog.String("repo", ref.mirrorRelPath()),
			slog.String("reason", reason),
			slog.Any("err", err))
		http.Error(w, "git: upstream error", http.StatusBadGateway)
	}

	proxy.ModifyResponse = func(resp *http.Response) error {
		// Never echo credentials back to the client.
		resp.Header.Del("Authorization")
		return nil
	}

	proxy.ServeHTTP(w, r)
}

// --------------------------------------------------------------------------
// Shared helpers
// --------------------------------------------------------------------------

// writeError writes a plain-text error response.
func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(message + "\n"))
}
