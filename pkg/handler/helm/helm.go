// Package helm re-exports the Helm chart repository data-plane handler.
package helm

import (
	"log/slog"

	inthelm "github.com/ivanzzeth/specula/internal/handler/helm"

	"github.com/ivanzzeth/specula/pkg/cache"
	"github.com/ivanzzeth/specula/pkg/store/meta"
	"github.com/ivanzzeth/specula/pkg/upstream"
	"github.com/ivanzzeth/specula/pkg/verify"
)

type (
	Handler = inthelm.Handler
	Option  = inthelm.Option
)

const Protocol = inthelm.Protocol

func NewHandler(cm cache.CacheManager, opts ...Option) *Handler {
	return inthelm.NewHandler(cm, opts...)
}
func WithMeta(m meta.MetadataStore) Option { return inthelm.WithMeta(m) }
func WithUpstream(c upstream.Client, ups []upstream.Upstream) Option {
	return inthelm.WithUpstream(c, ups)
}
func WithMutableTTL(secs int64) Option                         { return inthelm.WithMutableTTL(secs) }
func WithPathPrefix(prefix string) Option                      { return inthelm.WithPathPrefix(prefix) }
func WithQuarantineDir(dir string) Option                      { return inthelm.WithQuarantineDir(dir) }
func WithLogger(l *slog.Logger) Option                         { return inthelm.WithLogger(l) }
func WithProvenanceVerifier(v *verify.HelmProvVerifier) Option { return inthelm.WithProvenanceVerifier(v) }
