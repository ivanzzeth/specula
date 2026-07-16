// Package blob defines the content-addressed BlobStore (CAS) interface. The
// immutable tier stores bytes by sha256 digest; drivers live in
// internal/store/local (LocalDiskDriver) and internal/store/s3 (S3Driver).
package blob

import (
	"context"
	"io"
)

// BlobStore is the content-addressed store for immutable artifact bytes.
//
// Get supports Range reads (offset/length) so containerd resume works (fix M2);
// pass length < 0 to read to EOF. It returns the reader and the full object
// size. Put is idempotent: the same digest always maps to the same bytes
// (fix M1 write ordering is handled by the cache layer: blob first, meta after).
type BlobStore interface {
	// Get returns a reader for [offset, offset+length) of the object plus the
	// full object size. length < 0 means "to end of object".
	Get(ctx context.Context, digest string, offset, length int64) (io.ReadCloser, int64, error)
	// Put stores r (size bytes) under digest. Idempotent for identical bytes.
	Put(ctx context.Context, digest string, r io.Reader, size int64) error
	// Exists reports whether an object with digest is present.
	Exists(ctx context.Context, digest string) (bool, error)
	// Delete removes the object (or decrements its refcount for CAS dedup).
	Delete(ctx context.Context, digest string) error
	// UsageBytes returns the backend's total used bytes (may be cached/approx).
	UsageBytes(ctx context.Context) (int64, error)
}
