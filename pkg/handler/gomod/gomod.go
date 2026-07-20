// Package gomod re-exports the Go module proxy (GOPROXY) data-plane handler.
package gomod

import (
	"log/slog"
	"net/http"

	intgomod "github.com/ivanzzeth/specula/internal/handler/gomod"

	"github.com/ivanzzeth/specula/pkg/cache"
	"github.com/ivanzzeth/specula/pkg/store/meta"
	"github.com/ivanzzeth/specula/pkg/upstream"
	"github.com/ivanzzeth/specula/pkg/verify"
)

type (
	Handler      = intgomod.Handler
	Option       = intgomod.Option
	SumDBHandler = intgomod.SumDBHandler
	SumDBOption  = intgomod.SumDBOption
)

const Protocol = intgomod.Protocol

func NewHandler(cm cache.CacheManager, opts ...Option) *Handler {
	return intgomod.NewHandler(cm, opts...)
}
func WithMeta(m meta.MetadataStore) Option { return intgomod.WithMeta(m) }
func WithUpstream(c upstream.Client, ups []upstream.Upstream) Option {
	return intgomod.WithUpstream(c, ups)
}
func WithSumDB(s *SumDBHandler) Option    { return intgomod.WithSumDB(s) }
func WithPathPrefix(prefix string) Option { return intgomod.WithPathPrefix(prefix) }
func WithMutableTTL(secs int64) Option    { return intgomod.WithMutableTTL(secs) }
func WithQuarantineDir(dir string) Option { return intgomod.WithQuarantineDir(dir) }
func WithLogger(l *slog.Logger) Option    { return intgomod.WithLogger(l) }

// NewSumDBHandler constructs the /sumdb/ passthrough handler.
func NewSumDBHandler(upstreamURL string, opts ...SumDBOption) *SumDBHandler {
	return intgomod.NewSumDBHandler(upstreamURL, opts...)
}

func WithSumDBPrivateMatcher(m verify.PrivateMatcher) SumDBOption {
	return intgomod.WithSumDBPrivateMatcher(m)
}
func WithSumDBHTTPClient(c *http.Client) SumDBOption { return intgomod.WithSumDBHTTPClient(c) }
func WithSumDBLogger(l *slog.Logger) SumDBOption     { return intgomod.WithSumDBLogger(l) }
func WithSumDBName(name string) SumDBOption          { return intgomod.WithSumDBName(name) }
