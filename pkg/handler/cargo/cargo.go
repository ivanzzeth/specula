// Package cargo re-exports the Cargo sparse-registry data-plane handler.
package cargo

import (
	"log/slog"

	intcargo "github.com/ivanzzeth/specula/internal/handler/cargo"

	"github.com/ivanzzeth/specula/pkg/cache"
	"github.com/ivanzzeth/specula/pkg/coalesce"
	"github.com/ivanzzeth/specula/pkg/store/meta"
	"github.com/ivanzzeth/specula/pkg/upstream"
)

type (
	Handler = intcargo.Handler
	Option  = intcargo.Option
)

const Protocol = intcargo.Protocol

func NewHandler(cm cache.CacheManager, opts ...Option) *Handler {
	return intcargo.NewHandler(cm, opts...)
}
func WithMeta(m meta.MetadataStore) Option { return intcargo.WithMeta(m) }
func WithUpstream(c upstream.Client, ups []upstream.Upstream) Option {
	return intcargo.WithUpstream(c, ups)
}
func WithDLUpstreams(ups []upstream.Upstream) Option { return intcargo.WithDLUpstreams(ups) }
func WithMutableTTL(secs int64) Option               { return intcargo.WithMutableTTL(secs) }
func WithPathPrefix(prefix string) Option            { return intcargo.WithPathPrefix(prefix) }
func WithQuarantineDir(dir string) Option            { return intcargo.WithQuarantineDir(dir) }
func WithLogger(l *slog.Logger) Option               { return intcargo.WithLogger(l) }
func WithLocker(l coalesce.Locker) Option            { return intcargo.WithLocker(l) }
func CrateIndexPath(name string) string              { return intcargo.CrateIndexPath(name) }
