// Package helm implements the CLASSIC HTTP Helm chart repository data-plane
// handler. Supported routes (relative to the handler's mount prefix):
//
//	GET/HEAD /<repo>/index.yaml            — mutable: the repository index
//	GET/HEAD /<repo>/<chart>-<ver>.tgz     — immutable: a packaged chart
//	GET/HEAD /<repo>/<chart>-<ver>.tgz.prov — immutable: the chart provenance
//
// OCI-form Helm charts (helm registry login / oci://) are served by the OCI
// handler (internal/handler/oci) and are intentionally NOT duplicated here.
//
// # immutable vs mutable (DESIGN-REVIEW §3)
//
// index.yaml is MUTABLE (new chart versions are appended over time) and cached
// in the short-TTL mutable tier with conditional-GET revalidation (30 min
// default). Chart *.tgz files and their *.prov signatures are IMMUTABLE and
// promoted to the permanent CAS tier, content-addressed by streaming sha256.
//
// # provenance (.prov) signing (DESIGN-REVIEW §1.1)
//
// A chart's .prov file is a clear-signed GPG document binding the chart's
// SHA256. Verified against a local keyring it lifts helm to the "signed" tier
// (a gold standard on par with apt); a chart with no .prov degrades to a lower
// tier rather than failing. The verifier is injected via WithProvenanceVerifier;
// wiring it into the Chain is cmd/specula's job at integration.
//
// This is the contract skeleton: routing and ArtifactRef mapping are complete;
// endpoint bodies return 501.
package helm

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

// Protocol is the ArtifactRef.Protocol value for classic-HTTP Helm.
const Protocol = "helm"

// indexFile is the mutable repository index file name.
const indexFile = "index.yaml"

// Immutable chart file suffixes.
const (
	extChart = ".tgz"
	extProv  = ".tgz.prov"
)

// ttlNeverRevalidate / ttlAlwaysRevalidate mirror the config sentinel values.
const (
	ttlNeverRevalidate  int64 = -1
	ttlAlwaysRevalidate int64 = 0
)

// Handler serves the classic-HTTP Helm chart repository data-plane API. All
// bytes served are guaranteed to have passed the verify-on-write chain inside
// the CacheManager (fix C2).
type Handler struct {
	cache         cache.CacheManager
	meta          meta.MetadataStore  // optional: direct mutable-tier (index) access
	upstreamClt   upstream.Client     // optional: cache-miss upstream fetcher
	upstreams     []upstream.Upstream // ordered fallback list
	pathPrefix    string              // mount prefix trimmed before routing
	mutableTTLSec int64               // TTL for index.yaml entries (seconds)
	quarantineDir string              // directory for on-disk quarantine temp files

	// provVerifier is the optional .prov GPG signature verifier (signed tier).
	// Injected here as a seam; the Chain wiring happens in cmd/specula.
	provVerifier *verify.HelmProvVerifier

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

// WithMeta injects a MetadataStore for direct mutable-tier (index) lookups.
func WithMeta(m meta.MetadataStore) Option {
	return func(h *Handler) { h.meta = m }
}

// WithUpstream configures the fallback upstream client and ordered mirror list.
func WithUpstream(c upstream.Client, ups []upstream.Upstream) Option {
	return func(h *Handler) { h.upstreamClt = c; h.upstreams = ups }
}

// WithMutableTTL overrides the index.yaml cache TTL (seconds). -1 / 0 sentinels.
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

// WithProvenanceVerifier injects the .prov GPG verifier used to lift charts to
// the signed tier. Optional; without it Helm tops out at consensus/tofu.
func WithProvenanceVerifier(v *verify.HelmProvVerifier) Option {
	return func(h *Handler) { h.provVerifier = v }
}

// NewHandler constructs a classic-HTTP Helm Handler backed by the CacheManager.
func NewHandler(cm cache.CacheManager, opts ...Option) *Handler {
	h := &Handler{
		cache:         cm,
		fetchSF:       coalesce.NewLocalCoalescer(),
		mutableTTLSec: 1800, // 30 minutes default for index.yaml
		log:           slog.Default(),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Compile-time assertion.
var _ http.Handler = (*Handler)(nil)

// ServeHTTP dispatches classic-HTTP Helm repository requests.
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
		writeError(w, http.StatusNotFound, "not a Helm path")
		return
	}

	// Mutable: <repo>/index.yaml.
	if repo, ok := strings.CutSuffix(rest, "/"+indexFile); ok {
		if repo == "" {
			writeError(w, http.StatusNotFound, "missing repository path")
			return
		}
		h.serveIndex(w, r, repo)
		return
	}
	// The index may also sit at the mount root ("/index.yaml").
	if rest == indexFile {
		h.serveIndex(w, r, "")
		return
	}

	// Immutable: <repo>/<chart>-<ver>.tgz[.prov].
	// splitRepoFile handles the common "<repo>/<file>" shape. When there is no
	// slash (ok=false) the file sits at the mount root — a flat repository
	// layout where the chart lives directly at the base URL with no sub-path
	// (e.g. https://mirror.azure.cn/kubernetes/charts/redis-10.5.7.tgz).
	// Treat this identically to the root index.yaml special case above:
	// repo="" (empty) and file=the bare filename.  Without this, index.yaml
	// discovery succeeds (the special case above already handled it) but chart
	// downloads return 404, making the entire flat-repo workflow unusable.
	if strings.HasSuffix(rest, extProv) || strings.HasSuffix(rest, extChart) {
		repo, file, ok := splitRepoFile(rest)
		if !ok {
			// Flat repo: bare chart filename with no repo segment.
			repo = ""
			file = rest
		}
		h.serveChart(w, r, repo, file)
		return
	}

	writeError(w, http.StatusNotFound, "not a Helm path")
}

// --------------------------------------------------------------------------
// Routing helpers
// --------------------------------------------------------------------------

// splitRepoFile splits "<repo>/<file>" into the repository path and the chart
// file component (the last path segment). Returns ok=false when there is no
// separating slash.
func splitRepoFile(rest string) (repo, file string, ok bool) {
	i := strings.LastIndexByte(rest, '/')
	if i <= 0 || i == len(rest)-1 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}

// --------------------------------------------------------------------------
// ArtifactRef mapping
// --------------------------------------------------------------------------

// indexRef builds the mutable ref for a repository's index.yaml. buildPath
// rebuilds the fetch path as Name + "/index.yaml".
func indexRef(repo string) artifact.ArtifactRef {
	return artifact.ArtifactRef{
		Protocol: Protocol,
		Name:     repo,
		Version:  indexFile,
		Mutable:  true,
	}
}

// chartRef builds the immutable CAS ref for a chart .tgz or .prov file.
// buildPath rebuilds the fetch path as Name + "/" + Version.
func chartRef(repo, file string) artifact.ArtifactRef {
	return artifact.ArtifactRef{
		Protocol: Protocol,
		Name:     repo,
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

// writeError writes a plain-text error response.
func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(message + "\n"))
}
