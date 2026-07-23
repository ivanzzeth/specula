// Package apt — two-tier caching pipeline for APT repository artifacts.
//
// This file implements the mutable (dists/) and immutable (pool/) data pipelines
// following the reference pattern established by internal/handler/gomod/endpoints.go.
//
// # Mutable tier (dists/ metadata)
//
// InRelease / Release / Packages are versioned by the upstream Valid-Until
// field, not by a content digest. The handler uses a short-TTL mutable entry
// keyed by (Protocol="apt", Name=repo, Version=distsPath). Conditional GET
// (ETag / If-Modified-Since) is used for revalidation; 304 extends the TTL
// without re-downloading. On upstream failure, stale entries are served.
//
// # Immutable tier (pool/*.deb)
//
// Pool package files are content-addressed by SHA256 and promoted once to the
// permanent CAS tier via the verify-on-write quarantine pipeline (fix C2/C3).
// A second client request for the same .deb is served entirely from the CAS
// without contacting the upstream.
package apt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
	"github.com/ivanzzeth/specula/internal/coalesce"
	"github.com/ivanzzeth/specula/internal/metrics"
	"github.com/ivanzzeth/specula/internal/upstream"
)

// staler is an optional extension of CacheManager that returns mutable entries
// even when their TTL has expired. Used for serve-stale-on-upstream-failure.
// The production manager (cache.manager) implements this; test fakes may not.
type staler interface {
	LookupStale(ctx context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error)
}

// entryServer is an optional extension of CacheManager that serves the bytes of
// a CacheEntry the caller ALREADY holds, with no re-lookup and no freshness
// gate. This is what makes serve-stale-on-upstream-failure work: cache.Serve
// re-runs Lookup, which reports a miss for a stale mutable entry, so a stale
// entry can only be rendered through ServeEntry (DESIGN-REVIEW fix H1).
// The production manager (cache.manager) implements this; test fakes may not.
type entryServer interface {
	ServeEntry(ctx context.Context, entry *artifact.CacheEntry, offset, length int64) (io.ReadCloser, error)
}

// --------------------------------------------------------------------------
// Content-type helpers
// --------------------------------------------------------------------------

// contentTypeForDistsPath returns a MIME type for a dists/ metadata path.
// Compressed variants (.gz / .xz / .bz2) reflect the compressed format;
// everything else is treated as UTF-8 plain text.
func contentTypeForDistsPath(distsPath string) string {
	switch {
	case strings.HasSuffix(distsPath, ".gz"):
		return "application/x-gzip"
	case strings.HasSuffix(distsPath, ".xz"):
		return "application/x-xz"
	case strings.HasSuffix(distsPath, ".bz2"):
		return "application/x-bzip2"
	case strings.HasSuffix(distsPath, "Release.gpg"):
		return "application/pgp-signature"
	default:
		return "text/plain; charset=utf-8"
	}
}

// contentTypeForPool returns the MIME type for a pool package file.
func contentTypeForPool(file string) string {
	switch {
	case strings.HasSuffix(file, ".deb"), strings.HasSuffix(file, ".udeb"):
		return "application/vnd.debian.binary-package"
	case strings.HasSuffix(file, ".dsc"):
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

// --------------------------------------------------------------------------
// Mutable key helpers
// --------------------------------------------------------------------------

// aptMutableKey returns the MetadataStore key for a mutable apt ArtifactRef
// (dists/ metadata). Mirrors cache.mutableKey (unexported).
func aptMutableKey(ref artifact.ArtifactRef) string {
	return ref.Protocol + ":" + ref.Name + ":" + ref.Version
}

// --------------------------------------------------------------------------
// Mutable pipeline (dists/)
// --------------------------------------------------------------------------

// serveMutable implements the full mutable caching pipeline for dists/ metadata:
//
//  1. Check the short-TTL cache (fresh entries served immediately).
//  2. Stale: attempt a conditional GET (If-None-Match / If-Modified-Since).
//     304 → extend TTL, serve stale.  200 → store new content.
//  3. Complete miss: fresh fetch → quarantine → verify-on-write → store → serve.
//  4. Upstream unreachable with stale content: serve stale.
//
// # TTL=0 (always-revalidate) and cache.Serve
//
// cache.Serve internally calls cache.Lookup, which respects TTL. When
// mutableTTLSec==0 (Debian Repository Format §InRelease: always-revalidate),
// Lookup returns nil even for content just stored — so re-resolving a ref
// through Serve is never correct on this pipeline. Every response below is
// therefore written by serveFromCache from an entry the handler already holds
// (a fresh Lookup, a LookupStale, or a Store result), which streams the bytes
// via cache.EntryServer.ServeEntry without a second lookup.
func (h *Handler) serveMutable(w http.ResponseWriter, r *http.Request, ref artifact.ArtifactRef, ct string) {
	ctx := r.Context()

	// 1. Fresh cache lookup.
	entry, err := h.cache.Lookup(ctx, ref)
	if err != nil {
		h.log.Error("apt: mutable lookup", "ref", ref, "err", err)
		writeError(w, http.StatusInternalServerError, "cache lookup failed")
		return
	}
	if entry != nil {
		// Fresh or soft-expired (SWR) cache hit: body from cache, no blocking upstream.
		metrics.MarkHit(ctx)
		if entry.SoftExpired {
			h.swrRefreshAsync(ref)
		}
		h.serveFromCache(w, r, ref, entry, ct)
		return
	}

	// Capture stale entry for serve-stale-on-upstream-failure.
	var staleEntry *artifact.CacheEntry
	if sm, ok := h.cache.(staler); ok {
		staleEntry, _ = sm.LookupStale(ctx, ref)
	}

	// 2. Upstream required.
	ups, upsOK := h.selectUpstreams(ref.Name)
	if h.upstreamClt == nil || !upsOK {
		if staleEntry != nil {
			// Serve-stale with no upstream at all: the body came from cache.
			metrics.MarkHit(ctx)
			h.log.Warn("apt: no upstream; serving stale dists", "ref", ref)
			h.serveFromCache(w, r, ref, staleEntry, ct)
			return
		}
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	// 3. Conditional GET revalidation if we have prior ETag/Last-Modified.
	if prevMeta, hasPrev := h.getMutableUpstreamMeta(ctx, ref); hasPrev && staleEntry != nil {
		body, umeta, notModified, revalErr := h.upstreamClt.Revalidate(ctx, ref, prevMeta, ups)
		if revalErr == nil {
			if notModified {
				// 304: the upstream sent no body; the bytes we serve came from
				// cache. A hit under the bytes-origin definition.
				metrics.MarkHit(ctx)
				h.extendMutableTTL(ctx, ref, umeta)
				h.serveFromCache(w, r, ref, staleEntry, ct)
				return
			}
			// 200: new content — store, then serve the stored entry.
			defer body.Close()
			if newEntry, storeErr := h.fetchBodyAndStore(ctx, ref, body, umeta); storeErr == nil && newEntry != nil {
				// Revalidation returned a NEW body from the upstream.
				metrics.MarkMiss(ctx)
				h.serveFromCache(w, r, ref, newEntry, ct)
				return
			}
			// Fall through to a fresh fetch if the store failed.
		}
	}

	// 4. Fresh fetch.
	body, umeta, fetchErr := h.upstreamClt.Fetch(ctx, ref, ups)
	if fetchErr != nil {
		if staleEntry != nil {
			// Serve-stale on upstream failure (H1): body came from cache.
			metrics.MarkHit(ctx)
			h.log.Warn("apt: upstream failed; serving stale dists", "ref", ref, "err", fetchErr)
			h.serveFromCache(w, r, ref, staleEntry, ct)
			return
		}
		h.log.Error("apt: mutable fetch", "ref", ref, "err", fetchErr)
		if isNotFound(fetchErr) {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		writeError(w, http.StatusBadGateway, "upstream fetch failed")
		return
	}
	defer body.Close()

	// Stream the freshly-fetched body through verify-on-write into the CAS, then
	// serve the stored entry. Previously this path buffered the whole body in
	// memory and wrote it to the client directly, because with TTL=0 cache.Serve
	// re-ran Lookup and missed on content just stored; serveFromCache now serves
	// the entry it is handed, so the buffering is unnecessary. Removing it also
	// closes a fail-open hole: the old code served the buffered bytes even when
	// the store returned a VerifyError, i.e. it served content whose signature
	// check had FAILED (fix C2 requires the opposite — see the pool/ path).
	newEntry, storeErr := h.fetchBodyAndStore(ctx, ref, body, umeta)
	if storeErr != nil {
		h.log.Error("apt: store mutable dists", "ref", ref, "err", storeErr)
		writeError(w, http.StatusBadGateway, "failed to cache upstream response")
		return
	}
	// Cache miss: the body was fetched from an upstream and stored.
	metrics.MarkMiss(ctx)
	h.serveFromCache(w, r, ref, newEntry, ct)
}

// --------------------------------------------------------------------------
// Immutable pipeline (pool/)
// --------------------------------------------------------------------------

// serveImmutable implements the full immutable caching pipeline for pool/ package
// files:
//
//  1. Try the verified CAS (fast path, no upstream contact).
//  2. On cache miss: fetch → quarantine → verify-on-write → CAS promotion → serve.
//
// repo is the archive path prefix; poolDir is the unscoped pool/ directory used
// for upstream Fetch (cache keys may include the archive via ref.Name).
func (h *Handler) serveImmutable(w http.ResponseWriter, r *http.Request, repo, poolDir string, ref artifact.ArtifactRef) {
	ctx := r.Context()
	ct := contentTypeForPool(ref.Version)

	// 1. Try the verified CAS (cache hit fast path).
	entry, err := h.cache.Lookup(ctx, ref)
	if err != nil {
		h.log.Error("apt: immutable lookup", "name", ref.Name, "version", ref.Version, "err", err)
		writeError(w, http.StatusInternalServerError, "cache lookup failed")
		return
	}
	if entry != nil {
		// CAS hit: the body comes from cache, no upstream body transfer.
		metrics.MarkHit(ctx)
		h.serveFromCache(w, r, ref, entry, ct)
		return
	}

	// 2. Cache miss — upstream required.
	ups, upsOK := h.selectUpstreams(repo)
	if h.upstreamClt == nil || !upsOK {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	fetchRef := poolFetchRef(poolDir, ref.Version)

	// 3. Fetch → quarantine → verify-on-write → CAS promotion.
	entry, err = h.coalescedFetch(ctx, ref, func() (*artifact.CacheEntry, error) {
		return h.fetchAndStoreImmutable(ctx, ups, fetchRef, ref)
	})
	if err != nil {
		h.log.Error("apt: fetch immutable pool", "name", ref.Name, "version", ref.Version, "err", err)
		if errors.Is(err, cache.ErrCacheMiss) || isNotFound(err) {
			writeError(w, http.StatusNotFound, "not found")
		} else {
			writeError(w, http.StatusBadGateway, "upstream fetch failed")
		}
		return
	}
	if entry == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	// Cache miss: the pool body was fetched from an upstream and promoted to CAS.
	metrics.MarkMiss(ctx)
	h.serveFromCache(w, r, ref, entry, ct)
}

// --------------------------------------------------------------------------
// Shared fetch/store helpers
// --------------------------------------------------------------------------

// fetchAndStoreImmutable fetches an immutable pool artifact from the first
// healthy upstream, streams it through the quarantine/verify-on-write pipeline,
// and promotes it to the permanent CAS tier. fetchRef drives the upstream path;
// storeRef is the cache key (may be archive-scoped).
func (h *Handler) fetchAndStoreImmutable(ctx context.Context, ups []upstream.Upstream, fetchRef, storeRef artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	rc, umeta, err := h.upstreamClt.Fetch(ctx, fetchRef, ups)
	if err != nil {
		return nil, fmt.Errorf("upstream fetch: %w", err)
	}
	defer rc.Close()

	art, cleanup, err := cache.Quarantine(ctx, h.quarantineDir, rc, umeta)
	if err != nil {
		return nil, fmt.Errorf("quarantine: %w", err)
	}

	entry, storeErr := h.cache.Store(ctx, storeRef, art)
	if storeErr != nil {
		cleanup()
		return nil, fmt.Errorf("store: %w", storeErr)
	}
	// Store removes art.Path on success; cleanup() is a safe no-op thereafter.
	return entry, nil
}

// fetchBodyAndStore quarantines an already-opened mutable response body into
// the CAS and writes a TTL-bearing MutableEntry via h.meta (when set).
// The caller is responsible for closing the reader after this function returns.
func (h *Handler) fetchBodyAndStore(ctx context.Context, ref artifact.ArtifactRef, rc io.Reader, umeta artifact.UpstreamMeta) (*artifact.CacheEntry, error) {
	art, cleanup, err := cache.Quarantine(ctx, h.quarantineDir, rc, umeta)
	if err != nil {
		return nil, fmt.Errorf("quarantine: %w", err)
	}

	entry, storeErr := h.cache.Store(ctx, ref, art)
	if storeErr != nil {
		cleanup()
		return nil, fmt.Errorf("store: %w", storeErr)
	}

	// cache.Store writes the mutable pointer with TTLSeconds=0 (always
	// revalidate). Override it with the configured TTL so the mutable tier
	// stays fresh for h.mutableTTLSec without unnecessary upstream hits.
	if h.meta != nil {
		me := artifact.MutableEntry{
			Key:          aptMutableKey(ref),
			Protocol:     ref.Protocol,
			Digest:       art.Digest,
			ETag:         umeta.ETag,
			LastModified: umeta.LastModified,
			TTLSeconds:   h.mutableTTLSec,
			Upstream:     umeta.Upstream,
			FetchedAt:    time.Now().UTC(),
		}
		if putErr := h.meta.PutMutable(ctx, me); putErr != nil {
			// Non-fatal: mutable entry still in CAS; we just lose TTL tuning.
			h.log.Warn("apt: write mutable TTL pointer", "ref", ref, "err", putErr)
		}
	}

	return entry, nil
}

// --------------------------------------------------------------------------
// Shared serve helper
// --------------------------------------------------------------------------

// serveFromCache writes the artifact identified by ref to the HTTP response.
// entry supplies Content-Length when known. All bytes are read from h.cache.Serve
// (only verified content is returned — fix C2).
func (h *Handler) serveFromCache(w http.ResponseWriter, r *http.Request, ref artifact.ArtifactRef, entry *artifact.CacheEntry, ct string) {
	ctx := r.Context()
	rc, cacheEntry, err := h.serveBytes(ctx, ref, entry)
	if err != nil {
		if errors.Is(err, cache.ErrCacheMiss) {
			writeError(w, http.StatusNotFound, "artifact not in cache")
		} else {
			h.log.Error("apt: serve from cache", "ref", ref, "err", err)
			writeError(w, http.StatusInternalServerError, "cache serve failed")
		}
		return
	}
	if rc == nil {
		writeError(w, http.StatusNotFound, "artifact not in cache")
		return
	}
	defer rc.Close()

	// Prefer size from the CacheEntry returned by Serve (post-lookup),
	// falling back to the entry supplied by the caller (pre-lookup).
	var size int64
	if cacheEntry != nil && cacheEntry.Size > 0 {
		size = cacheEntry.Size
	} else if entry != nil && entry.Size > 0 {
		size = entry.Size
	}

	w.Header().Set("Content-Type", ct)
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	w.WriteHeader(http.StatusOK)

	if r.Method == http.MethodGet {
		_, _ = io.Copy(w, rc)
	}
}

// serveBuffered has been removed: it existed only to work around cache.Serve
// re-running Lookup (and thus missing on TTL=0 content that had just been
// stored). serveFromCache now serves the entry it is passed via ServeEntry, so
// mutable responses stream from the CAS like every other tier.

// --------------------------------------------------------------------------
// Mutable revalidation helpers
// --------------------------------------------------------------------------

// swrRefreshAsync kicks a coalesced background revalidation for an XFetch
// soft-expired hit (RFC 5861 stale-while-revalidate).
func (h *Handler) swrRefreshAsync(ref artifact.ArtifactRef) {
	ups, upsOK := h.selectUpstreams(ref.Name)
	if h.upstreamClt == nil || !upsOK {
		return
	}
	key := coalesce.FetchKey(ref.Protocol, ref.Name, ref.Version, ref.Digest) + "|swr"
	cache.StartBackgroundRefresh(key, func(ctx context.Context) error {
		if prevMeta, hasPrev := h.getMutableUpstreamMeta(ctx, ref); hasPrev {
			body, umeta, notModified, err := h.upstreamClt.Revalidate(ctx, ref, prevMeta, ups)
			if err != nil {
				return err
			}
			if notModified {
				h.extendMutableTTL(ctx, ref, umeta)
				return nil
			}
			defer body.Close()
			_, err = h.fetchBodyAndStore(ctx, ref, body, umeta)
			return err
		}
		body, umeta, err := h.upstreamClt.Fetch(ctx, ref, ups)
		if err != nil {
			return err
		}
		defer body.Close()
		_, err = h.fetchBodyAndStore(ctx, ref, body, umeta)
		return err
	})
}

// getMutableUpstreamMeta returns the upstream ETag/LastModified from the
// MetadataStore for conditional GET revalidation. Returns (zero, false) when
// h.meta is nil, the entry is absent, or there is no revalidation state.
func (h *Handler) getMutableUpstreamMeta(ctx context.Context, ref artifact.ArtifactRef) (artifact.UpstreamMeta, bool) {
	if h.meta == nil {
		return artifact.UpstreamMeta{}, false
	}
	key := aptMutableKey(ref)
	me, err := h.meta.GetMutable(ctx, key)
	if err != nil || me == nil || (me.ETag == "" && me.LastModified == "") {
		return artifact.UpstreamMeta{}, false
	}
	return artifact.UpstreamMeta{
		ETag:         me.ETag,
		LastModified: me.LastModified,
		Upstream:     me.Upstream,
	}, true
}

// extendMutableTTL updates the FetchedAt timestamp after a 304 Not Modified
// response, renewing the TTL without a new blob transfer.
func (h *Handler) extendMutableTTL(ctx context.Context, ref artifact.ArtifactRef, umeta artifact.UpstreamMeta) {
	if h.meta == nil {
		return
	}
	key := aptMutableKey(ref)
	me, err := h.meta.GetMutable(ctx, key)
	if err != nil || me == nil {
		return
	}
	me.FetchedAt = time.Now().UTC()
	if umeta.ETag != "" {
		me.ETag = umeta.ETag
	}
	if putErr := h.meta.PutMutable(ctx, *me); putErr != nil {
		h.log.Warn("apt: extend mutable TTL", "ref", ref, "err", putErr)
	}
}

// --------------------------------------------------------------------------
// Misc helpers
// --------------------------------------------------------------------------

// isNotFound returns true for errors that look like a 404 from the upstream.
// Used to map upstream 404s to handler 404s without depending on a specific
// error type from the upstream package.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if upstream.IsNotFound(err) {
		return true
	}
	return strings.Contains(err.Error(), "HTTP 404")
}

// serveBytes returns a reader for the artifact's bytes plus the entry they
// belong to. When the caller already holds an entry — including a STALE one
// obtained from LookupStale — the bytes are served directly from it via
// ServeEntry. Re-resolving through cache.Serve would re-run the freshness gate
// and return ErrCacheMiss for stale mutable content, 404-ing the very content
// the serve-stale path just decided to serve (DESIGN-REVIEW fix H1).
func (h *Handler) serveBytes(ctx context.Context, ref artifact.ArtifactRef, entry *artifact.CacheEntry) (io.ReadCloser, *artifact.CacheEntry, error) {
	if es, ok := h.cache.(entryServer); ok && entry != nil {
		rc, err := es.ServeEntry(ctx, entry, 0, -1)
		return rc, entry, err
	}
	return h.cache.Serve(ctx, ref, 0, -1)
}

// coalescedFetch runs fn under the cold-fetch single-flight so concurrent
// callers for the SAME request identity share one upstream round trip
// (ARCHITECTURE §7). See coalesce.FetchLocked for cross-replica semantics.
func (h *Handler) coalescedFetch(
	ctx context.Context,
	ref artifact.ArtifactRef,
	fn func() (*artifact.CacheEntry, error),
) (*artifact.CacheEntry, error) {
	key := coalesce.FetchKey(ref.Protocol, ref.Name, ref.Version, ref.Digest)
	return coalesce.FetchLocked(ctx, h.fetchSF, h.locker, key, 0,
		func(ctx context.Context) (*artifact.CacheEntry, bool, error) {
			e, err := h.cache.Lookup(ctx, ref)
			if err != nil {
				return nil, false, err
			}
			if e != nil {
				return e, true, nil
			}
			return nil, false, nil
		},
		fn,
	)
}
