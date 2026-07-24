// Package cache re-exports the two-tier CacheManager and verify-on-write helpers.
package cache

import (
	"context"
	"io"
	"log/slog"

	intcache "github.com/ivanzzeth/specula/internal/cache"

	"github.com/ivanzzeth/specula/pkg/artifact"
	"github.com/ivanzzeth/specula/pkg/store/blob"
	"github.com/ivanzzeth/specula/pkg/store/meta"
	"github.com/ivanzzeth/specula/pkg/verify"
)

type (
	CacheManager     = intcache.CacheManager
	EntryServer      = intcache.EntryServer
	VerifyError      = intcache.VerifyError
	PinMismatchError = intcache.PinMismatchError
	Option           = intcache.Option
)

var ErrCacheMiss = intcache.ErrCacheMiss

// New constructs a CacheManager wiring CAS blob store, metadata, and verify chain.
func New(blobs blob.BlobStore, metaStore meta.MetadataStore, chain *verify.Chain, opts ...Option) CacheManager {
	return intcache.New(blobs, metaStore, chain, opts...)
}

// WithMaxBytes sets the immutable-cache capacity ceiling in bytes (0 = unlimited).
func WithMaxBytes(n int64) Option { return intcache.WithMaxBytes(n) }

// WithLogger sets the structured logger used for capacity-eviction messages.
func WithLogger(l *slog.Logger) Option { return intcache.WithLogger(l) }

// WithEvictHook registers a callback invoked after each successful eviction.
func WithEvictHook(fn func(ctx context.Context, protocol string, size int64)) Option {
	return intcache.WithEvictHook(fn)
}

// WithVerifyHook registers a callback invoked after each verification outcome.
func WithVerifyHook(fn func(ctx context.Context, ref artifact.ArtifactRef, digest string, res artifact.Result)) Option {
	return intcache.WithVerifyHook(fn)
}

// Quarantine streams r into a temp file under dir, computing sha256 while writing.
func Quarantine(ctx context.Context, dir string, r io.Reader, umeta artifact.UpstreamMeta) (*artifact.Artifact, func(), error) {
	return intcache.Quarantine(ctx, dir, r, umeta)
}

// AsPinMismatchError unwraps err to a *PinMismatchError.
func AsPinMismatchError(err error) (*PinMismatchError, bool) {
	return intcache.AsPinMismatchError(err)
}

// AsVerifyError unwraps err to a *VerifyError.
func AsVerifyError(err error) (*VerifyError, bool) {
	return intcache.AsVerifyError(err)
}
