package gomod

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
	"github.com/ivanzzeth/specula/internal/metrics"
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

// contentTypeForFile returns the MIME type for a GOPROXY @v file component.
func contentTypeForFile(file string) string {
	switch {
	case strings.HasSuffix(file, extInfo):
		return "application/json"
	case strings.HasSuffix(file, extMod):
		return "text/plain; charset=utf-8"
	case strings.HasSuffix(file, extZip):
		return "application/zip"
	default:
		return "application/octet-stream"
	}
}

// gomodMutableKey returns the MetadataStore key for a mutable gomod ArtifactRef.
// Mirrors cache.mutableKey (unexported): protocol + ":" + name + ":" + version.
func gomodMutableKey(ref artifact.ArtifactRef) string {
	return ref.Protocol + ":" + ref.Name + ":" + ref.Version
}

// --------------------------------------------------------------------------
// Mutable endpoints — @v/list and @latest
// --------------------------------------------------------------------------

// serveList handles GET/HEAD /{module}/@v/list (mutable, short TTL).
// Body: newline-separated list of known versions (text/plain).
func (h *Handler) serveList(w http.ResponseWriter, r *http.Request, escMod string) {
	if !allowGetHead(w, r) {
		return
	}
	if _, err := canonicalModule(escMod); err != nil {
		writeGoError(w, http.StatusBadRequest, "invalid module path: "+err.Error())
		return
	}
	ref := listRef(escMod)
	h.serveMutable(w, r, ref, "text/plain; charset=utf-8")
}

// serveLatest handles GET/HEAD /{module}/@latest (mutable, short TTL).
// Body: JSON {"Version":..,"Time":..} for the latest known version.
func (h *Handler) serveLatest(w http.ResponseWriter, r *http.Request, escMod string) {
	if !allowGetHead(w, r) {
		return
	}
	if _, err := canonicalModule(escMod); err != nil {
		writeGoError(w, http.StatusBadRequest, "invalid module path: "+err.Error())
		return
	}
	ref := latestRef(escMod)
	h.serveMutable(w, r, ref, "application/json")
}

// serveMutable is the shared pipeline for mutable endpoints (@v/list, @latest):
//
//  1. Check the short-TTL cache (fresh entries only).
//  2. On expiry: attempt a conditional GET (If-None-Match / If-Modified-Since).
//     304 → extend TTL, serve stale.  200 → store new content.
//  3. On complete cache miss: fresh fetch → quarantine → store → serve.
//  4. If the upstream is unreachable and stale content exists, serve stale.
func (h *Handler) serveMutable(w http.ResponseWriter, r *http.Request, ref artifact.ArtifactRef, ct string) {
	ctx := r.Context()

	// 1. Check the mutable cache (fresh entries only; TTL enforced by Lookup).
	entry, err := h.cache.Lookup(ctx, ref)
	if err != nil {
		h.log.Error("gomod: mutable lookup", "ref", ref, "err", err)
		writeGoError(w, http.StatusInternalServerError, "cache lookup failed")
		return
	}
	if entry != nil {
		// Fresh cache hit: the body comes from cache, no upstream body transfer.
		metrics.MarkHit(ctx)
		h.serveFromCache(w, r, ref, entry, ct)
		return
	}

	// Capture a stale entry for serve-stale-on-upstream-failure if the
	// CacheManager supports it (optional, not all test fakes implement this).
	var staleEntry *artifact.CacheEntry
	if sm, ok := h.cache.(staler); ok {
		staleEntry, _ = sm.LookupStale(ctx, ref)
	}

	// 2. Upstream required for revalidation or a fresh fetch.
	if h.upstreamClt == nil || len(h.upstreams) == 0 {
		if staleEntry != nil {
			// Serve-stale with no upstream at all: the body came from cache.
			metrics.MarkHit(ctx)
			h.log.Warn("gomod: no upstream; serving stale", "ref", ref)
			h.serveFromCache(w, r, ref, staleEntry, ct)
			return
		}
		writeGoError(w, http.StatusNotFound, "not found")
		return
	}

	// 3. Conditional GET revalidation if we have prev ETag/Last-Modified.
	if prevMeta, hasPrev := h.getMutableUpstreamMeta(ctx, ref); hasPrev && staleEntry != nil {
		body, umeta, notModified, revalErr := h.upstreamClt.Revalidate(ctx, ref, prevMeta, h.upstreams)
		if revalErr == nil {
			if notModified {
				// 304: the upstream sent no body; the bytes we serve came from
				// cache. A hit under the bytes-origin definition.
				metrics.MarkHit(ctx)
				h.extendMutableTTL(ctx, ref, umeta)
				h.serveFromCache(w, r, ref, staleEntry, ct)
				return
			}
			// 200: new body — store and serve.
			defer body.Close()
			if newEntry, storeErr := h.fetchBodyAndStore(ctx, ref, body, umeta); storeErr == nil && newEntry != nil {
				// Revalidation returned a NEW body from the upstream.
				metrics.MarkMiss(ctx)
				h.serveFromCache(w, r, ref, newEntry, ct)
				return
			}
			// Fall through to fresh fetch if store failed.
		}
	}

	// 4. Fresh fetch.
	body, umeta, fetchErr := h.upstreamClt.Fetch(ctx, ref, h.upstreams)
	if fetchErr != nil {
		if staleEntry != nil {
			// Serve-stale on upstream failure (H1): body came from cache.
			metrics.MarkHit(ctx)
			h.log.Warn("gomod: upstream failed; serving stale", "ref", ref, "err", fetchErr)
			h.serveFromCache(w, r, ref, staleEntry, ct)
			return
		}
		h.log.Error("gomod: mutable fetch", "ref", ref, "err", fetchErr)
		writeGoError(w, http.StatusBadGateway, "upstream fetch failed")
		return
	}
	defer body.Close()

	newEntry, storeErr := h.fetchBodyAndStore(ctx, ref, body, umeta)
	if storeErr != nil {
		h.log.Error("gomod: store mutable", "ref", ref, "err", storeErr)
		writeGoError(w, http.StatusBadGateway, "failed to cache upstream response")
		return
	}
	// Cache miss: the body was fetched from an upstream and stored.
	metrics.MarkMiss(ctx)
	h.serveFromCache(w, r, ref, newEntry, ct)
}

// --------------------------------------------------------------------------
// Immutable endpoints — .info, .mod, .zip
// --------------------------------------------------------------------------

// serveInfo handles GET/HEAD /{module}/@v/{version}.info (immutable, CAS).
// Body: JSON {"Version":..,"Time":..}.
func (h *Handler) serveInfo(w http.ResponseWriter, r *http.Request, escMod, file string) {
	h.serveImmutable(w, r, escMod, file)
}

// serveMod handles GET/HEAD /{module}/@v/{version}.mod (immutable, CAS).
// Body: the module's go.mod file.
func (h *Handler) serveMod(w http.ResponseWriter, r *http.Request, escMod, file string) {
	h.serveImmutable(w, r, escMod, file)
}

// serveZip handles GET/HEAD /{module}/@v/{version}.zip (immutable, CAS).
// Body: module zip — also the artifact whose h1: hash is checked by sumdb.
func (h *Handler) serveZip(w http.ResponseWriter, r *http.Request, escMod, file string) {
	h.serveImmutable(w, r, escMod, file)
}

// serveImmutable is the shared pipeline for .info/.mod/.zip endpoints:
//
//  1. Validate method, module path, and version file component.
//  2. Try to serve from the verified CAS (fast path, no upstream contact).
//  3. On cache miss: fetch → quarantine → verify-on-write → promote → serve.
func (h *Handler) serveImmutable(w http.ResponseWriter, r *http.Request, escMod, file string) {
	if !allowGetHead(w, r) {
		return
	}
	if _, err := canonicalModule(escMod); err != nil {
		writeGoError(w, http.StatusBadRequest, "invalid module path: "+err.Error())
		return
	}
	if _, ok := versionFromFile(file); !ok {
		writeGoError(w, http.StatusNotFound, "unknown @v file: "+file)
		return
	}

	ref := immutableRef(escMod, file)
	ct := contentTypeForFile(file)
	ctx := r.Context()

	// 1. Try the verified CAS (cache hit fast path).
	entry, err := h.cache.Lookup(ctx, ref)
	if err != nil {
		h.log.Error("gomod: immutable lookup", "module", escMod, "file", file, "err", err)
		writeGoError(w, http.StatusInternalServerError, "cache lookup failed")
		return
	}
	if entry != nil {
		// CAS hit: the body comes from cache, no upstream body transfer.
		metrics.MarkHit(ctx)
		h.serveFromCache(w, r, ref, entry, ct)
		return
	}

	// 2. Cache miss — upstream required.
	if h.upstreamClt == nil || len(h.upstreams) == 0 {
		writeGoError(w, http.StatusNotFound, "not found")
		return
	}

	// 3. Fetch → quarantine → verify-on-write → CAS promotion.
	entry, err = h.fetchAndStoreImmutable(ctx, ref)
	if err != nil {
		h.log.Error("gomod: fetch immutable", "module", escMod, "file", file, "err", err)
		writeGoError(w, http.StatusBadGateway, "upstream fetch failed")
		return
	}
	if entry == nil {
		writeGoError(w, http.StatusNotFound, "not found")
		return
	}

	// Cache miss: the body was fetched from an upstream and promoted to CAS.
	metrics.MarkMiss(ctx)
	h.serveFromCache(w, r, ref, entry, ct)
}

// --------------------------------------------------------------------------
// Shared fetch/store helpers
// --------------------------------------------------------------------------

// fetchAndStoreImmutable fetches an immutable artifact (one of .info/.mod/.zip)
// from the first healthy upstream, streams it through the quarantine /
// verify-on-write pipeline, and promotes it to the permanent CAS tier.
func (h *Handler) fetchAndStoreImmutable(ctx context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	rc, umeta, err := h.upstreamClt.Fetch(ctx, ref, h.upstreams)
	if err != nil {
		return nil, fmt.Errorf("upstream fetch: %w", err)
	}
	defer rc.Close()

	art, cleanup, err := cache.Quarantine(ctx, h.quarantineDir, rc, umeta)
	if err != nil {
		return nil, fmt.Errorf("quarantine: %w", err)
	}

	entry, storeErr := h.cache.Store(ctx, ref, art)
	if storeErr != nil {
		cleanup()
		return nil, fmt.Errorf("store: %w", storeErr)
	}
	// Store removes art.Path on success; cleanup() is a safe no-op from here.

	return entry, nil
}

// fetchBodyAndStore quarantines an already-opened mutable response body into
// the CAS and writes a proper TTL-bearing MutableEntry via h.meta (when set).
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
	// revalidate). Override it with the configured short TTL so the mutable
	// tier stays fresh for h.mutableTTLSec without unnecessary upstream hits.
	if h.meta != nil {
		me := artifact.MutableEntry{
			Key:          gomodMutableKey(ref),
			Protocol:     ref.Protocol,
			Digest:       art.Digest,
			ETag:         umeta.ETag,
			LastModified: umeta.LastModified,
			TTLSeconds:   h.mutableTTLSec,
			Upstream:     umeta.Upstream,
			FetchedAt:    time.Now().UTC(),
		}
		if putErr := h.meta.PutMutable(ctx, me); putErr != nil {
			// Non-fatal: mutable entry is still in CAS; we just lose TTL tuning.
			h.log.Warn("gomod: write mutable TTL pointer", "ref", ref, "err", putErr)
		}
	}

	return entry, nil
}

// --------------------------------------------------------------------------
// Shared serve helper
// --------------------------------------------------------------------------

// serveFromCache writes the artifact identified by ref to the HTTP response.
// entry is used to supply Content-Length when known (may be nil if unavailable).
// The actual bytes are read from h.cache.Serve (only verified content is returned).
func (h *Handler) serveFromCache(w http.ResponseWriter, r *http.Request, ref artifact.ArtifactRef, entry *artifact.CacheEntry, ct string) {
	ctx := r.Context()
	rc, cacheEntry, err := h.serveBytes(ctx, ref, entry)
	if err != nil {
		if errors.Is(err, cache.ErrCacheMiss) {
			writeGoError(w, http.StatusNotFound, "artifact not in cache")
		} else {
			h.log.Error("gomod: serve from cache", "ref", ref, "err", err)
			writeGoError(w, http.StatusInternalServerError, "cache serve failed")
		}
		return
	}
	if rc == nil {
		writeGoError(w, http.StatusNotFound, "artifact not in cache")
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

// --------------------------------------------------------------------------
// Mutable revalidation helpers
// --------------------------------------------------------------------------

// getMutableUpstreamMeta returns the upstream ETag/LastModified from the
// MetadataStore for conditional GET revalidation. Returns (zero, false) when
// h.meta is nil, the entry is absent, or there is no revalidation state.
func (h *Handler) getMutableUpstreamMeta(ctx context.Context, ref artifact.ArtifactRef) (artifact.UpstreamMeta, bool) {
	if h.meta == nil {
		return artifact.UpstreamMeta{}, false
	}
	key := gomodMutableKey(ref)
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

// extendMutableTTL updates the FetchedAt timestamp in the MetadataStore after
// a 304 Not Modified response, renewing the TTL without a new blob transfer.
func (h *Handler) extendMutableTTL(ctx context.Context, ref artifact.ArtifactRef, umeta artifact.UpstreamMeta) {
	if h.meta == nil {
		return
	}
	key := gomodMutableKey(ref)
	me, err := h.meta.GetMutable(ctx, key)
	if err != nil || me == nil {
		return
	}
	me.FetchedAt = time.Now().UTC()
	if umeta.ETag != "" {
		me.ETag = umeta.ETag
	}
	if putErr := h.meta.PutMutable(ctx, *me); putErr != nil {
		h.log.Warn("gomod: extend mutable TTL", "ref", ref, "err", putErr)
	}
}

// --------------------------------------------------------------------------
// Method guard
// --------------------------------------------------------------------------

// allowGetHead enforces GET/HEAD-only semantics, writing 405 otherwise.
func allowGetHead(w http.ResponseWriter, r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return true
	default:
		w.Header().Set("Allow", "GET, HEAD")
		writeGoError(w, http.StatusMethodNotAllowed, "method not allowed")
		return false
	}
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
