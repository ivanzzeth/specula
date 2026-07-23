// Package hf re-exports the Hugging Face Hub–compatible proxy data-plane handler.
package hf

import (
	"log/slog"

	inthf "github.com/ivanzzeth/specula/internal/handler/hf"

	"github.com/ivanzzeth/specula/pkg/cache"
	"github.com/ivanzzeth/specula/pkg/coalesce"
	"github.com/ivanzzeth/specula/pkg/store/meta"
	"github.com/ivanzzeth/specula/pkg/upstream"
)

type (
	Handler = inthf.Handler
	Option  = inthf.Option
)

const Protocol = inthf.Protocol

func NewHandler(cm cache.CacheManager, opts ...Option) *Handler {
	return inthf.NewHandler(cm, opts...)
}
func WithMeta(m meta.MetadataStore) Option { return inthf.WithMeta(m) }
func WithUpstream(c upstream.Client, ups []upstream.Upstream) Option {
	return inthf.WithUpstream(c, ups)
}
func WithMutableTTL(secs int64) Option  { return inthf.WithMutableTTL(secs) }
func WithPathPrefix(prefix string) Option { return inthf.WithPathPrefix(prefix) }
func WithQuarantineDir(dir string) Option { return inthf.WithQuarantineDir(dir) }
func WithLogger(l *slog.Logger) Option    { return inthf.WithLogger(l) }
func WithLocker(l coalesce.Locker) Option { return inthf.WithLocker(l) }
