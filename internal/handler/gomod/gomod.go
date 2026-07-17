// Package gomod implements the Go module proxy (GOPROXY protocol) data-plane
// handler. Supported routes (relative to the handler's mount prefix):
//
//	GET/HEAD /{module}/@v/list             — mutable: available version list
//	GET/HEAD /{module}/@v/{version}.info    — immutable: version metadata (JSON)
//	GET/HEAD /{module}/@v/{version}.mod     — immutable: go.mod file
//	GET/HEAD /{module}/@v/{version}.zip     — immutable: module zip
//	GET/HEAD /{module}/@latest              — mutable: latest version (JSON)
//	GET      /sumdb/{name}/...              — sumdb passthrough (see SumDBHandler)
//
// # immutable vs mutable
//
// Per-version .info/.mod/.zip are immutable and promoted to the permanent CAS
// tier (content-addressed by the sha256 computed while streaming). @v/list and
// @latest are mutable and cached with a short TTL in the mutable tier.
//
// # module-path escaping
//
// GOPROXY URLs carry the case-encoded ("bang") escaping of module paths: an
// uppercase letter U is encoded as "!u" (e.g. github.com/Azure → github.com/!azure).
// ArtifactRef.Name holds the ESCAPED (URL-form) path so upstream fetch paths are
// byte-correct; the CANONICAL (unescaped) module path — used for GONOSUMDB
// private-pattern matching — is recovered via module.UnescapePath.
//
// This is the contract skeleton: routing, escaping, and ArtifactRef mapping are
// complete; endpoint bodies return 501 until the leaf handler agent implements
// the two-tier CAS + verify-on-write fetch (mirroring internal/handler/oci).
package gomod

import (
	"log/slog"
	"net/http"
	"strings"

	"golang.org/x/mod/module"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
	"github.com/ivanzzeth/specula/internal/coalesce"
	"github.com/ivanzzeth/specula/internal/store/meta"
	"github.com/ivanzzeth/specula/internal/upstream"
)

// Protocol is the ArtifactRef.Protocol value for Go modules. It is "gomod"
// (matching upstream buildPath, the verify checksum/tofu/sumdb keys, and the
// store rows) even though the config protocol map keys this block "go".
const Protocol = "gomod"

// latestVersion is the ArtifactRef.Version sentinel for the /{module}/@latest
// endpoint. It MUST match the value special-cased by upstream.buildPath so the
// fetch path becomes "/{module}/@latest" rather than "/{module}/@v/@latest".
const latestVersion = "@latest"

// listFile is the mutable @v/list file component.
const listFile = "list"

// Recognised immutable per-version file extensions (GOPROXY @v/<version>.<ext>).
const (
	extInfo = ".info"
	extMod  = ".mod"
	extZip  = ".zip"
)

// ttlNeverRevalidate / ttlAlwaysRevalidate mirror the config sentinel values so
// the handler has no import cycle on internal/config.
const (
	ttlNeverRevalidate  int64 = -1
	ttlAlwaysRevalidate int64 = 0
)

// Handler serves the GOPROXY data-plane API. All bytes served are guaranteed to
// have passed the verify-on-write chain inside the CacheManager (fix C2).
type Handler struct {
	cache         cache.CacheManager
	meta          meta.MetadataStore  // optional: direct mutable-tier access
	upstreamClt   upstream.Client     // optional: cache-miss upstream fetcher
	upstreams     []upstream.Upstream // ordered fallback list
	sumdb         *SumDBHandler       // optional: /sumdb/ passthrough sub-handler
	pathPrefix    string              // mount prefix trimmed before routing (e.g. "/go")
	mutableTTLSec int64               // TTL for @v/list & @latest mutable entries (seconds)
	quarantineDir string              // directory for on-disk quarantine temp files
	log           *slog.Logger

	// fetchSF collapses concurrent COLD fetches for the same request identity
	// (ARCHITECTURE §7). It is keyed by protocol|name|version|digest — what the
	// callers asked for — because the content digest that cache.Store coalesces
	// on is not knowable until the download it is supposed to prevent has
	// already happened.
	//
	// Constructed unconditionally in NewHandler rather than injected by an
	// option: stampede protection is not a feature an operator opts into, and
	// leaving it to wiring is precisely how the coalescer came to exist while
	// nothing collapsed.
	fetchSF coalesce.Coalescer
}

// Option is a functional option applied to Handler during construction.
type Option func(*Handler)

// WithMeta injects a MetadataStore for direct mutable-tier lookups and
// short-TTL revalidation of @v/list and @latest.
func WithMeta(m meta.MetadataStore) Option {
	return func(h *Handler) { h.meta = m }
}

// WithUpstream configures the fallback upstream client and ordered mirror list
// used when an artifact is absent from the verified cache.
func WithUpstream(c upstream.Client, ups []upstream.Upstream) Option {
	return func(h *Handler) { h.upstreamClt = c; h.upstreams = ups }
}

// WithSumDB attaches the /sumdb/ passthrough sub-handler. Without it, /sumdb/
// requests return 404 (sumdb passthrough disabled).
func WithSumDB(s *SumDBHandler) Option {
	return func(h *Handler) { h.sumdb = s }
}

// WithPathPrefix sets a mount prefix stripped from the request path before
// routing (e.g. "/go" when mounted at "/go/"). Default "" assumes the handler
// is mounted at the root or wrapped in http.StripPrefix.
func WithPathPrefix(prefix string) Option {
	return func(h *Handler) { h.pathPrefix = strings.TrimRight(prefix, "/") }
}

// WithMutableTTL overrides the @v/list & @latest cache TTL (seconds).
// Pass -1 for never-revalidate or 0 for always-revalidate.
func WithMutableTTL(secs int64) Option {
	return func(h *Handler) { h.mutableTTLSec = secs }
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

// NewHandler constructs a GOPROXY Handler backed by the given CacheManager.
// Use With* options to add MetadataStore, upstream, and /sumdb/ passthrough
// support for production; without them the handler only serves already-cached
// content and returns 501 for the (not-yet-implemented) fetch path.
func NewHandler(cm cache.CacheManager, opts ...Option) *Handler {
	h := &Handler{
		cache:         cm,
		mutableTTLSec: 300, // 5 minutes default for @v/list & @latest
		log:           slog.Default(),
		fetchSF:       coalesce.NewLocalCoalescer(),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Compile-time assertion.
var _ http.Handler = (*Handler)(nil)

// ServeHTTP dispatches GOPROXY requests. Routing is:
//
//	/sumdb/...            → sumdb passthrough sub-handler
//	/{module}/@latest     → latest-version (mutable)
//	/{module}/@v/{file}   → list (mutable) | <v>.info|.mod|.zip (immutable)
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if h.pathPrefix != "" {
		p = strings.TrimPrefix(p, h.pathPrefix)
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}

	// /sumdb/{name}/... — checksum-database passthrough.
	if p == "/sumdb" || strings.HasPrefix(p, "/sumdb/") {
		if h.sumdb == nil {
			writeGoError(w, http.StatusNotFound, "sumdb passthrough not configured")
			return
		}
		h.sumdb.serve(w, r, strings.TrimPrefix(p, "/sumdb"))
		return
	}

	rest := strings.TrimPrefix(p, "/")

	// /{module}/@latest
	if escMod, ok := strings.CutSuffix(rest, "/@latest"); ok {
		h.serveLatest(w, r, escMod)
		return
	}

	// /{module}/@v/{file}
	if mod, file, ok := splitAtV(rest); ok {
		h.serveModuleFile(w, r, mod, file)
		return
	}

	writeGoError(w, http.StatusNotFound, "not a GOPROXY path")
}

// serveModuleFile dispatches the @v/{file} endpoints by file component.
func (h *Handler) serveModuleFile(w http.ResponseWriter, r *http.Request, escMod, file string) {
	switch {
	case file == listFile:
		h.serveList(w, r, escMod)
	case strings.HasSuffix(file, extInfo):
		h.serveInfo(w, r, escMod, file)
	case strings.HasSuffix(file, extMod):
		h.serveMod(w, r, escMod, file)
	case strings.HasSuffix(file, extZip):
		h.serveZip(w, r, escMod, file)
	default:
		writeGoError(w, http.StatusNotFound, "unknown @v file: "+file)
	}
}

// --------------------------------------------------------------------------
// Routing / escaping helpers
// --------------------------------------------------------------------------

// splitAtV splits "{module}/@v/{file}" into the escaped module path and the file
// component. LastIndex is used because a module path can itself contain "@v/"
// only in the final separator position produced by the proxy scheme.
func splitAtV(rest string) (escMod, file string, ok bool) {
	i := strings.LastIndex(rest, "/@v/")
	if i < 0 {
		return "", "", false
	}
	escMod = rest[:i]
	file = rest[i+len("/@v/"):]
	if escMod == "" || file == "" {
		return "", "", false
	}
	return escMod, file, true
}

// canonicalModule unescapes the URL-form (bang-encoded) module path to its
// canonical form and validates it. Returns an error for malformed escaping or an
// invalid module path (module.CheckPath).
func canonicalModule(escaped string) (string, error) {
	path, err := module.UnescapePath(escaped)
	if err != nil {
		return "", err
	}
	if err := module.CheckPath(path); err != nil {
		return "", err
	}
	return path, nil
}

// versionFromFile strips a known extension from an @v file component, returning
// the version and true. E.g. "v1.2.3.info" → ("v1.2.3", true). "list" → ("", false).
func versionFromFile(file string) (version string, ok bool) {
	for _, ext := range []string{extInfo, extMod, extZip} {
		if v, cut := strings.CutSuffix(file, ext); cut {
			return v, true
		}
	}
	return "", false
}

// --------------------------------------------------------------------------
// ArtifactRef mapping
// --------------------------------------------------------------------------

// immutableRef builds the CAS ref for a per-version .info/.mod/.zip file.
// Name is the escaped module path (URL form); Version is the full @v file
// component (e.g. "v1.2.3.mod") so upstream.buildPath yields the correct URL.
func immutableRef(escMod, file string) artifact.ArtifactRef {
	return artifact.ArtifactRef{
		Protocol: Protocol,
		Name:     escMod,
		Version:  file,
		Mutable:  false,
	}
}

// listRef builds the mutable ref for /{module}/@v/list.
func listRef(escMod string) artifact.ArtifactRef {
	return artifact.ArtifactRef{
		Protocol: Protocol,
		Name:     escMod,
		Version:  listFile,
		Mutable:  true,
	}
}

// latestRef builds the mutable ref for /{module}/@latest.
func latestRef(escMod string) artifact.ArtifactRef {
	return artifact.ArtifactRef{
		Protocol: Protocol,
		Name:     escMod,
		Version:  latestVersion,
		Mutable:  true,
	}
}

// --------------------------------------------------------------------------
// Error envelope
// --------------------------------------------------------------------------

// writeGoError writes a plain-text error response. The go command reads the
// HTTP status code (404 = absent, 410 = gone, 403 = forbidden) and surfaces the
// body text to the user, so responses are text/plain rather than JSON.
func writeGoError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(message + "\n"))
}
