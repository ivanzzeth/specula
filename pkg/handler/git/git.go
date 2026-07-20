// Package git re-exports the public git clone acceleration handler.
package git

import (
	"log/slog"
	"time"

	intgit "github.com/ivanzzeth/specula/internal/handler/git"

	"github.com/ivanzzeth/specula/pkg/store/meta"
	"github.com/ivanzzeth/specula/pkg/upstream"
	"github.com/ivanzzeth/specula/pkg/verify"
)

type (
	Handler = intgit.Handler
	Option  = intgit.Option
)

const Protocol = intgit.Protocol

func NewHandler(opts ...Option) *Handler {
	return intgit.NewHandler(opts...)
}
func WithMeta(m meta.MetadataStore) Option { return intgit.WithMeta(m) }
func WithUpstream(c upstream.Client, ups []upstream.Upstream) Option {
	return intgit.WithUpstream(c, ups)
}
func WithMutableTTL(secs int64) Option           { return intgit.WithMutableTTL(secs) }
func WithPathPrefix(prefix string) Option        { return intgit.WithPathPrefix(prefix) }
func WithLogger(l *slog.Logger) Option           { return intgit.WithLogger(l) }
func WithMirrorDir(dir string) Option            { return intgit.WithMirrorDir(dir) }
func WithAllowedUpstreams(hosts []string) Option { return intgit.WithAllowedUpstreams(hosts) }
func WithSyncStaleAfter(d time.Duration) Option  { return intgit.WithSyncStaleAfter(d) }
func WithUpstreamTimeout(d time.Duration) Option { return intgit.WithUpstreamTimeout(d) }
func WithPublicOnly(publicOnly bool) Option      { return intgit.WithPublicOnly(publicOnly) }
func WithFailClosed(failClosed bool) Option      { return intgit.WithFailClosed(failClosed) }
func WithSignedRefsVerifier(v *verify.GitSignedVerifier) Option {
	return intgit.WithSignedRefsVerifier(v)
}
func WithUpstreamScheme(scheme string) Option { return intgit.WithUpstreamScheme(scheme) }
