// Package cache implements the protocol-agnostic two-tier CacheManager:
//
//   - Immutable CAS tier — blobs keyed by sha256 digest; permanent; never
//     re-verified once written. Backed by BlobStore + CacheEntry rows.
//   - Mutable metadata tier — short-TTL entries (tag→digest, index pages,
//     packuments) keyed by a protocol-scoped string. Backed by MutableEntry
//     rows with conditional-GET revalidation state (ETag / Last-Modified).
//
// # verify-on-write quarantine (fix C2 + C3)
//
// Handlers stream upstream bytes into a quarantine file via Quarantine(), then
// call Store(). Store runs the verification Chain over the on-disk file (never
// buffers in memory), and only on PASS promotes the artifact: blob FIRST,
// metadata AFTER (fix M1). On FAIL the quarantine file is removed and a
// VerifyError is returned.
//
// # stampede protection
//
// Store uses a local singleflight Coalescer keyed by digest so concurrent
// callers for the same content run only one verify+promote pipeline.
package cache

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/coalesce"
	"github.com/ivanzzeth/specula/internal/store/blob"
	"github.com/ivanzzeth/specula/internal/store/meta"
	"github.com/ivanzzeth/specula/internal/verify"
)

// TTL sentinels mirror config.TTLNeverRevalidate / config.TTLAlwaysRevalidate
// without importing the config package (which would import koanf, creating an
// unnecessary dependency from this hot-path package).
const (
	ttlNeverRevalidate  = int64(-1)
	ttlAlwaysRevalidate = int64(0)
)

// CacheManager is the two-tier cache facade used by protocol handlers.
type CacheManager interface {
	// Lookup returns the verified CacheEntry for ref if present and the blob
	// exists (treats "meta hit but blob missing" as a miss, fix M1).
	// For mutable refs it checks TTL freshness; stale entries return nil.
	Lookup(ctx context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error)

	// Store runs the verification chain over the quarantined artifact and, on
	// PASS, atomically promotes it: blob first, metadata after (fix M1).
	// On FAIL the quarantine file is removed. Concurrent calls for the same
	// digest are coalesced (stampede protection, fix M3).
	Store(ctx context.Context, ref artifact.ArtifactRef, art *artifact.Artifact) (*artifact.CacheEntry, error)

	// Serve returns a reader for [offset, offset+length) of the verified blob
	// plus its entry. Only ever serves already-verified content (fix C2).
	// Returns ErrCacheMiss if the artifact is absent or the mutable TTL has
	// expired.
	Serve(ctx context.Context, ref artifact.ArtifactRef, offset, length int64) (io.ReadCloser, *artifact.CacheEntry, error)
}

// manager is the concrete two-tier CacheManager implementation.
type manager struct {
	blobs     blob.BlobStore
	meta      meta.MetadataStore
	chain     *verify.Chain
	coalescer coalesce.Coalescer

	// verifyFn is an internal test hook that replaces chain.Verify.
	// Production code never sets this field.
	verifyFn func(context.Context, artifact.ArtifactRef, *artifact.Artifact) (artifact.Result, error)
}

// New constructs the default CacheManager wiring the CAS BlobStore, the
// MetadataStore, and the verification Chain.
func New(blobs blob.BlobStore, metaStore meta.MetadataStore, chain *verify.Chain) CacheManager {
	return &manager{
		blobs:     blobs,
		meta:      metaStore,
		chain:     chain,
		coalescer: coalesce.NewLocalCoalescer(),
	}
}

// Compile-time assertion.
var _ CacheManager = (*manager)(nil)

// --------------------------------------------------------------------------
// Internal helpers
// --------------------------------------------------------------------------

// mutableKey is the MetadataStore key for a mutable ArtifactRef.
func mutableKey(ref artifact.ArtifactRef) string {
	return ref.Protocol + ":" + ref.Name + ":" + ref.Version
}

// isMutableFresh reports whether the mutable entry's TTL window has not
// expired. Sentinels: -1 = never expire, 0 = always expired.
func isMutableFresh(e *artifact.MutableEntry) bool {
	switch e.TTLSeconds {
	case ttlNeverRevalidate:
		return true
	case ttlAlwaysRevalidate:
		return false
	default:
		return time.Since(e.FetchedAt) < time.Duration(e.TTLSeconds)*time.Second
	}
}

// --------------------------------------------------------------------------
// Lookup
// --------------------------------------------------------------------------

// Lookup returns the verified CacheEntry for ref, or nil on a cache miss.
func (m *manager) Lookup(ctx context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	if ref.Mutable {
		return m.lookupMutable(ctx, ref, false)
	}
	return m.lookupImmutable(ctx, ref)
}

// LookupStale is like Lookup for mutable refs but returns the entry even
// when the TTL has expired, enabling serve-stale-on-upstream-failure
// (DESIGN-REVIEW fix H1). Handlers reach this via a type assertion to *manager.
func (m *manager) LookupStale(ctx context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	return m.lookupMutable(ctx, ref, true)
}

func (m *manager) lookupImmutable(ctx context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	entry, err := m.meta.Get(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("cache: meta get: %w", err)
	}
	if entry == nil {
		return nil, nil
	}
	// M1: "meta hit but blob missing" = miss; GC is responsible for orphans.
	ok, err := m.blobs.Exists(ctx, entry.Digest)
	if err != nil {
		return nil, fmt.Errorf("cache: blob exists: %w", err)
	}
	if !ok {
		return nil, nil
	}
	return entry, nil
}

func (m *manager) lookupMutable(ctx context.Context, ref artifact.ArtifactRef, allowStale bool) (*artifact.CacheEntry, error) {
	key := mutableKey(ref)
	me, err := m.meta.GetMutable(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("cache: mutable meta get: %w", err)
	}
	if me == nil {
		return nil, nil
	}
	if !allowStale && !isMutableFresh(me) {
		return nil, nil
	}
	// For entries that resolve to an immutable CAS blob, ensure the blob exists.
	if me.Digest != "" {
		ok, err := m.blobs.Exists(ctx, me.Digest)
		if err != nil {
			return nil, fmt.Errorf("cache: mutable blob exists: %w", err)
		}
		if !ok {
			return nil, nil
		}
	}
	// Synthesize a CacheEntry from the MutableEntry metadata.
	return &artifact.CacheEntry{
		Ref:        ref,
		Digest:     me.Digest,
		Protocol:   me.Protocol,
		Upstream:   me.Upstream,
		ETag:       me.ETag,
		VerifiedAt: me.FetchedAt,
		CreatedAt:  me.FetchedAt,
	}, nil
}

// --------------------------------------------------------------------------
// Store — verify-on-write quarantine promotion
// --------------------------------------------------------------------------

// Store runs verify-on-write: verifies the quarantined artifact via the
// Chain, and on PASS promotes it blob-first then metadata (fix M1). On FAIL
// the quarantine file is removed. Concurrent Stores for the same digest are
// coalesced; only one verify+promote pipeline runs (fix M3).
func (m *manager) Store(ctx context.Context, ref artifact.ArtifactRef, art *artifact.Artifact) (*artifact.CacheEntry, error) {
	key := "store:" + art.Digest
	ch := m.coalescer.DoChan(ctx, key, func() (any, error) {
		return m.storeOnce(ctx, ref, art)
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		if r.Err != nil {
			// Forget the key so the next caller retries rather than seeing a
			// cached failure (error-amplification guard, DESIGN-REVIEW §7).
			m.coalescer.Forget(key)
			return nil, r.Err
		}
		return r.Val.(*artifact.CacheEntry), nil
	}
}

// storeOnce is the single-flight body: verify, then promote blob → metadata.
func (m *manager) storeOnce(ctx context.Context, ref artifact.ArtifactRef, art *artifact.Artifact) (any, error) {
	// 1. Run the verification chain (or injected test hook).
	result, err := m.runVerify(ctx, ref, art)
	if err != nil {
		_ = os.Remove(art.Path)
		return nil, fmt.Errorf("cache: verify chain: %w", err)
	}
	if result.Status == artifact.StatusFail {
		// FAIL: clean up quarantine and return a typed error (fix C2).
		_ = os.Remove(art.Path)
		return nil, &VerifyError{Ref: ref, Result: result}
	}

	// 2. PASS or WARN: promote. Blob FIRST, metadata AFTER (fix M1 write order).
	f, err := os.Open(art.Path)
	if err != nil {
		_ = os.Remove(art.Path)
		return nil, fmt.Errorf("cache: open quarantine for promotion: %w", err)
	}
	defer f.Close() // deferred after os.Remove; safe on Linux (fd still valid)

	if err := m.blobs.Put(ctx, art.Digest, f, art.Size); err != nil {
		_ = os.Remove(art.Path)
		return nil, fmt.Errorf("cache: blob put: %w", err)
	}

	now := time.Now().UTC()
	entry := artifact.CacheEntry{
		Ref:        ref,
		Digest:     art.Digest,
		Size:       art.Size,
		Protocol:   ref.Protocol,
		Tier:       result.Tier,
		Upstream:   art.Meta.Upstream,
		ETag:       art.Meta.ETag,
		VerifiedAt: now,
		CreatedAt:  now,
	}
	if err := m.meta.Put(ctx, entry); err != nil {
		// Blob already in CAS (idempotent); metadata failure leaves an orphan
		// for GC to collect. Return the error so the caller can handle it.
		_ = os.Remove(art.Path)
		return nil, fmt.Errorf("cache: meta put: %w", err)
	}

	// Quarantine file is no longer needed after successful promotion.
	_ = os.Remove(art.Path)

	// 3. For mutable refs, update the short-TTL index entry (tag→digest map).
	// Use TTLAlwaysRevalidate as the safe default; callers that want longer TTLs
	// call meta.PutMutable directly with protocol-specific TTL from config.
	if ref.Mutable {
		me := artifact.MutableEntry{
			Key:          mutableKey(ref),
			Protocol:     ref.Protocol,
			Digest:       art.Digest,
			ETag:         art.Meta.ETag,
			LastModified: art.Meta.LastModified,
			TTLSeconds:   ttlAlwaysRevalidate,
			Upstream:     art.Meta.Upstream,
			FetchedAt:    now,
		}
		if err := m.meta.PutMutable(ctx, me); err != nil {
			return nil, fmt.Errorf("cache: mutable meta put: %w", err)
		}
	}

	return &entry, nil
}

// runVerify delegates to the test hook when set, otherwise calls the Chain.
func (m *manager) runVerify(ctx context.Context, ref artifact.ArtifactRef, art *artifact.Artifact) (artifact.Result, error) {
	if m.verifyFn != nil {
		return m.verifyFn(ctx, ref, art)
	}
	return m.chain.Verify(ctx, ref, art)
}

// --------------------------------------------------------------------------
// Serve
// --------------------------------------------------------------------------

// Serve returns a reader for [offset, offset+length) of the verified blob.
// Only ever serves from the already-verified CAS store (fix C2).
// Returns ErrCacheMiss when no valid (fresh) entry exists.
func (m *manager) Serve(ctx context.Context, ref artifact.ArtifactRef, offset, length int64) (io.ReadCloser, *artifact.CacheEntry, error) {
	entry, err := m.Lookup(ctx, ref)
	if err != nil {
		return nil, nil, fmt.Errorf("cache: serve lookup: %w", err)
	}
	if entry == nil {
		return nil, nil, ErrCacheMiss
	}
	// Payload-backed mutable entries have no CAS blob (e.g. small index pages,
	// packuments stored directly in the MutableEntry.Payload field).
	if entry.Digest == "" {
		rc, err := m.serveMutablePayload(ctx, ref, offset, length)
		if err != nil {
			return nil, nil, err
		}
		return rc, entry, nil
	}
	rc, _, err := m.blobs.Get(ctx, entry.Digest, offset, length)
	if err != nil {
		return nil, nil, fmt.Errorf("cache: blob get: %w", err)
	}
	return rc, entry, nil
}

// serveMutablePayload serves the Payload bytes of a MutableEntry (no CAS blob)
// with Range semantics. Used for small metadata responses stored inline.
func (m *manager) serveMutablePayload(ctx context.Context, ref artifact.ArtifactRef, offset, length int64) (io.ReadCloser, error) {
	key := mutableKey(ref)
	me, err := m.meta.GetMutable(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("cache: mutable get for payload serve: %w", err)
	}
	if me == nil || len(me.Payload) == 0 {
		return nil, ErrCacheMiss
	}
	data := me.Payload
	total := int64(len(data))
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	data = data[offset:]
	if length >= 0 && length < int64(len(data)) {
		data = data[:length]
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}
