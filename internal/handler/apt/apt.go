// Package apt implements the Debian/Ubuntu APT repository data-plane handler.
// Supported routes (relative to the handler's mount prefix; the mount point maps
// to a single repository root such as ".../ubuntu"):
//
//	GET/HEAD /dists/<suite>/InRelease               — mutable: signed release index
//	GET/HEAD /dists/<suite>/Release[.gpg]           — mutable: release index + sig
//	GET/HEAD /dists/<suite>/<comp>/binary-<arch>/Packages[.gz|.xz] — mutable
//	GET/HEAD /pool/<component>/<path>/<file>.deb    — immutable: a package file
//
// # immutable vs mutable (DESIGN-REVIEW §3)
//
// The dists/ metadata (InRelease / Release / Packages) is MUTABLE and cached in
// the short-TTL mutable tier. Because InRelease carries its own Valid-Until
// field the default policy is always-revalidate (by-hash paths make this
// race-free). The pool/*.deb package files are IMMUTABLE and promoted to the
// permanent CAS tier, content-addressed by streaming sha256.
//
// # GPG chain (DESIGN-REVIEW §1.1 — the apt gold standard)
//
// apt authenticity is end-to-end: a local, out-of-band distro keyring signs
// InRelease, which pins the SHA256 of each Packages index, which pins the SHA256
// of each .deb. A malicious mirror cannot forge the chain. The verifier is
// injected via WithGPGVerifier; Chain wiring is cmd/specula's job at integration.
//
// Routing (dists vs pool), ArtifactRef mapping, and the two-tier caching
// pipeline are fully implemented in this package.
package apt

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

// Protocol is the ArtifactRef.Protocol value for APT.
const Protocol = "apt"

// Path segment markers used to classify a request.
const (
	distsSeg = "/dists/"
	poolSeg  = "/pool/"
)

// ttlNeverRevalidate / ttlAlwaysRevalidate mirror the config sentinel values.
const (
	ttlNeverRevalidate  int64 = -1
	ttlAlwaysRevalidate int64 = 0
)

// Handler serves the APT repository data-plane API. All bytes served are
// guaranteed to have passed the verify-on-write chain inside the CacheManager
// (fix C2).
type Handler struct {
	cache         cache.CacheManager
	meta          meta.MetadataStore  // optional: direct mutable-tier (dists) access
	upstreamClt   upstream.Client     // optional: cache-miss upstream fetcher
	upstreams     []upstream.Upstream // ordered fallback list
	pathPrefix    string              // mount prefix trimmed before routing
	mutableTTLSec int64               // TTL for dists/ metadata entries (seconds)
	quarantineDir string              // directory for on-disk quarantine temp files

	// gpgVerifier is the optional apt GPG chain verifier (signed tier). Injected
	// here as a seam; the Chain wiring happens in cmd/specula.
	gpgVerifier *verify.GPGVerifier

	// fetchSF collapses concurrent COLD fetches for the same request identity
	// (ARCHITECTURE §7): N concurrent cold requests for one artifact become ONE
	// upstream round trip. Keyed by protocol|name|version|digest — what the
	// callers asked for — because the content digest cache.Store coalesces on is
	// only knowable after the download it should have prevented.
	fetchSF coalesce.Coalescer
	locker  coalesce.Locker

	log *slog.Logger
}

// Option is a functional option applied to Handler during construction.
type Option func(*Handler)

// WithMeta injects a MetadataStore for direct mutable-tier (dists) lookups.
func WithMeta(m meta.MetadataStore) Option {
	return func(h *Handler) { h.meta = m }
}

// WithUpstream configures the fallback upstream client and ordered mirror list.
func WithUpstream(c upstream.Client, ups []upstream.Upstream) Option {
	return func(h *Handler) { h.upstreamClt = c; h.upstreams = ups }
}

// WithMutableTTL overrides the dists/ metadata cache TTL (seconds). -1 / 0
// sentinels; apt defaults to 0 (always revalidate).
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

// WithLocker injects a cross-replica Locker for cold-fetch stampede protection.
func WithLocker(l coalesce.Locker) Option {
	return func(h *Handler) { h.locker = l }
}

// WithGPGVerifier injects the apt InRelease→Packages→.deb chain verifier
// (signed tier). Optional; without it apt tops out at tofu.
func WithGPGVerifier(v *verify.GPGVerifier) Option {
	return func(h *Handler) { h.gpgVerifier = v }
}

// NewHandler constructs an APT Handler backed by the given CacheManager.
func NewHandler(cm cache.CacheManager, opts ...Option) *Handler {
	h := &Handler{
		cache:         cm,
		fetchSF:       coalesce.NewLocalCoalescer(),
		mutableTTLSec: ttlAlwaysRevalidate, // InRelease has its own Valid-Until
		log:           slog.Default(),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Compile-time assertion.
var _ http.Handler = (*Handler)(nil)

// ServeHTTP dispatches APT repository requests.
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
	if strings.Contains(p, "..") {
		writeError(w, http.StatusNotFound, "not an apt path")
		return
	}

	// Immutable: pool/*.deb (and .udeb/.dsc/source tarballs).
	if repo, distPath, ok := cut(p, poolSeg); ok {
		name, file, ok := splitDir(distPath)
		if !ok {
			writeError(w, http.StatusNotFound, "not a pool file path")
			return
		}
		h.servePool(w, r, repo, name, file)
		return
	}

	// Mutable: dists/ metadata (InRelease / Release / Packages ...).
	if repo, distsPath, ok := cut(p, distsSeg); ok {
		if distsPath == "" {
			writeError(w, http.StatusNotFound, "empty dists path")
			return
		}
		h.serveDists(w, r, repo, distsPath)
		return
	}

	writeError(w, http.StatusNotFound, "not an apt path")
}

// --------------------------------------------------------------------------
// Routing helpers
// --------------------------------------------------------------------------

// cut splits p at the first occurrence of seg, returning the repository prefix
// (the portion before seg, trimmed of slashes) and the portion after seg. The
// repository prefix scopes the cache key only; buildPath rebuilds the fetch path
// relative to the upstream base (which already includes the repository root).
func cut(p, seg string) (repo, after string, ok bool) {
	i := strings.Index(p, seg)
	if i < 0 {
		return "", "", false
	}
	repo = strings.Trim(p[:i], "/")
	after = p[i+len(seg):]
	if after == "" {
		return "", "", false
	}
	return repo, after, true
}

// splitDir splits a path into its directory portion and final file component.
func splitDir(path string) (dir, file string, ok bool) {
	path = strings.TrimLeft(path, "/")
	i := strings.LastIndexByte(path, '/')
	if i <= 0 || i == len(path)-1 {
		return "", "", false
	}
	return path[:i], path[i+1:], true
}

// --------------------------------------------------------------------------
// ArtifactRef mapping
// --------------------------------------------------------------------------

// distsRef builds the mutable ref for a dists/ metadata file. buildPath rebuilds
// the fetch path as "dists/" + Version. Name carries the repository prefix so
// two mounts sharing a suite name do not collide in the mutable cache key.
func distsRef(repo, distsPath string) artifact.ArtifactRef {
	return artifact.ArtifactRef{
		Protocol: Protocol,
		Name:     repo,
		Version:  distsPath,
		Mutable:  true,
	}
}

// poolRef builds the immutable CAS ref for a pool/*.deb file. buildPath rebuilds
// the fetch path as "pool/" + Name + "/" + Version.
func poolRef(name, file string) artifact.ArtifactRef {
	return artifact.ArtifactRef{
		Protocol: Protocol,
		Name:     name,
		Version:  file,
		Mutable:  false,
	}
}

// --------------------------------------------------------------------------
// Endpoint implementations (mutable dists + immutable pool pipeline)
// --------------------------------------------------------------------------

// serveDists handles GET/HEAD /dists/... (mutable metadata; always-revalidate by
// default because InRelease carries its own Valid-Until field).
func (h *Handler) serveDists(w http.ResponseWriter, r *http.Request, repo, distsPath string) {
	ref := distsRef(repo, distsPath)
	ct := contentTypeForDistsPath(distsPath)
	h.serveMutable(w, r, ref, ct)
}

// servePool handles GET/HEAD /pool/... (immutable package file; CAS promotion).
func (h *Handler) servePool(w http.ResponseWriter, r *http.Request, repo, name, file string) {
	_ = repo // reserved for future per-repo policy scoping
	ref := poolRef(name, file)
	h.serveImmutable(w, r, ref)
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
