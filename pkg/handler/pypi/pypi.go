// Package pypi re-exports the PyPI data-plane handler.
package pypi

import (
	"log/slog"

	intpypi "github.com/ivanzzeth/specula/internal/handler/pypi"

	"github.com/ivanzzeth/specula/pkg/cache"
	"github.com/ivanzzeth/specula/pkg/coalesce"
	"github.com/ivanzzeth/specula/pkg/store/meta"
	"github.com/ivanzzeth/specula/pkg/upstream"
)

type (
	Handler = intpypi.Handler
	Option  = intpypi.Option
)

const Protocol = intpypi.Protocol

func NewHandler(cm cache.CacheManager, opts ...Option) *Handler {
	return intpypi.NewHandler(cm, opts...)
}
func WithMeta(m meta.MetadataStore) Option { return intpypi.WithMeta(m) }
func WithUpstream(c upstream.Client, ups []upstream.Upstream) Option {
	return intpypi.WithUpstream(c, ups)
}
func WithMutableTTL(secs int64) Option       { return intpypi.WithMutableTTL(secs) }
func WithPathPrefix(prefix string) Option    { return intpypi.WithPathPrefix(prefix) }
func WithQuarantineDir(dir string) Option    { return intpypi.WithQuarantineDir(dir) }
func WithLogger(l *slog.Logger) Option       { return intpypi.WithLogger(l) }
func WithLocker(l coalesce.Locker) Option    { return intpypi.WithLocker(l) }
func WithPrivateNames(names []string) Option { return intpypi.WithPrivateNames(names) }
func WithPrivateUpstream(up upstream.Upstream) Option {
	return intpypi.WithPrivateUpstream(up)
}
func WithFailClosed(failClosed bool) Option { return intpypi.WithFailClosed(failClosed) }
