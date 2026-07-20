// Package blob re-exports the content-addressed BlobStore interface.
//
// Drivers: pkg/store/local (default), pkg/store/s3 (opt-in).
package blob

import intblob "github.com/ivanzzeth/specula/internal/store/blob"

// BlobStore is the content-addressed store for immutable artifact bytes.
type BlobStore = intblob.BlobStore
