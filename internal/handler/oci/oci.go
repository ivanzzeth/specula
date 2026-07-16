// Package oci implements the Docker Registry v2 + OCI Distribution v1 data-plane
// handler. Supported routes:
//
//	GET/HEAD /v2/                        — version probe
//	GET/HEAD /v2/<name>/manifests/<ref>  — manifest by tag or digest
//	GET/HEAD /v2/<name>/blobs/<digest>   — CAS blob with Range support
//
// Tag freshness is resolved via a short-TTL mutable lookup followed by an
// upstream fetch (only verified bytes are ever served). For cache misses
// the handler fetches from the first healthy upstream, streams bytes through
// the quarantine/verify-on-write pipeline, and promotes the result to CAS
// before serving. The upstream client handles the OCI registry bearer-token
// dance automatically.
package oci

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	godigest "github.com/opencontainers/go-digest"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
	"github.com/ivanzzeth/specula/internal/store/meta"
	"github.com/ivanzzeth/specula/internal/upstream"
)

// ttlNeverRevalidate / ttlAlwaysRevalidate mirror the config sentinel values
// so the handler package has no import cycle on internal/config.
const (
	ttlNeverRevalidate  int64 = -1
	ttlAlwaysRevalidate int64 = 0
)

// Handler serves the OCI Distribution v1 / Docker Registry v2 data-plane API.
// All bytes served are guaranteed to have passed the verify-on-write chain
// inside the CacheManager (fix C2 from DESIGN-REVIEW).
type Handler struct {
	cache         cache.CacheManager
	meta          meta.MetadataStore     // optional: direct mutable-tier (tag→digest) access
	upstreamClt   upstream.Client        // optional: cache-miss upstream fetcher
	upstreams     []upstream.Upstream    // ordered fallback list
	mutableTTLSec int64                  // TTL for tag→digest mutable entries (seconds)
	quarantineDir string                 // directory for on-disk quarantine temp files
	hosted        HostedResolver         // optional: hosted-first pull seam (R2)
	hostedAuthz   HostedReadAuthz        // optional: visibility enforcement for hosted repos (R2)
	owned         OwnedNamespaceResolver // optional: authoritative-local namespace gate (R2)
	log           *slog.Logger
}

// Option is a functional option applied to Handler during construction.
type Option func(*Handler)

// WithMeta injects a MetadataStore for direct mutable-tier (tag→digest) lookups
// and short-TTL revalidation. Without it the handler falls back to
// CacheManager.Lookup for mutable refs (suitable for tests with a fake manager).
func WithMeta(m meta.MetadataStore) Option {
	return func(h *Handler) { h.meta = m }
}

// WithUpstream configures the fallback upstream client and ordered mirror list
// used when a manifest or blob is absent from the verified cache.
func WithUpstream(c upstream.Client, ups []upstream.Upstream) Option {
	return func(h *Handler) { h.upstreamClt = c; h.upstreams = ups }
}

// WithMutableTTL overrides the tag→digest cache TTL (seconds).
// Pass -1 for never-revalidate or 0 for always-revalidate.
func WithMutableTTL(secs int64) Option {
	return func(h *Handler) { h.mutableTTLSec = secs }
}

// WithQuarantineDir sets the directory used for on-disk quarantine files
// during the verify-on-write pipeline. Defaults to the OS temp directory.
func WithQuarantineDir(dir string) Option {
	return func(h *Handler) { h.quarantineDir = dir }
}

// WithLogger injects a structured logger.
func WithLogger(l *slog.Logger) Option {
	return func(h *Handler) { h.log = l }
}

// NewHandler constructs an OCI Handler backed by the given CacheManager.
// Use With* options to add MetadataStore and upstream support for production;
// without them the handler only serves content already in the verified cache.
func NewHandler(cm cache.CacheManager, opts ...Option) *Handler {
	h := &Handler{
		cache:         cm,
		mutableTTLSec: 300, // 5 minutes default
		log:           slog.Default(),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Compile-time assertion.
var _ http.Handler = (*Handler)(nil)

// ServeHTTP dispatches OCI Distribution v1 requests.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// /v2/ — registry version probe.
	if path == "/v2/" || path == "/v2" {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("{}"))
		}
		return
	}

	if !strings.HasPrefix(path, "/v2/") {
		writeOCIError(w, http.StatusNotFound, "NAME_UNKNOWN", "repository not known")
		return
	}

	rest := strings.TrimPrefix(path, "/v2/")

	// /v2/<name>/manifests/<reference>
	if i := strings.LastIndex(rest, "/manifests/"); i >= 0 {
		imgName := rest[:i]
		ref := rest[i+len("/manifests/"):]
		if imgName == "" || ref == "" {
			writeOCIError(w, http.StatusBadRequest, "NAME_INVALID", "invalid name or reference")
			return
		}
		h.serveManifest(w, r, imgName, ref)
		return
	}

	// /v2/<name>/blobs/<digest>
	if i := strings.LastIndex(rest, "/blobs/"); i >= 0 {
		imgName := rest[:i]
		digest := rest[i+len("/blobs/"):]
		if imgName == "" || digest == "" {
			writeOCIError(w, http.StatusBadRequest, "DIGEST_INVALID", "invalid name or digest")
			return
		}
		h.serveBlob(w, r, imgName, digest)
		return
	}

	writeOCIError(w, http.StatusNotFound, "NAME_UNKNOWN", "repository not known")
}

// isDigestRef returns true when ref is a well-formed OCI content digest
// (e.g. "sha256:abc…", "sha512:abc…"). Validation uses opencontainers/go-digest
// directly with blank imports for crypto/sha512 (registered in
// internal/store/local) so sha384 and sha512 are recognised alongside the
// default sha256. Tags never contain ':', so any valid digest string is
// unambiguously a content address rather than a mutable tag.
func isDigestRef(ref string) bool {
	return godigest.Digest(ref).Validate() == nil
}

// isMutableExpired reports whether a short-TTL mutable entry has exceeded its TTL.
func isMutableExpired(e *artifact.MutableEntry) bool {
	switch e.TTLSeconds {
	case ttlNeverRevalidate:
		return false
	case ttlAlwaysRevalidate:
		return true
	}
	return time.Now().After(e.FetchedAt.Add(time.Duration(e.TTLSeconds) * time.Second))
}

// mutableKey returns the MetadataStore key for a tag→digest mutable entry.
func mutableKey(imageName, tag string) string {
	return "oci:" + imageName + ":" + tag
}

// ociError is the OCI Distribution v1 error envelope.
type ociError struct {
	Errors []ociErrorItem `json:"errors"`
}

type ociErrorItem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeOCIError writes an OCI Distribution spec JSON error response.
func writeOCIError(w http.ResponseWriter, status int, code, message string) {
	body, _ := json.Marshal(ociError{Errors: []ociErrorItem{{Code: code, Message: message}}})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
