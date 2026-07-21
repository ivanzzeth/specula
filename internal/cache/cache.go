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
	"log/slog"
	"os"
	"sync"
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
	// For mutable refs: hard-TTL-expired entries return nil; XFetch soft-expired
	// entries return with SoftExpired=true so callers can serve immediately and
	// refresh in the background (RFC 5861 stale-while-revalidate).
	//
	// Digest pin: entries are keyed by (protocol, name, version) — ref.Digest is
	// NOT part of the key — so an entry found by name may hold a different
	// digest than the caller pinned. When ref.Digest is set and contradicts the
	// entry, Lookup returns a *PinMismatchError and a nil entry; it never hands
	// back an artifact that contradicts the caller's pin. An empty ref.Digest
	// means "no pin" and is always satisfied.
	//
	// Handlers that accept a caller-supplied pin should map PinMismatchError to
	// the same status as a cold-path verification failure (502) rather than to a
	// 500: it is the client's assertion failing, not a server fault.
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
	//
	// Serve is freshness-gated: it re-runs Lookup internally, so it can never
	// serve a stale mutable entry. Callers that already hold an entry — notably
	// the serve-stale-on-upstream-failure path, which holds a LookupStale result
	// — must use EntryServer.ServeEntry instead; passing a stale ref here yields
	// ErrCacheMiss (DESIGN-REVIEW fix H1).
	Serve(ctx context.Context, ref artifact.ArtifactRef, offset, length int64) (io.ReadCloser, *artifact.CacheEntry, error)
}

// EntryServer serves the bytes for a CacheEntry the caller ALREADY holds,
// performing no lookup and therefore applying no freshness gate.
//
// This is what makes serve-stale-on-upstream-failure possible (DESIGN-REVIEW
// fix H1): a handler that obtained an entry from LookupStale can render its
// bytes even though Lookup — and hence Serve — would report a miss for it.
//
// It is an optional extension rather than a CacheManager method so that the
// many CacheManager implementations elsewhere in the tree need not change; the
// production manager implements it, and handlers opt in by type assertion (the
// same convention already used for LookupStale).
//
// Freshness is the caller's decision; verification is NOT. ServeEntry still only
// ever reads from the verified CAS blob store or a verified mutable payload, so
// the "unverified bytes are never served" invariant (fix C2) holds regardless of
// how the entry was obtained.
type EntryServer interface {
	ServeEntry(ctx context.Context, entry *artifact.CacheEntry, offset, length int64) (io.ReadCloser, error)
}

// manager is the concrete two-tier CacheManager implementation.
type manager struct {
	blobs     blob.BlobStore
	meta      meta.MetadataStore
	chain     *verify.Chain
	coalescer coalesce.Coalescer

	// maxBytes is the hard ceiling on SUM(cache_entries.size). 0 = unlimited.
	maxBytes int64
	// evictMu serialises capacity enforcement so concurrent Stores do not
	// over-evict under a stampede of promotions.
	evictMu sync.Mutex
	log     *slog.Logger
	// onEvict is an optional hook (e.g. stats.RecordEvict) fired after each
	// successful entry eviction. Failures from the hook are ignored.
	onEvict func(ctx context.Context, protocol string, size int64)

	// verifyFn is an internal test hook that replaces chain.Verify.
	// Production code never sets this field.
	verifyFn func(context.Context, artifact.ArtifactRef, *artifact.Artifact) (artifact.Result, error)
}

// Option configures optional CacheManager behaviour.
type Option func(*manager)

// WithMaxBytes sets the immutable-cache capacity ceiling in bytes.
// 0 (default) means unlimited. When a Store pushes total usage above this
// value, the oldest unpinned entries are evicted until usage is at or below
// the ceiling (or no more unpinned candidates remain).
func WithMaxBytes(n int64) Option {
	return func(m *manager) {
		if n < 0 {
			n = 0
		}
		m.maxBytes = n
	}
}

// WithLogger sets the structured logger used for capacity-eviction messages.
func WithLogger(l *slog.Logger) Option {
	return func(m *manager) {
		if l != nil {
			m.log = l
		}
	}
}

// WithEvictHook registers a callback invoked after each successful eviction
// (meta deleted). Used to keep Prometheus capacity gauges current.
func WithEvictHook(fn func(ctx context.Context, protocol string, size int64)) Option {
	return func(m *manager) { m.onEvict = fn }
}

// New constructs the default CacheManager wiring the CAS BlobStore, the
// MetadataStore, and the verification Chain.
func New(blobs blob.BlobStore, metaStore meta.MetadataStore, chain *verify.Chain, opts ...Option) CacheManager {
	m := &manager{
		blobs:     blobs,
		meta:      metaStore,
		chain:     chain,
		coalescer: coalesce.NewLocalCoalescer(),
		log:       slog.Default(),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Compile-time assertions. The EntryServer assertion matters: handlers reach
// serve-stale through a type assertion, which would silently degrade to a
// freshness-gated Serve (and thus 404 on stale content) if manager ever stopped
// implementing it.
var (
	_ CacheManager = (*manager)(nil)
	_ EntryServer  = (*manager)(nil)
)

// --------------------------------------------------------------------------
// Internal helpers
// --------------------------------------------------------------------------

// mutableKey is the MetadataStore key for a mutable ArtifactRef.
func mutableKey(ref artifact.ArtifactRef) string {
	return ref.Protocol + ":" + ref.Name + ":" + ref.Version
}

// checkDigestPin enforces the caller's digest pin against the digest of the
// entry the cache is about to hand back.
//
// The immutable tier is keyed by (protocol, name, version) — ref.Digest is
// stored but is NOT part of the lookup key — so an entry found by name can
// carry any digest at all. A caller who supplies ref.Digest is making an
// integrity assertion ("serve me these bytes or fail"); without this check the
// assertion is honoured on the cold path (where the verify chain compares the
// pin against the streaming-computed digest) and silently ignored on every warm
// hit, i.e. almost always in production.
//
// This is a string comparison against metadata already in hand. It does NOT
// re-verify or re-hash the stored blob, so ARCHITECTURE §3 ("CAS 永久缓存,
// 绝不重验") is preserved: we still trust the stored bytes; we simply refuse to
// answer a request for X with the artifact Y.
//
// An empty ref.Digest means "no pin" and always passes — the pin is optional.
func checkDigestPin(ref artifact.ArtifactRef, got string) error {
	if ref.Digest == "" || ref.Digest == got {
		return nil
	}
	return &PinMismatchError{Ref: ref, Want: ref.Digest, Got: got}
}

// isMutableFresh is defined in xfetch.go (XFetch soft-expiry + TTL sentinels).

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
	// Pin check runs AFTER the M1 blob-existence gate: a missing blob is a miss
	// (the caller re-fetches and the verify chain adjudicates the pin on write,
	// which also self-heals metadata pointing at a since-GC'd digest). Only once
	// we hold genuinely servable bytes does a contradicting pin become an error.
	if err := checkDigestPin(ref, entry.Digest); err != nil {
		return nil, err
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
	softExpired := false
	if !allowStale {
		if isHardExpired(me) {
			return nil, nil
		}
		// XFetch soft-expiry: still return the entry (SWR) but flag SoftExpired
		// so handlers can kick a background revalidate without blocking the client.
		if !isMutableFresh(me) {
			softExpired = true
		}
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
	// Same pin invariant as the immutable tier. A payload-backed mutable entry
	// (me.Digest == "") cannot satisfy a digest pin either, and falls out of the
	// same comparison rather than being waved through.
	if err := checkDigestPin(ref, me.Digest); err != nil {
		return nil, err
	}
	// Synthesize a CacheEntry from the MutableEntry metadata.
	return &artifact.CacheEntry{
		Ref:         ref,
		Digest:      me.Digest,
		Protocol:    me.Protocol,
		Upstream:    me.Upstream,
		ETag:        me.ETag,
		VerifiedAt:  me.FetchedAt,
		CreatedAt:   me.FetchedAt,
		SoftExpired: softExpired,
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

	// 4. Capacity enforcement: may evict older unpinned entries so total usage
	// stays at or below maxBytes. Failures are logged but do not fail Store —
	// the just-promoted artifact is already verified and must remain servable.
	if err := m.enforceCapacity(ctx, entry.Ref); err != nil {
		m.log.Warn("cache: capacity enforcement", "err", err, "max_bytes", m.maxBytes)
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

// Serve resolves ref to a fresh entry and serves its bytes. It is exactly
// Lookup + ServeEntry: the freshness gate lives in Lookup, and every byte
// leaves through ServeEntry, so there is a single byte-serving path.
// Returns ErrCacheMiss when no valid (fresh) entry exists.
func (m *manager) Serve(ctx context.Context, ref artifact.ArtifactRef, offset, length int64) (io.ReadCloser, *artifact.CacheEntry, error) {
	entry, err := m.Lookup(ctx, ref)
	if err != nil {
		return nil, nil, fmt.Errorf("cache: serve lookup: %w", err)
	}
	rc, err := m.ServeEntry(ctx, entry, offset, length)
	if err != nil {
		return nil, nil, err
	}
	return rc, entry, nil
}

// ServeEntry serves [offset, offset+length) of the bytes for an entry the
// caller already holds, with no re-lookup and no freshness gate — this is what
// lets handlers serve a LookupStale result when the upstream is down
// (DESIGN-REVIEW fix H1).
//
// The bytes still come only from verified storage (fix C2): either the CAS blob
// named by entry.Digest, or the verified inline payload of the mutable entry.
func (m *manager) ServeEntry(ctx context.Context, entry *artifact.CacheEntry, offset, length int64) (io.ReadCloser, error) {
	if entry == nil {
		return nil, ErrCacheMiss
	}
	// Payload-backed mutable entries have no CAS blob (e.g. small index pages,
	// packuments stored directly in the MutableEntry.Payload field).
	if entry.Digest == "" {
		return m.serveMutablePayload(ctx, entry.Ref, offset, length)
	}
	rc, _, err := m.blobs.Get(ctx, entry.Digest, offset, length)
	if err != nil {
		return nil, fmt.Errorf("cache: blob get: %w", err)
	}
	return rc, nil
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
