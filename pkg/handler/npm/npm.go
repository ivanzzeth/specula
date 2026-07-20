// Package npm re-exports the npm registry data-plane handler.
package npm

import (
	"log/slog"

	intnpm "github.com/ivanzzeth/specula/internal/handler/npm"

	"github.com/ivanzzeth/specula/pkg/cache"
	"github.com/ivanzzeth/specula/pkg/store/meta"
	"github.com/ivanzzeth/specula/pkg/upstream"
)

type (
	Handler = intnpm.Handler
	Option  = intnpm.Option
)

const Protocol = intnpm.Protocol

func NewHandler(cm cache.CacheManager, opts ...Option) *Handler {
	return intnpm.NewHandler(cm, opts...)
}
func WithMeta(m meta.MetadataStore) Option { return intnpm.WithMeta(m) }
func WithUpstream(c upstream.Client, ups []upstream.Upstream) Option {
	return intnpm.WithUpstream(c, ups)
}
func WithMutableTTL(secs int64) Option          { return intnpm.WithMutableTTL(secs) }
func WithPathPrefix(prefix string) Option       { return intnpm.WithPathPrefix(prefix) }
func WithQuarantineDir(dir string) Option       { return intnpm.WithQuarantineDir(dir) }
func WithLogger(l *slog.Logger) Option          { return intnpm.WithLogger(l) }
func WithPrivateScopes(scopes []string) Option  { return intnpm.WithPrivateScopes(scopes) }
func WithPrivateUnscoped(names []string) Option { return intnpm.WithPrivateUnscoped(names) }
func WithPrivateUpstream(up upstream.Upstream) Option {
	return intnpm.WithPrivateUpstream(up)
}
func WithFailClosed(failClosed bool) Option { return intnpm.WithFailClosed(failClosed) }
