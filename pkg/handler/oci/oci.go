// Package oci re-exports the OCI Distribution data-plane handler.
package oci

import (
	"log/slog"

	intoci "github.com/ivanzzeth/specula/internal/handler/oci"

	"github.com/ivanzzeth/specula/pkg/cache"
	"github.com/ivanzzeth/specula/pkg/coalesce"
	"github.com/ivanzzeth/specula/pkg/store/meta"
	"github.com/ivanzzeth/specula/pkg/upstream"
)

type (
	Handler = intoci.Handler
	Option  = intoci.Option
)

func NewHandler(cm cache.CacheManager, opts ...Option) *Handler {
	return intoci.NewHandler(cm, opts...)
}
func WithMeta(m meta.MetadataStore) Option             { return intoci.WithMeta(m) }
func WithUpstream(c upstream.Client, ups []upstream.Upstream) Option {
	return intoci.WithUpstream(c, ups)
}
func WithMutableTTL(secs int64) Option                 { return intoci.WithMutableTTL(secs) }
func WithQuarantineDir(dir string) Option              { return intoci.WithQuarantineDir(dir) }
func WithLogger(l *slog.Logger) Option                 { return intoci.WithLogger(l) }
func WithLocker(l coalesce.Locker) Option              { return intoci.WithLocker(l) }

// Hosted registry options (daemon multi-tenant write path).
func WithHostedResolver(r intoci.HostedResolver) Option {
	return intoci.WithHostedResolver(r)
}
func WithHostedReadAuthz(a intoci.HostedReadAuthz) Option {
	return intoci.WithHostedReadAuthz(a)
}
func WithOwnedNamespaceResolver(r intoci.OwnedNamespaceResolver) Option {
	return intoci.WithOwnedNamespaceResolver(r)
}

type (
	HostedResolver         = intoci.HostedResolver
	HostedReadAuthz        = intoci.HostedReadAuthz
	OwnedNamespaceResolver = intoci.OwnedNamespaceResolver
)
