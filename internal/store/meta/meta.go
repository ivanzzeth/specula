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
	// ListEntries returns one page of immutable cache entries for protocol,
	// narrowed by filter and ordered/sliced by page. It powers the WebUI cache
	// browser (REGISTRY-DESIGN §5.2): every protocol must be able to answer
	// "what exactly is cached here", not merely a byte total.
	//
	// protocol is required (the browser is always scoped to one protocol tab);
	// an empty protocol returns entries across all protocols.
	ListEntries(ctx context.Context, protocol string, filter EntryFilter, page Page) (EntryPage, error)
	// SetPinned marks or unmarks an entry as pinned. A pinned entry is
	// protected from GC/eviction — it is an operator's explicit "keep this"
	// override on top of the cached (evictable) tier.
	SetPinned(ctx context.Context, ref artifact.ArtifactRef, pinned bool) error
}

// EntryFilter narrows a ListEntries query. The zero value matches everything
// within the requested protocol. All non-zero fields are ANDed together.
type EntryFilter struct {
	// NameContains matches entries whose name contains this substring
	// (case-sensitive LIKE '%…%'). Empty = no name constraint.
	NameContains string
	// Tier, when non-nil, matches only entries at exactly this tier. A pointer
	// (rather than a sentinel) because TierChecksum is the zero value and must
	// remain selectable.
	Tier *artifact.Tier
	// Upstream matches entries whose recorded upstream equals this value
	// exactly. Empty = no upstream constraint.
	Upstream string
	// Pinned, when non-nil, matches only pinned (true) or unpinned (false)
	// entries.
	Pinned *bool
}

// SortField names a ListEntries ordering column. Unknown values fall back to
// SortCreatedAt so a malformed query can never produce unordered (and therefore
// unstable-paginated) output.
type SortField string

const (
	// SortCreatedAt orders by first-cached time. This is the default.
	SortCreatedAt SortField = "created_at"
	// SortSize orders by byte size (largest-first when Desc).
	SortSize SortField = "size"
	// SortName orders lexicographically by name, then version.
	SortName SortField = "name"
	// SortVerifiedAt orders by verification time.
	SortVerifiedAt SortField = "verified_at"
)

// Page describes the requested slice and ordering of a ListEntries result.
type Page struct {
	// Limit is the maximum number of rows to return. Values <= 0 or > MaxLimit
	// are clamped by the store to DefaultLimit / MaxLimit respectively, so an
	// unbounded query is not expressible.
	Limit int
	// Offset is the number of rows to skip. Negative values clamp to 0.
	Offset int
	// Sort selects the ordering column (default SortCreatedAt).
	Sort SortField
	// Desc reverses the ordering (newest / largest first).
	Desc bool
}

// Pagination bounds applied by every MetadataStore implementation. They exist so
// the WebUI cache browser can never ask a store to materialise an entire
// protocol's cache in one response.
const (
	// DefaultLimit is used when Page.Limit <= 0.
	DefaultLimit = 50
	// MaxLimit is the hard ceiling on Page.Limit.
	MaxLimit = 500
)

// Entry is one row of a ListEntries result: a CacheEntry plus the listing-only
// fields that are not part of the write path (pinned state, stable row ID).
type Entry struct {
	artifact.CacheEntry
	// ID is the opaque, URL-safe identifier for this entry, used by the
	// delete/pin endpoints. It encodes the (protocol, name, version) primary
	// key; see EncodeEntryID / DecodeEntryID.
	ID string
	// Pinned reports whether the entry is protected from eviction.
	Pinned bool
}

// EntryPage is one page of ListEntries output plus the total row count matching
// the filter (before Limit/Offset), so the UI can render "N of M".
type EntryPage struct {
	Entries []Entry
	// Total is the number of rows matching protocol+filter, ignoring the page
	// window. Callers use it to size the pager.
	Total int64
	// Limit and Offset echo the clamped page window actually applied.
	Limit  int
	Offset int
}

// Normalize clamps a Page to the store's bounds and resolves the default sort.
// Every implementation must call it so the clamping rules are identical across
// drivers (and so EntryPage.Limit/Offset always report what really happened).
func (p Page) Normalize() Page {
	if p.Limit <= 0 {
		p.Limit = DefaultLimit
	}
	if p.Limit > MaxLimit {
		p.Limit = MaxLimit
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	switch p.Sort {
	case SortSize, SortName, SortVerifiedAt, SortCreatedAt:
	default:
		p.Sort = SortCreatedAt
	}
	return p
}
