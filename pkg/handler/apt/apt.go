// Package apt re-exports the apt repository data-plane handler.
package apt

import (
	"log/slog"

	intapt "github.com/ivanzzeth/specula/internal/handler/apt"

	"github.com/ivanzzeth/specula/pkg/cache"
	"github.com/ivanzzeth/specula/pkg/coalesce"
	"github.com/ivanzzeth/specula/pkg/store/meta"
	"github.com/ivanzzeth/specula/pkg/upstream"
	"github.com/ivanzzeth/specula/pkg/verify"
)

type (
	Handler = intapt.Handler
	Option  = intapt.Option
)

const Protocol = intapt.Protocol

func NewHandler(cm cache.CacheManager, opts ...Option) *Handler {
	return intapt.NewHandler(cm, opts...)
}
func WithMeta(m meta.MetadataStore) Option { return intapt.WithMeta(m) }
func WithUpstream(c upstream.Client, ups []upstream.Upstream) Option {
	return intapt.WithUpstream(c, ups)
}
func WithMutableTTL(secs int64) Option             { return intapt.WithMutableTTL(secs) }
func WithPathPrefix(prefix string) Option          { return intapt.WithPathPrefix(prefix) }
func WithQuarantineDir(dir string) Option          { return intapt.WithQuarantineDir(dir) }
func WithLogger(l *slog.Logger) Option             { return intapt.WithLogger(l) }
func WithLocker(l coalesce.Locker) Option          { return intapt.WithLocker(l) }
func WithGPGVerifier(v *verify.GPGVerifier) Option { return intapt.WithGPGVerifier(v) }

type RepositorySpec = intapt.RepositorySpec

func WithRepositories(repos intapt.RepositoryMap) Option {
	return intapt.WithRepositories(repos)
}

func RepositoriesFromSpecs(specs []RepositorySpec) intapt.RepositoryMap {
	return intapt.RepositoriesFromSpecs(specs)
}

