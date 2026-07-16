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
	"github.com/ivanzzeth/specula/internal/store/meta"
	"github.com/ivanzzeth/specula/internal/upstream"
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

	// Dependency-confusion seam (DESIGN-REVIEW §4). Populated via options; the
	// guard logic is implemented by the leaf agent.
	privateNames    []string           // exact private-name patterns (flat namespace)
	privateUpstream *upstream.Upstream // private index; nil = none configured
	failClosed      bool               // private name + private down → 5xx, never public

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
func WithUpstream(c upstream.Client, ups []upstream.Upstream) Option {
	return func(h *Handler) { h.upstreamClt = c; h.upstreams = ups }
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
func WithPrivateUpstream(up upstream.Upstream) Option {
	return func(h *Handler) { h.privateUpstream = &up }
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
		mutableTTLSec: 1800, // 30 minutes default (devpi mirror_cache_expiry)
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
