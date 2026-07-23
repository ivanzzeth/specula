// Package cargo implements the Cargo sparse-registry data-plane handler
// (RFC 2789 / Cargo Book "Registry Index"). Supported routes (relative to
// the handler mount prefix, typically /cargo):
//
//	GET/HEAD /index/config.json              — mutable: registry config (dl/api rewritten)
//	GET/HEAD /index/<crate-path>             — mutable: sparse index JSON for a crate
//	GET/HEAD /crates/<name>/<version>/download — immutable: .crate tarball
//
// Clients point Cargo at sparse+http://host:7732/cargo/index/ via source
// replacement. config.json rewrites "dl" so crate downloads also hit Specula.
package cargo

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
	"github.com/ivanzzeth/specula/internal/coalesce"
	"github.com/ivanzzeth/specula/internal/store/meta"
	"github.com/ivanzzeth/specula/internal/upstream"
)

const Protocol = "cargo"

const (
	indexVersion        = "index"
	ttlNeverRevalidate  int64 = -1
	ttlAlwaysRevalidate int64 = 0
	defaultDLBase             = "https://static.crates.io"
)

// Handler serves Cargo sparse index + crate downloads.
type Handler struct {
	cache         cache.CacheManager
	meta          meta.MetadataStore
	upstreamClt   upstream.Client
	upstreams     []upstream.Upstream // sparse index mirrors
	registries    RegistryMap         // allowlisted named sparse-index roots
	dlUpstreams   []upstream.Upstream // .crate download mirrors (defaults to static.crates.io)
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
func WithDLUpstreams(ups []upstream.Upstream) Option {
	return func(h *Handler) { h.dlUpstreams = ups }
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
	if len(h.dlUpstreams) == 0 {
		h.dlUpstreams = []upstream.Upstream{{
			Name: "static-crates-io", BaseURL: defaultDLBase, Priority: 1, Official: true,
		}}
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
		http.Error(w, "not a cargo path", http.StatusNotFound)
		return
	}
	if strings.Contains(rest, "..") {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	// /crates/<name>/<version>/download
	if strings.HasPrefix(rest, "crates/") {
		parts := strings.Split(rest, "/")
		// crates / name / version / download
		if len(parts) == 4 && parts[3] == "download" && parts[1] != "" && parts[2] != "" {
			h.serveCrate(w, r, parts[1], parts[2])
			return
		}
		http.Error(w, "invalid crate download path", http.StatusNotFound)
		return
	}

	// /index/...
	if strings.HasPrefix(rest, "index/") {
		indexPath := strings.TrimPrefix(rest, "index/")
		if indexPath == "" {
			http.Error(w, "missing index path", http.StatusNotFound)
			return
		}
		h.serveIndex(w, r, indexPath)
		return
	}

	http.Error(w, "not a cargo path", http.StatusNotFound)
}

func indexRef(indexPath string) artifact.ArtifactRef {
	return artifact.ArtifactRef{
		Protocol: Protocol,
		Name:     indexPath,
		Version:  indexVersion,
		Mutable:  true,
	}
}

func crateRef(name, version string) artifact.ArtifactRef {
	return artifact.ArtifactRef{
		Protocol: Protocol,
		Name:     name,
		Version:  version,
		Mutable:  false,
	}
}

// CrateIndexPath returns the sparse-index relative path for a crate name
// (Cargo Book "Index Format").
func CrateIndexPath(name string) string {
	n := strings.ToLower(name)
	switch len(n) {
	case 0:
		return ""
	case 1:
		return "1/" + n
	case 2:
		return "2/" + n
	case 3:
		return "3/" + n[:1] + "/" + n
	default:
		return n[:2] + "/" + n[2:4] + "/" + n
	}
}
