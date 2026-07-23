// Package hf implements the Hugging Face Hub–compatible proxy data-plane handler.
// Supported routes (relative to the handler mount prefix, typically /hf):
//
// Requests bearing Authorization or Cookie headers are reverse-proxied to the
// first configured upstream without caching (private/gated content passthrough).
//
// Anonymous requests are cached:
//   - Mutable: paths containing /api/, metadata paths (no extension), or *.json
//   - Immutable: model/dataset file downloads (everything else)
//
// Mutable JSON responses have absolute huggingface.co / hf.co / hf-mirror.com URLs
// rewritten to point back at this Specula instance.
package hf

import (
	"log/slog"
	"net/http"
	"path"
	"strings"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
	"github.com/ivanzzeth/specula/internal/coalesce"
	"github.com/ivanzzeth/specula/internal/store/meta"
	"github.com/ivanzzeth/specula/internal/upstream"
)

const Protocol = "hf"

const mutableVersion = "api"

// Handler serves Hugging Face Hub API and artifact downloads.
type Handler struct {
	cache         cache.CacheManager
	meta          meta.MetadataStore
	upstreamClt   upstream.Client
	upstreams     []upstream.Upstream
	pathPrefix    string
	mutableTTLSec int64
	quarantineDir string
	fetchSF       coalesce.Coalescer
	locker        coalesce.Locker
	log           *slog.Logger
	httpClient    *http.Client
}

type Option func(*Handler)

func WithMeta(m meta.MetadataStore) Option { return func(h *Handler) { h.meta = m } }
func WithUpstream(c upstream.Client, ups []upstream.Upstream) Option {
	return func(h *Handler) { h.upstreamClt = c; h.upstreams = ups }
}
func WithMutableTTL(secs int64) Option {
	return func(h *Handler) { h.mutableTTLSec = secs }
}
func WithPathPrefix(prefix string) Option {
	return func(h *Handler) { h.pathPrefix = strings.TrimRight(prefix, "/") }
}
func WithQuarantineDir(dir string) Option {
	return func(h *Handler) { h.quarantineDir = dir }
}
func WithLogger(l *slog.Logger) Option { return func(h *Handler) { h.log = l } }
func WithLocker(l coalesce.Locker) Option {
	return func(h *Handler) { h.locker = l }
}

func NewHandler(cm cache.CacheManager, opts ...Option) *Handler {
	h := &Handler{
		cache:         cm,
		fetchSF:       coalesce.NewLocalCoalescer(),
		mutableTTLSec: 300,
		log:           slog.Default(),
		httpClient:    http.DefaultClient,
	}
	for _, o := range opts {
		o(h)
	}
	return h
}

var _ http.Handler = (*Handler)(nil)

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if h.pathPrefix != "" {
		p = strings.TrimPrefix(p, h.pathPrefix)
	}
	rest := strings.Trim(strings.TrimPrefix(p, "/"), "/")
	if rest == "" {
		http.Error(w, "not a huggingface path", http.StatusNotFound)
		return
	}
	if strings.Contains(rest, "..") {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	if hasAuthHeaders(r) {
		h.passthrough(w, r, rest)
		return
	}

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if isMutablePath(rest) {
		h.serveMutable(w, r, rest)
		return
	}
	h.serveImmutable(w, r, rest)
}

func hasAuthHeaders(r *http.Request) bool {
	return r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != ""
}

func isMutablePath(p string) bool {
	// resolve/ and blob/ are content downloads (may end in .json).
	if strings.Contains(p, "/resolve/") || strings.Contains(p, "/blob/") {
		return false
	}
	if strings.HasPrefix(p, "api/") || strings.Contains(p, "/api/") {
		return true
	}
	if strings.HasSuffix(p, ".json") {
		return true
	}
	base := path.Base(p)
	return !strings.Contains(base, ".")
}

func mutableRef(hubPath string) artifact.ArtifactRef {
	return artifact.ArtifactRef{
		Protocol: Protocol,
		Name:     hubPath,
		Version:  mutableVersion,
		Mutable:  true,
	}
}

func immutableRef(hubPath string) artifact.ArtifactRef {
	return artifact.ArtifactRef{
		Protocol: Protocol,
		Name:     hubPath,
		Version:  path.Base(hubPath),
		Mutable:  false,
	}
}

func requestBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if fp := r.Header.Get("X-Forwarded-Proto"); fp != "" {
		scheme = fp
	}
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	return scheme + "://" + host
}
