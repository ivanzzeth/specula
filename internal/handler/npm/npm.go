// Package npm implements the npm registry data-plane handler. Supported routes
// (relative to the handler's mount prefix):
//
//	GET/HEAD /<package>                       — mutable: the packument (metadata)
//	GET/HEAD /@<scope>/<package>              — mutable: scoped packument
//	GET/HEAD /<package>/-/<file>.tgz          — immutable: a package tarball
//	GET/HEAD /@<scope>/<package>/-/<file>.tgz — immutable: scoped tarball
//
// # immutable vs mutable (DESIGN-REVIEW §3)
//
// The packument (the per-package JSON metadata document, including dist-tags and
// the version list) is MUTABLE and cached in the short-TTL mutable tier with
// conditional-GET revalidation (verdaccio default 2 min). The *.tgz tarballs it
// references are IMMUTABLE and promoted to the permanent CAS tier,
// content-addressed by the sha256 computed while streaming.
//
// # dependency confusion (DESIGN-REVIEW §4)
//
// Scoped names (@scope/pkg) are structurally confusion-resistant — an attacker
// cannot publish under your scope. Unscoped private names need an explicit
// denylist (WithPrivateUnscoped) and must never be queried upstream. Scoped
// private names bind to a private registry (WithPrivateScopes + WithPrivateUpstream)
// and fail closed when it is unreachable. Those seams are declared here; the
// guard logic lands with the leaf implementation.
//
// This is the contract skeleton: routing (including scoped names and the %2F
// separator), and ArtifactRef mapping are complete; endpoint bodies return 501.
package npm

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
	"github.com/ivanzzeth/specula/internal/store/meta"
	"github.com/ivanzzeth/specula/internal/upstream"
)

// Protocol is the ArtifactRef.Protocol value for npm.
const Protocol = "npm"

// packumentVersion is the ArtifactRef.Version sentinel for a mutable packument.
// buildPath ignores Version for the mutable npm branch; it only scopes the
// mutable cache key ("npm:<package>:packument").
const packumentVersion = "packument"

// tarballSep is the npm tarball path separator: /<package>/-/<file>.
const tarballSep = "/-/"

// ttlNeverRevalidate / ttlAlwaysRevalidate mirror the config sentinel values so
// the handler has no import cycle on internal/config.
const (
	ttlNeverRevalidate  int64 = -1
	ttlAlwaysRevalidate int64 = 0
)

// Handler serves the npm registry data-plane API. All bytes served are
// guaranteed to have passed the verify-on-write chain inside the CacheManager
// (fix C2).
type Handler struct {
	cache         cache.CacheManager
	meta          meta.MetadataStore  // optional: direct mutable-tier (packument) access
	upstreamClt   upstream.Client     // optional: cache-miss upstream fetcher
	upstreams     []upstream.Upstream // ordered fallback list
	pathPrefix    string              // mount prefix trimmed before routing
	mutableTTLSec int64               // TTL for packument entries (seconds)
	quarantineDir string              // directory for on-disk quarantine temp files

	// Dependency-confusion seam (DESIGN-REVIEW §4). Populated via options.
	privateScopes   []string           // scopes bound to the private registry (e.g. "@myorg")
	privateUnscoped []string           // unscoped names that must never be queried upstream
	privateUpstream *upstream.Upstream // private registry; nil = none configured
	failClosed      bool               // private + private down → 5xx, never public

	log *slog.Logger
}

// Option is a functional option applied to Handler during construction.
type Option func(*Handler)

// WithMeta injects a MetadataStore for direct mutable-tier (packument) lookups.
func WithMeta(m meta.MetadataStore) Option {
	return func(h *Handler) { h.meta = m }
}

// WithUpstream configures the fallback upstream client and ordered mirror list.
func WithUpstream(c upstream.Client, ups []upstream.Upstream) Option {
	return func(h *Handler) { h.upstreamClt = c; h.upstreams = ups }
}

// WithMutableTTL overrides the packument cache TTL (seconds). -1 / 0 sentinels.
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

// WithPrivateScopes sets the npm scopes bound to the private registry.
func WithPrivateScopes(scopes []string) Option {
	return func(h *Handler) { h.privateScopes = scopes }
}

// WithPrivateUnscoped sets the unscoped names that must never be queried upstream.
func WithPrivateUnscoped(names []string) Option {
	return func(h *Handler) { h.privateUnscoped = names }
}

// WithPrivateUpstream sets the private registry that owns the private scopes/names.
func WithPrivateUpstream(up upstream.Upstream) Option {
	return func(h *Handler) { h.privateUpstream = &up }
}

// WithFailClosed controls behaviour when a private name's registry is unreachable.
func WithFailClosed(failClosed bool) Option {
	return func(h *Handler) { h.failClosed = failClosed }
}

// NewHandler constructs an npm Handler backed by the given CacheManager.
func NewHandler(cm cache.CacheManager, opts ...Option) *Handler {
	h := &Handler{
		cache:         cm,
		mutableTTLSec: 120, // 2 minutes default (verdaccio packument maxage)
		failClosed:    true,
		log:           slog.Default(),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Compile-time assertion.
var _ http.Handler = (*Handler)(nil)

// ServeHTTP dispatches npm registry requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !allowGetHead(w, r) {
		return
	}

	p := r.URL.Path
	if h.pathPrefix != "" {
		p = strings.TrimPrefix(p, h.pathPrefix)
	}
	rest := strings.TrimLeft(p, "/")
	if rest == "" {
		writeError(w, http.StatusNotFound, "not an npm path")
		return
	}

	// Tarball: /<package>/-/<file>. Split on the LAST "/-/" so scoped names
	// (@scope/pkg) survive intact in the package portion.
	if pkg, file, ok := splitTarball(rest); ok {
		h.serveTarball(w, r, pkg, file)
		return
	}

	// Otherwise a packument request. Decode the %2F scoped-name separator that
	// npm sends for scoped packuments (@scope%2Fpkg).
	pkg := decodeScopedName(rest)
	if !validPackageName(pkg) {
		writeError(w, http.StatusNotFound, "invalid package name")
		return
	}
	h.servePackument(w, r, pkg)
}

// --------------------------------------------------------------------------
// Routing helpers
// --------------------------------------------------------------------------

// splitTarball splits "<package>/-/<file>" into the package name (possibly
// scoped) and the tarball file component. Returns ok=false when there is no
// "/-/" separator.
func splitTarball(rest string) (pkg, file string, ok bool) {
	i := strings.LastIndex(rest, tarballSep)
	if i <= 0 {
		return "", "", false
	}
	pkg = rest[:i]
	file = rest[i+len(tarballSep):]
	if pkg == "" || file == "" || strings.Contains(file, "/") {
		return "", "", false
	}
	return pkg, file, true
}

// decodeScopedName converts the URL-encoded scoped-name separator (%2F / %2f)
// back to '/', so "@scope%2Fpkg" becomes "@scope/pkg".
func decodeScopedName(name string) string {
	name = strings.ReplaceAll(name, "%2F", "/")
	name = strings.ReplaceAll(name, "%2f", "/")
	return strings.Trim(name, "/")
}

// validPackageName performs a minimal structural check on a (possibly scoped)
// package name. Full npm validation is left to the leaf implementation.
func validPackageName(pkg string) bool {
	if pkg == "" || strings.Contains(pkg, "..") {
		return false
	}
	if strings.HasPrefix(pkg, "@") {
		// Scoped: exactly one '/' separating scope and package.
		return strings.Count(pkg, "/") == 1
	}
	// Unscoped: no '/'.
	return !strings.Contains(pkg, "/")
}

// --------------------------------------------------------------------------
// ArtifactRef mapping
// --------------------------------------------------------------------------

// packumentRef builds the mutable ref for a package's packument.
func packumentRef(pkg string) artifact.ArtifactRef {
	return artifact.ArtifactRef{
		Protocol: Protocol,
		Name:     pkg,
		Version:  packumentVersion,
		Mutable:  true,
	}
}

// tarballRef builds the immutable CAS ref for a package tarball. buildPath
// rebuilds the fetch path as Name + "/-/" + Version.
func tarballRef(pkg, file string) artifact.ArtifactRef {
	return artifact.ArtifactRef{
		Protocol: Protocol,
		Name:     pkg,
		Version:  file,
		Mutable:  false,
	}
}

// servePackument and serveTarball are implemented in endpoints.go.

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

// writeError writes a JSON error envelope (npm clients parse {"error": ...}).
func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":"` + message + `"}` + "\n"))
}
