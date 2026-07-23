// Package conda re-exports the Conda channel proxy data-plane handler.
package conda

import (
	"log/slog"

	intconda "github.com/ivanzzeth/specula/internal/handler/conda"

	"github.com/ivanzzeth/specula/pkg/cache"
	"github.com/ivanzzeth/specula/pkg/coalesce"
	"github.com/ivanzzeth/specula/pkg/store/meta"
	"github.com/ivanzzeth/specula/pkg/upstream"
)

type (
	Handler = intconda.Handler
	Option  = intconda.Option
)

const Protocol = intconda.Protocol

func NewHandler(cm cache.CacheManager, opts ...Option) *Handler {
	return intconda.NewHandler(cm, opts...)
}
func WithMeta(m meta.MetadataStore) Option { return intconda.WithMeta(m) }
func WithUpstream(c upstream.Client, ups []upstream.Upstream) Option {
	return intconda.WithUpstream(c, ups)
}
func WithMutableTTL(secs int64) Option    { return intconda.WithMutableTTL(secs) }
func WithPathPrefix(prefix string) Option   { return intconda.WithPathPrefix(prefix) }
func WithQuarantineDir(dir string) Option   { return intconda.WithQuarantineDir(dir) }
func WithLogger(l *slog.Logger) Option      { return intconda.WithLogger(l) }
func WithLocker(l coalesce.Locker) Option   { return intconda.WithLocker(l) }
