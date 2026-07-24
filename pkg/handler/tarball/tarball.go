// Package tarball re-exports the generic URL-keyed tarball cache handler.
package tarball

import (
	"log/slog"

	inttarball "github.com/ivanzzeth/specula/internal/handler/tarball"

	"github.com/ivanzzeth/specula/pkg/cache"
	"github.com/ivanzzeth/specula/pkg/coalesce"
	"github.com/ivanzzeth/specula/pkg/store/meta"
	"github.com/ivanzzeth/specula/pkg/upstream"
)

type (
	Handler = inttarball.Handler
	Option  = inttarball.Option
)

const Protocol = inttarball.Protocol

func NewHandler(cm cache.CacheManager, opts ...Option) *Handler {
	return inttarball.NewHandler(cm, opts...)
}
func WithMeta(m meta.MetadataStore) Option { return inttarball.WithMeta(m) }
func WithUpstream(c upstream.Client, ups []upstream.Upstream) Option {
	return inttarball.WithUpstream(c, ups)
}
func WithMutableTTL(secs int64) Option       { return inttarball.WithMutableTTL(secs) }
func WithPathPrefix(prefix string) Option    { return inttarball.WithPathPrefix(prefix) }
func WithQuarantineDir(dir string) Option    { return inttarball.WithQuarantineDir(dir) }
func WithLogger(l *slog.Logger) Option       { return inttarball.WithLogger(l) }
func WithAllowedHosts(hosts []string) Option { return inttarball.WithAllowedHosts(hosts) }
func WithScheme(scheme string) Option        { return inttarball.WithScheme(scheme) }
func WithLocker(l coalesce.Locker) Option    { return inttarball.WithLocker(l) }
