// Package pypi implements the PyPI (PEP 503 "simple" repository API) data-plane
// handler. Supported routes (relative to the handler's mount prefix):
//
//	GET/HEAD /simple/<project>/            — mutable: the project's simple index
//	GET/HEAD /packages/<path...>/<file>    — immutable: a wheel or sdist file
//
// # immutable vs mutable (DESIGN-REVIEW §3)
//
// The /simple/<project>/ index page is MUTABLE (new releases appear over time)
// and is cached in the short-TTL mutable tier with conditional-GET revalidation.
// The wheel/sdist files it links to are IMMUTABLE (a released file never changes)
// and are promoted to the permanent CAS tier, content-addressed by the sha256
// computed while streaming.
//
// # single-index / dependency confusion (DESIGN-REVIEW §4)
//
// PyPI is a FLAT namespace with no scopes, so it is the structurally worst
// ecosystem for dependency confusion. Specula is meant to be the SOLE index a
// client points at (only --index-url, never --extra-index-url). Private names
// (WithPrivateNames) resolve ONLY from the private upstream and fail closed when
// it is unreachable — never falling back to a public mirror. Those seams are
// declared here; the guard logic lands with the leaf implementation.
//
// This is the contract skeleton: routing, PEP 503 name normalisation, and
// ArtifactRef mapping are complete; endpoint bodies return 501 until the leaf
// handler agent implements the two-tier CAS + verify-on-write fetch (mirroring
// internal/handler/oci and internal/handler/gomod).
package pypi

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
	"github.com/ivanzzeth/specula/internal/coalesce"
	"github.com/ivanzzeth/specula/internal/store/meta"
	"github.com/ivanzzeth/specula/internal/upstream"
	"github.com/ivanzzeth/specula/internal/verify"
)

// Protocol is the ArtifactRef.Protocol value for PyPI (matches upstream.buildPath
// and the store rows).
const Protocol = "pypi"

// indexVersion is the ArtifactRef.Version sentinel for a /simple/<project>/ index.
// buildPath ignores Version for the mutable pypi branch; this only scopes the
// mutable cache key ("pypi:<project>:simple").
const indexVersion = "simple"

// ttlNeverRevalidate / ttlAlwaysRevalidate mirror the config sentinel values so
// the handler has no import cycle on internal/config.
const (
	ttlNeverRevalidate  int64 = -1
	ttlAlwaysRevalidate int64 = 0
)

// Handler serves the PyPI simple-repository data-plane API. All bytes served are
// guaranteed to have passed the verify-on-write chain inside the CacheManager
// (fix C2).
type Handler struct {
	cache         cache.CacheManager
	meta          meta.MetadataStore  // optional: direct mutable-tier (index) access
	upstreamClt   upstream.Client     // optional: cache-miss upstream fetcher
	upstreams     []upstream.Upstream // ordered fallback list
	pathPrefix    string              // mount prefix trimmed before routing
	mutableTTLSec int64               // TTL for /simple/ index entries (seconds)
	quarantineDir string              // directory for on-disk quarantine temp files

	// Dependency-confusion seam (DESIGN-REVIEW §4). Populated via options.
	privateNames    []string           // exact private-name patterns (flat namespace)
	privateUpstream *upstream.Upstream // private index; nil = none configured
	failClosed      bool               // private name + private down → 5xx, never public

	// guard is the wired DependencyConfusionGuard built from the private-name
	// configuration in NewHandler. It is nil when no private names are configured.
	guard *verify.DependencyConfusionGuard

	// fetchSF collapses concurrent COLD fetches for the same request identity
	// (ARCHITECTURE §7): N concurrent cold requests for one artifact become ONE
	// upstream round trip. Keyed by protocol|name|version|digest — what the
	// callers asked for — because the content digest cache.Store coalesces on is
	// only knowable after the download it should have prevented.
	fetchSF coalesce.Coalescer

	log *slog.Logger
}

// Option is a functional option applied to Handler during construction.
type Option func(*Handler)

// WithMeta injects a MetadataStore for direct mutable-tier (index) lookups and
// short-TTL revalidation.
func WithMeta(m meta.MetadataStore) Option {
	return func(h *Handler) { h.meta = m }
}

// WithUpstream configures the fallback upstream client and ordered mirror list
// used when an artifact is absent from the verified cache.
//
// Base URLs are normalised: a trailing "/simple" suffix is stripped because the
// generic upstream.Client path builder (buildPath for protocol "pypi") already
// prepends "simple/" for mutable (index) refs.  Operators naturally write the
// full simple-index base URL (e.g. "https://pypi.tuna.tsinghua.edu.cn/simple")
// matching PEP 503 / pip --index-url convention; without this normalisation the
// fetch URL would be doubled ("…/simple/simple/<project>/").
//
// Spec reference: PEP 503 §1 defines the simple repository API at a base URL
// whose sub-path for a project is "<base>/<project>/".  The internal buildPath
// already produces "simple/<project>/" so the stored base must be the mirror
// root without the "/simple" suffix.
func WithUpstream(c upstream.Client, ups []upstream.Upstream) Option {
	return func(h *Handler) {
		h.upstreamClt = c
		normalized := make([]upstream.Upstream, len(ups))
		for i, u := range ups {
			normalized[i] = u
			normalized[i].BaseURL = normalizeUpstreamBase(u.BaseURL)
		}
		h.upstreams = normalized
	}
}

// WithMutableTTL overrides the /simple/ index cache TTL (seconds).
// Pass -1 for never-revalidate or 0 for always-revalidate.
func WithMutableTTL(secs int64) Option {
	return func(h *Handler) { h.mutableTTLSec = secs }
}

// WithPathPrefix sets a mount prefix stripped from the request path before
// routing (e.g. "/pypi" when mounted at "/pypi/").
func WithPathPrefix(prefix string) Option {
	return func(h *Handler) { h.pathPrefix = strings.TrimRight(prefix, "/") }
}

// WithQuarantineDir sets the directory used for on-disk quarantine files during
// the verify-on-write pipeline. Defaults to the OS temp directory.
func WithQuarantineDir(dir string) Option {
	return func(h *Handler) { h.quarantineDir = dir }
}

// WithLogger injects a structured logger.
func WithLogger(l *slog.Logger) Option {
	return func(h *Handler) { h.log = l }
}

// WithPrivateNames sets the exact private-name patterns that must resolve only
// from the private upstream (dependency-confusion guard, DESIGN-REVIEW §4).
func WithPrivateNames(names []string) Option {
	return func(h *Handler) { h.privateNames = names }
}

// WithPrivateUpstream sets the private index that owns the private names. Private
// names are NEVER queried against the public mirrors.
//
// The base URL is normalised identically to WithUpstream (see above).
func WithPrivateUpstream(up upstream.Upstream) Option {
	return func(h *Handler) {
		normalized := up
		normalized.BaseURL = normalizeUpstreamBase(up.BaseURL)
		h.privateUpstream = &normalized
	}
}

// WithFailClosed controls the behaviour when a private name's private upstream is
// unreachable: true = fail closed (5xx, never fall back to public).
func WithFailClosed(failClosed bool) Option {
	return func(h *Handler) { h.failClosed = failClosed }
}

// NewHandler constructs a PyPI Handler backed by the given CacheManager.
func NewHandler(cm cache.CacheManager, opts ...Option) *Handler {
	h := &Handler{
		cache:         cm,
		fetchSF:       coalesce.NewLocalCoalescer(),
		mutableTTLSec: 1800, // 30 minutes default (devpi mirror_cache_expiry)
		failClosed:    true,
		log:           slog.Default(),
	}
	for _, opt := range opts {
		opt(h)
	}
	// Wire the dependency-confusion guard from the handler's private-name config.
	// The guard is the canonical decision authority for IsPrivate and private-down
	// actions; the handler fields are kept for backward-compat option wiring.
	if len(h.privateNames) > 0 {
		h.guard = verify.NewDependencyConfusionGuard(verify.DepConfusionConfig{
			Protocol:     "pypi",
			PrivateNames: h.privateNames,
			FailClosed:   h.failClosed,
		})
	}
	return h
}

// Compile-time assertion.
var _ http.Handler = (*Handler)(nil)

// ServeHTTP dispatches PyPI simple-repository requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !allowGetHead(w, r) {
		return
	}

	p := r.URL.Path
	if h.pathPrefix != "" {
		p = strings.TrimPrefix(p, h.pathPrefix)
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}

	switch {
	case strings.HasPrefix(p, "/simple/"):
		project, ok := projectFromSimplePath(p)
		if !ok {
			writeError(w, http.StatusNotFound, "not a simple index path")
			return
		}
		h.serveIndex(w, r, project)

	case strings.HasPrefix(p, "/packages/"):
		name, file, ok := splitPackageFile(p)
		if !ok {
			writeError(w, http.StatusNotFound, "not a package file path")
			return
		}
		h.serveFile(w, r, name, file)

	default:
		writeError(w, http.StatusNotFound, "not a PyPI path")
	}
}

// --------------------------------------------------------------------------
// Upstream base-URL normalisation
// --------------------------------------------------------------------------

// normalizeUpstreamBase strips the "/simple" suffix (with optional trailing
// slash) from a PyPI upstream base URL.
//
// # Rationale (PEP 503, upstream.buildPath)
//
// The upstream.Client path builder for protocol "pypi" always prepends
// "simple/" for mutable (index) refs:
//
//	buildPath(ref{Protocol:"pypi", Mutable:true, Name:"six"}) → "simple/six/"
//
// The resulting fetch URL is therefore "<baseURL>/simple/<project>/".
// Operators naturally configure the mirror as its full simple-index root
// (e.g. "https://pypi.tuna.tsinghua.edu.cn/simple"), matching pip's
// --index-url convention (PEP 503 §1).  Without stripping the trailing
// "/simple" the URL becomes ".../simple/simple/<project>/" which returns 404
// on every conformant PyPI mirror.
//
// After stripping, the base is the mirror root:
//   - Index:   <root>/simple/<project>/   — built by buildPath + buildURL
//   - Package: <root>/packages/<path>     — built by buildPath + buildURL
func normalizeUpstreamBase(base string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/simple") {
		return base[:len(base)-len("/simple")]
	}
	return base
}

// --------------------------------------------------------------------------
// Routing / normalisation helpers
// --------------------------------------------------------------------------

// projectFromSimplePath extracts and PEP 503-normalises the project name from a
// "/simple/<project>/" path. Returns ("", false) when the path has no project.
func projectFromSimplePath(p string) (project string, ok bool) {
	rest := strings.TrimPrefix(p, "/simple/")
	rest = strings.Trim(rest, "/")
	if rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	return normalizeProject(rest), true
}

// normalizeProject applies PEP 503 normalisation: lowercase and collapse any run
// of -, _ or . into a single '-'. This canonicalises the mutable cache key so
// "Flask", "flask" and "fl_ask" resolve to the same index entry.
func normalizeProject(name string) string {
	name = strings.ToLower(name)
	var b strings.Builder
	prevSep := false
	for _, r := range name {
		if r == '-' || r == '_' || r == '.' {
			if !prevSep {
				b.WriteByte('-')
				prevSep = true
			}
			continue
		}
		b.WriteRune(r)
		prevSep = false
	}
	return strings.Trim(b.String(), "-")
}

// splitPackageFile splits "/packages/<path...>/<file>" into the CAS Name (the
// directory portion under packages/) and the file component. buildPath rebuilds
// the fetch path as "packages/" + Name + "/" + Version.
func splitPackageFile(p string) (name, file string, ok bool) {
	rest := strings.TrimPrefix(p, "/packages/")
	rest = strings.TrimLeft(rest, "/")
	i := strings.LastIndexByte(rest, '/')
	if i <= 0 || i == len(rest)-1 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}

// --------------------------------------------------------------------------
// ArtifactRef mapping
// --------------------------------------------------------------------------

// indexRef builds the mutable ref for a /simple/<project>/ index.
func indexRef(project string) artifact.ArtifactRef {
	return artifact.ArtifactRef{
		Protocol: Protocol,
		Name:     project,
		Version:  indexVersion,
		Mutable:  true,
	}
}

// fileRef builds the immutable CAS ref for a wheel/sdist file.
func fileRef(name, file string) artifact.ArtifactRef {
	return artifact.ArtifactRef{
		Protocol: Protocol,
		Name:     name,
		Version:  file,
		Mutable:  false,
	}
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

// writeError writes a plain-text error response (pip surfaces the status code).
func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(message + "\n"))
}
