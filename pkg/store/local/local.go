// Package local re-exports the local-disk CAS blob driver (default, light deps).
package local

import (
	intlocal "github.com/ivanzzeth/specula/internal/store/local"

	"github.com/ivanzzeth/specula/pkg/store/blob"
)

func init() {
	blob.SetLocalOpener(func(root string) blob.BlobStore {
		return New(root)
	})
}

type LocalDiskDriver = intlocal.LocalDiskDriver

var (
	ErrNotFound       = intlocal.ErrNotFound
	ErrDigestMismatch = intlocal.ErrDigestMismatch
)

// New constructs a local-disk BlobStore rooted at root.
func New(root string) blob.BlobStore {
	return intlocal.NewLocalDiskDriver(root)
}

// NewLocalDiskDriver is an alias for New retained for parity with internal API.
func NewLocalDiskDriver(root string) *LocalDiskDriver {
	return intlocal.NewLocalDiskDriver(root)
}
