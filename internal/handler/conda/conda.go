// Package conda implements the Conda channel proxy data-plane handler.
// Supported routes (relative to the handler mount prefix, typically /conda):
//
//	GET/HEAD …/repodata.json              — mutable channel index
//	GET/HEAD …/current_repodata.json      — mutable channel index
//	GET/HEAD …/repodata.json.zst          — mutable compressed index
//	GET/HEAD …/current_repodata.json.zst  — mutable compressed index
//	GET/HEAD …/channeldata.json           — mutable channel metadata
//	GET/HEAD …/*.conda                    — immutable package
//	GET/HEAD …/*.tar.bz2                  — immutable package
package conda

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

const Protocol = "conda"

const mutableVersion = "repodata"

var mutableSuffixes = []string{
	"repodata.json",
	"current_repodata.json",
	"repodata.json.zst",
	"current_repodata.json.zst",
	"channeldata.json",
}

// Handler serves Conda channel index files and package downloads.
type Handler struct {
	cache         cache.CacheManager
	meta          meta.MetadataStore
	upstreamClt   upstream.Client
	upstreams     []upstream.Upstream
	channels      ChannelMap
	pathPrefix    string
	mutableTTLSec int64
	quarantineDir string
	fetchSF       coalesce.Coalescer
	locker        coalesce.Locker
	log           *slog.Logger
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
	}
	for _, o := range opts {
		o(h)
	}
	return h
}

var _ http.Handler = (*Handler)(nil)

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := r.URL.Path
	if h.pathPrefix != "" {
		p = strings.TrimPrefix(p, h.pathPrefix)
	}
	rest := strings.Trim(strings.TrimPrefix(p, "/"), "/")
	if rest == "" {
		http.Error(w, "not a conda path", http.StatusNotFound)
		return
	}
	if strings.Contains(rest, "..") {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	if isMutablePath(rest) {
		h.serveIndex(w, r, rest)
		return
	}
	if isImmutablePath(rest) {
		h.servePackage(w, r, rest)
		return
	}
	http.Error(w, "not a conda path", http.StatusNotFound)
}

func isMutablePath(p string) bool {
	for _, suffix := range mutableSuffixes {
		if strings.HasSuffix(p, suffix) {
			return true
		}
	}
	return false
}

func isImmutablePath(p string) bool {
	return strings.HasSuffix(p, ".conda") || strings.HasSuffix(p, ".tar.bz2")
}

func indexRef(relPath string) artifact.ArtifactRef {
	return artifact.ArtifactRef{
		Protocol: Protocol,
		Name:     relPath,
		Version:  mutableVersion,
		Mutable:  true,
	}
}

func packageRef(relPath string) artifact.ArtifactRef {
	return artifact.ArtifactRef{
		Protocol: Protocol,
		Name:     relPath,
		Version:  path.Base(relPath),
		Mutable:  false,
	}
}

func contentTypeForPath(relPath string) string {
	switch {
	case strings.HasSuffix(relPath, ".json"):
		return "application/json"
	case strings.HasSuffix(relPath, ".zst"):
		return "application/zstd"
	default:
		return "application/octet-stream"
	}
}
