// Package meta defines the MetadataStore interface: the authoritative record of
// immutable CacheEntry rows (digest, size, protocol, tier, upstream,
// verified_at, etag) plus the short-TTL mutable tier (tag->digest, indexes,
// packuments) with conditional-revalidation state. Drivers live in
// internal/store/sqlite and internal/store/postgres.
package meta

import (
	"context"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// MetadataStore is the protocol-agnostic metadata backend. Immutable entries
// are keyed by ArtifactRef/digest; mutable entries by a protocol-scoped string
// key. CacheSizeByProtocol powers O(1) capacity stats (G7).
type MetadataStore interface {
	// Get returns the immutable CacheEntry for ref, or (nil, nil) if absent.
	Get(ctx context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error)
	// Put upserts an immutable CacheEntry (records digest, size, protocol,
	// tier, upstream, verified_at, etag). Written AFTER the blob lands (M1).
	Put(ctx context.Context, entry artifact.CacheEntry) error
	// Delete removes the immutable entry for ref.
	Delete(ctx context.Context, ref artifact.ArtifactRef) error
	// GetMutable returns the short-TTL MutableEntry for key, or (nil, nil).
	GetMutable(ctx context.Context, key string) (*artifact.MutableEntry, error)
	// PutMutable upserts a MutableEntry with its TTL + revalidation state.
	PutMutable(ctx context.Context, entry artifact.MutableEntry) error
	// DeleteMutable removes the mutable entry for key. A no-op if absent.
	//
	// The mutable tier is a pointer layer (tag→digest), so deleting a name must
	// remove the pointer, not merely the bytes it points at: a stale pointer with
	// a never-revalidate TTL would keep resolving a deleted tag forever.
	DeleteMutable(ctx context.Context, key string) error
	// CacheSizeByProtocol returns SUM(size),COUNT(*),MIN/MAX(created_at)
	// grouped by protocol.
	CacheSizeByProtocol(ctx context.Context) (map[string]artifact.SizeStat, error)
}
