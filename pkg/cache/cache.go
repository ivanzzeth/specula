// Package cache re-exports the two-tier CacheManager and verify-on-write helpers.
package cache

import (
	"context"
	"io"

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
)

var ErrCacheMiss = intcache.ErrCacheMiss

// New constructs a CacheManager wiring CAS blob store, metadata, and verify chain.
func New(blobs blob.BlobStore, metaStore meta.MetadataStore, chain *verify.Chain) CacheManager {
	return intcache.New(blobs, metaStore, chain)
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
